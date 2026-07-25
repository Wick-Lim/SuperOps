package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	goredis "github.com/redis/go-redis/v9"
	"github.com/rs/cors"
	"golang.org/x/crypto/bcrypt"

	"github.com/Wick-Lim/SuperOps/backend/internal/admin"
	"github.com/Wick-Lim/SuperOps/backend/internal/audit"
	"github.com/Wick-Lim/SuperOps/backend/internal/auth"
	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/block"
	"github.com/Wick-Lim/SuperOps/backend/internal/channel"
	"github.com/Wick-Lim/SuperOps/backend/internal/collab"
	"github.com/Wick-Lim/SuperOps/backend/internal/comment"
	"github.com/Wick-Lim/SuperOps/backend/internal/drive"
	"github.com/Wick-Lim/SuperOps/backend/internal/drive/registry"
	"github.com/Wick-Lim/SuperOps/backend/internal/emoji"
	"github.com/Wick-Lim/SuperOps/backend/internal/file"
	"github.com/Wick-Lim/SuperOps/backend/internal/inbox"
	"github.com/Wick-Lim/SuperOps/backend/internal/issue"
	"github.com/Wick-Lim/SuperOps/backend/internal/mail"
	"github.com/Wick-Lim/SuperOps/backend/internal/message"
	"github.com/Wick-Lim/SuperOps/backend/internal/presence"
	"github.com/Wick-Lim/SuperOps/backend/internal/ratelimit"
	"github.com/Wick-Lim/SuperOps/backend/internal/rbac"
	"github.com/Wick-Lim/SuperOps/backend/internal/search"
	"github.com/Wick-Lim/SuperOps/backend/internal/sso"
	"github.com/Wick-Lim/SuperOps/backend/internal/storage"
	"github.com/Wick-Lim/SuperOps/backend/internal/user"
	"github.com/Wick-Lim/SuperOps/backend/internal/webhook"
	"github.com/Wick-Lim/SuperOps/backend/internal/workspace"
	"github.com/Wick-Lim/SuperOps/backend/internal/ws"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
	redispkg "github.com/Wick-Lim/SuperOps/backend/pkg/redis"
)

type App struct {
	Config *Config
	Logger *slog.Logger
	DB     *pgxpool.Pool
	Redis  *goredis.Client
	NATS   *natspkg.Client
	Hub    *ws.Hub
	Server *http.Server

	// Authz is exported so an operational tool can run the object-permission
	// backfill (authz.Checker.Rebuild) or the drift verifier against a live
	// pool. Handlers never reach it through here; they are given the checker
	// directly.
	Authz *authz.Checker

	// Storage is the object-storage backend, or nil when none is configured.
	// Exported for the same reason Authz is: a test or an operational tool has
	// to be able to look at the bucket, and handlers are given it directly.
	Storage storage.Backend

	// Drive is exported so a test can drive the same search.BodySource the
	// worker uses. Without it a test would have to restate what the worker
	// reads, which is how an indexer and its test agree with each other and
	// with nothing else.
	Drive *drive.Repository

	// audit is closed on shutdown so the Tier 2 buffer drains instead of
	// discarding the records for the last requests this replica served.
	audit *audit.Service

	// draining flips as soon as shutdown starts so /ready fails before the
	// listener stops accepting: a load balancer needs a failing readiness probe
	// to stop routing to this replica, and it only learns that by polling.
	draining atomic.Bool
}

func New(ctx context.Context, cfg *Config, logger *slog.Logger) (*App, error) {
	// Infrastructure
	pool, err := database.NewPool(ctx, database.Config{
		DSN:                      cfg.DB.DSN(),
		MaxConns:                 cfg.DB.MaxConns,
		MinConns:                 cfg.DB.MinConns,
		StatementTimeout:         cfg.DB.StatementTimeout,
		LockTimeout:              cfg.DB.LockTimeout,
		IdleInTransactionTimeout: cfg.DB.IdleInTransactionTimeout,
	}, logger)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}

	redisClient, err := redispkg.NewClient(ctx, redispkg.Config{
		Addr:         cfg.Redis.Addr,
		Password:     cfg.Redis.Password,
		DB:           cfg.Redis.DB,
		DialTimeout:  cfg.Redis.DialTimeout,
		ReadTimeout:  cfg.Redis.ReadTimeout,
		WriteTimeout: cfg.Redis.WriteTimeout,
		PoolSize:     cfg.Redis.PoolSize,
	}, logger)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("redis: %w", err)
	}

	natsClient, err := natspkg.NewClient(natspkg.Config{
		URL:          cfg.NATS.URL,
		DrainTimeout: cfg.NATS.DrainTimeout,
	}, logger)
	if err != nil {
		redisClient.Close()
		pool.Close()
		return nil, fmt.Errorf("nats: %w", err)
	}

	// The API publishes every message/reaction event with PublishDurable, which
	// blocks on a JetStream storage ack. If only the worker created the stream,
	// an API booted without one burns the full publish timeout on every single
	// event. Both processes call this now.
	if err := EnsureEventStream(ctx, natsClient, logger); err != nil {
		// Non-fatal: core subscribers (the ws relay) still see the events.
		logger.Warn("JetStream stream unavailable; durable publishes will fail", "error", err)
	}

	// Repositories
	userRepo := user.NewRepository(pool)
	authRepo := auth.NewRepository(pool)
	workspaceRepo := workspace.NewRepository(pool)
	channelRepo := channel.NewRepository(pool)
	messageRepo := message.NewRepository(pool)
	inboxRepo := inbox.NewRepository(pool)

	// WebSocket Hub with NATS bridge for multi-replica support.
	// - NATS bridge: relays client-originated ephemeral events (typing) between replicas.
	// - Event relay: fans application domain events (message/reaction/notification) to local clients.
	// - Room bridge: fans collaboration updates and revocations between replicas,
	//   so two editors on different pods see each other's keystrokes.
	//
	// It is built before authz.Checker because the checker revokes through it:
	// see liveRevoker below.
	hub := ws.NewHub(logger)
	hub.StartNATSBridge(natsClient.Conn, logger)
	hub.StartEventRelay(natsClient.Conn, logger)
	hub.StartRoomBridge(natsClient.Conn, logger)
	go hub.Run()

	// authz.Checker is the single source of truth for authorization decisions:
	// handlers ask it for a capability on an object (docs/plans/00-permissions.md).
	//
	// The revoker is the live half of that. Deleting a grant settles every
	// future request and settles nothing about a WebSocket subscription or an
	// editing session that was authorized before it — so Revoke and Move drive
	// hub.RevokeChannelSubscription and collab.Service.RevokeAccess through this
	// seam. collabRevoker is filled in a few lines below, once the collaboration
	// service exists; it cannot be passed here because that service needs the
	// checker this call is constructing.
	revoker := &liveRevoker{hub: hub}
	// az is built before auditService because the checker is passed to almost
	// everything; the auditor is attached below, once it exists, through the
	// same option list. See authz.WithAuditor for why the hooks live inside
	// Grant/Revoke/Move rather than at their call sites.
	az := authz.New(pool, authz.WithRevoker(revoker))

	// audit must exist before auth: auth.Service records login/logout/password
	// events through it.
	//
	// The sink is constructed here and a failure is FATAL. That is the §3c rule
	// (docs/ROADMAP.md): a transport named without its credentials must be a boot
	// failure, not a first-use failure — an operator who sets AUDIT_SINK=http and
	// forgets the secret has to find out at deploy time, not the first time a
	// chain needed anchoring.
	auditSink, err := audit.NewSink(audit.SinkConfig{
		Transport: cfg.Audit.Sink,
		Path:      cfg.Audit.SinkPath,
		Endpoint:  cfg.Audit.SinkEndpoint,
		Secret:    cfg.Audit.SinkSecret,
		Timeout:   cfg.Audit.SinkTimeout,
		Logger:    logger,
	})
	if err != nil {
		natsClient.Close()
		redisClient.Close()
		pool.Close()
		return nil, err
	}
	auditService := audit.NewService(pool, logger)
	// Tier 2: egress and sensitive reads, off the request's critical path. Tier 1
	// (authentication, authorization changes, sharing, configuration) never
	// touches this queue — see the package comment.
	auditService.StartBuffer(audit.BufferConfig{
		Size:    cfg.Audit.BufferSize,
		Workers: cfg.Audit.BufferWorkers,
	})
	// Authorization changes are audited from INSIDE authz.Grant/Revoke/Move, so a
	// pillar cannot forget a hook it never had to write.
	authz.WithAuditor(auditService)(az)
	logger.Info("audit configured", "sink", auditSink.Name(),
		"retention_days", cfg.Audit.RetentionDays,
		"note", "the hash chain detects tampering; only anchored_seq is protected off-box")

	// Services
	jwtMgr := auth.NewJWTManager(cfg.JWT.Secret, cfg.JWT.AccessTokenTTL)
	// WithAuthz is what lets password login, refresh and accept-invite consult
	// SSO enforcement. Without it they behave exactly as they did before, which
	// is correct for a deployment that has no SSO provider anywhere.
	authService := auth.NewService(authRepo, userRepo, pool, jwtMgr, cfg.JWT.RefreshTokenTTL, auditService,
		auth.WithAuthz(az))
	presenceService := presence.NewService(redisClient)

	// Search
	var searchService *search.Service
	if cfg.Meili.IsEnabled() {
		searchService, err = search.NewService(cfg.Meili.Host, cfg.Meili.MasterKey, logger)
		if err != nil {
			logger.Warn("Meilisearch not available, search disabled", "error", err)
			searchService = nil
		}
	} else {
		logger.Info("search disabled by configuration")
	}

	// Object storage — a deployment-dependent capability (ROADMAP §3c).
	//
	// A CONFIGURATION error is fatal: STORAGE_BACKEND=s4, or s3 with no region,
	// is a typo the operator fixes in a minute, and booting past it means
	// discovering it as a 500 on somebody's first upload.
	//
	// A REACHABILITY error is not. The API has always booted without object
	// storage and simply not registered the file routes, and compose starts
	// every service at once — making an unreachable MinIO fatal would turn a
	// startup race into a crash loop. It is logged at Error, the routes are
	// absent rather than broken, and POST /admin/storage/test re-runs the same
	// check on demand once the operator has something to fix.
	var fileStorage storage.Backend
	if cfg.MinIO.IsEnabled() {
		storageCfg := storage.Config{
			Backend:      cfg.MinIO.Backend,
			Endpoint:     cfg.MinIO.Endpoint,
			AccessKey:    cfg.MinIO.AccessKey,
			SecretKey:    cfg.MinIO.SecretKey,
			Bucket:       cfg.MinIO.Bucket,
			UseSSL:       cfg.MinIO.UseSSL,
			Region:       cfg.MinIO.Region,
			PathStyle:    cfg.MinIO.PathStyle,
			CreateBucket: cfg.MinIO.CreateBucket,
		}
		if err := storageCfg.Normalize(); err != nil {
			natsClient.Close()
			pool.Close()
			return nil, err
		}
		fileStorage, err = storage.Open(ctx, storageCfg, logger)
		if err != nil {
			logger.Error("object storage unavailable; file uploads and Drive are disabled",
				"backend", storageCfg.Backend, "endpoint", storageCfg.Endpoint, "error", err)
			fileStorage = nil
		}
	} else {
		logger.Info("file uploads disabled by configuration")
	}

	// Single sign-on. Disabled without SSO_SECRET_KEY: a per-workspace OIDC
	// client secret has to be sealed at rest, and without a key there is
	// nothing to seal it with — so the feature refuses to construct rather than
	// storing tenant credentials in the clear.
	var ssoHandler *sso.Handler
	if cfg.SSO.IsEnabled() {
		key, err := sso.ParseSecretKey(cfg.SSO.SecretKey)
		if err != nil {
			// Already validated in LoadConfig; belt and braces, because App.New
			// is also reachable from tests that build a Config by hand.
			natsClient.Close()
			redisClient.Close()
			pool.Close()
			return nil, fmt.Errorf("sso: %w", err)
		}
		ssoCfg := sso.Config{
			SecretKey:           key,
			AllowInsecureIssuer: cfg.SSO.AllowInsecureIssuer,
			HTTPTimeout:         cfg.SSO.HTTPTimeout,
			AuthRequestTTL:      cfg.SSO.AuthRequestTTL,
			PendingTTL:          cfg.SSO.PendingTTL,
			JWKSCacheTTL:        cfg.SSO.JWKSCacheTTL,
			ClockSkew:           cfg.SSO.ClockSkew,
		}
		ssoRepo := sso.NewRepository(pool)
		ssoService := sso.NewService(ssoRepo, sso.NewClient(ssoCfg), authService, auditService, ssoCfg, logger)
		ssoHandler = sso.NewHandler(ssoService, az)
		logger.Info("single sign-on enabled", "allow_insecure_issuer", cfg.SSO.AllowInsecureIssuer)
		if cfg.SSO.AllowInsecureIssuer {
			logger.Warn("SSO_ALLOW_INSECURE_ISSUER is on: a workspace admin can point an OIDC issuer at " +
				"an http:// URL or a private/link-local address and have this process fetch it. " +
				"Local test providers only")
		}
	} else {
		logger.Info("single sign-on disabled: SSO_SECRET_KEY is not set")
	}

	// Collaborative editing. No configuration: it needs Postgres and, for more
	// than one replica, the NATS connection that already exists. The service is
	// both the HTTP handler's backend and the WebSocket room handler, which is
	// why it is built before wsHandler.
	collabRepo := collab.NewRepository(pool)
	collabSvc := collab.NewService(
		collabRepo,
		collab.NewWorkspaceAuthorizer(az),
		hub,
		logger,
	)
	collabHandler := collab.NewHandler(collabSvc)

	// The editor registry. Register is called HERE, not from init(), so an
	// editor that is not configured in a deployment is not discoverable in it —
	// see internal/drive/registry.
	//
	// `document` is the block editor. Adding the next one — a spreadsheet, a
	// design surface — is one more Register line here and one client component;
	// no table, no route, no migration. That is what the registry is for, and
	// the projection field is what makes it true for the searchable body too.
	driveKinds := registry.New()
	for _, kind := range []registry.Kind{
		drive.DocumentKind(collabRepo),
		drive.SpreadsheetKind(collabRepo),
		drive.DesignKind(collabRepo),
	} {
		if err := driveKinds.Register(kind); err != nil {
			natsClient.Close()
			pool.Close()
			return nil, fmt.Errorf("register drive kind %q: %w", kind.Type, err)
		}
	}
	driveRepo := drive.NewRepository(pool, az, driveKinds)
	driveHandler := drive.NewHandler(pool, az, driveKinds, fileStorage, collabRepo, auditService,
		drive.NewPublisher(natsClient, logger))

	// The comment surface. ONE per product: plan 02 §13 cut per-file comments
	// so that Phase 2 could build this once and Drive adopt it, and a second
	// comment system is the failure the all-in-one thesis is written against.
	//
	// It is given a Titler rather than knowing what an issue or a file is —
	// "somebody commented on 5d3f…" is a notification nobody can act on, and the
	// alternative is this package importing every pillar.
	commentHandler := comment.NewHandler(pool, az,
		comment.NewNATSNotifier(natsClient), comment.NewTitler(pool), auditService)

	// Work tracking. `project` and `issue` are ACL-NATIVE, so their acl_object
	// rows are written by RegisterTx in the same transaction as the row they
	// describe — internal/authz's derivedTypes is unchanged and migration 031
	// adds no arm to either expected-state view.
	issueHandler := issue.NewHandler(pool, az, comment.NewNATSNotifier(natsClient), auditService)
	// Closes the cycle: the checker revokes document sessions through the
	// service, and the service authorizes through the checker.
	revoker.collab.Store(collabSvc)
	// Drive revokes editor sessions directly rather than through authz's
	// liveTypes fan-out — see collab.Service.RevokeFileAccess.
	driveHandler.SetCollabRevoker(collabSvc)

	// Handlers
	authHandler := auth.NewHandler(authService)
	userHandler := user.NewHandler(userRepo)
	workspaceHandler := workspace.NewHandler(workspaceRepo, az, hub)
	// The hub is what makes channel lifecycle events reach connected clients and
	// what revokes a subscription the roster no longer authorizes; natsClient
	// carries channel.member_added to the worker's channel-invite notifier.
	channelHandler := channel.NewHandler(channelRepo, az, hub, natsClient, logger)
	messageHandler := message.NewHandler(messageRepo, az, natsClient, logger)
	wsHandler := ws.NewWSHandler(
		hub,
		jwtMgr,
		presenceService,
		// MemberChecker: a DB error must stay distinct from "not a member", or a
		// transient blip revokes every subscription at once.
		//
		// CapWrite, because that is the rung channel membership maps to. A
		// subscription is the live feed of a channel's traffic and is gated on
		// the membership row, not on public-channel readability: asking for
		// CapRead here would subscribe every workspace member to every public
		// channel they never joined.
		func(ctx context.Context, channelID, userID string) (bool, error) {
			return az.Can(ctx, authz.UserSubject(userID), authz.ChannelObject(channelID), authz.CapWrite)
		},
		// WorkspaceLister: resolved once per connection, used to route
		// workspace-scoped events (channel.created, presence.changed).
		func(ctx context.Context, userID string) ([]string, error) {
			rows, err := pool.Query(ctx,
				`SELECT workspace_id FROM workspace_members WHERE user_id = $1`, userID)
			if err != nil {
				return nil, fmt.Errorf("list user workspaces: %w", err)
			}
			defer rows.Close()
			ids := []string{}
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return nil, fmt.Errorf("scan user workspace: %w", err)
				}
				ids = append(ids, id)
			}
			return ids, rows.Err()
		},
		cfg.CORS.AllowedOrigins,
		logger,
		// Attaches the collaboration layer to the existing socket. Without it
		// every collab.* frame is answered UNSUPPORTED.
		ws.WithRoomHandler(collabSvc),
	)
	presenceHandler := presence.NewHandler(presenceService, az, pool)
	var fileHandler *file.Handler
	if fileStorage != nil {
		fileHandler = file.NewHandler(fileStorage, pool, az, auditService)
	}
	// The inbox owns notifications for the whole product (docs/plans/README.md
	// "Resolved conflicts" §1). The API only READS it — every write happens in
	// cmd/worker behind a durable consumer — so no Notifier is built here.
	inboxHandler := inbox.NewHandler(inboxRepo)

	// Outbound mail. Both the sender and the renderer are constructed here, and
	// a failure is fatal: the whole point of the configuration validation is
	// that a mail misconfiguration must be a boot failure and not a user who
	// silently never receives their invitation.
	//
	// The API holds a Sender as well as a Publisher even though it never sends
	// invitations itself: the admin configuration test is synchronous, because
	// an operator verifying their settings needs the transport's real error.
	mailSender, err := NewMailSender(cfg, logger)
	if err != nil {
		natsClient.Close()
		redisClient.Close()
		pool.Close()
		return nil, fmt.Errorf("mail: %w", err)
	}
	mailRenderer, err := NewMailRenderer(cfg)
	if err != nil {
		natsClient.Close()
		redisClient.Close()
		pool.Close()
		return nil, fmt.Errorf("mail: %w", err)
	}
	logger.Info("outbound mail configured",
		"transport", mailSender.Name(),
		"from", cfg.Mail.From,
		"public_base_url", mailRenderer.BaseURL())
	if cfg.Mail.Transport == mail.TransportLog {
		logger.Warn("MAIL_TRANSPORT is 'log': invitations are written to this log instead of being delivered. " +
			"Set MAIL_TRANSPORT (smtp / smtp-direct / resend) to send real email")
	}

	adminHandler := admin.NewHandler(pool, auditService, az, admin.MailDeps{
		Publisher: mail.NewPublisher(natsClient, logger),
		Renderer:  mailRenderer,
		Sender:    mailSender,
		Secrets:   []string{cfg.Mail.SMTP.Password, cfg.Mail.Resend.APIKey},
		Logger:    logger,
	}, admin.StorageDeps{
		// nil when storage failed to open, which is what makes
		// POST /admin/storage/test answer 503 ("not configured") rather than
		// 502 ("configured and broken"). The operator needs to tell those apart.
		Backend: storageValidator(fileStorage),
		Secrets: []string{cfg.MinIO.SecretKey, cfg.MinIO.AccessKey},
	})
	var searchHandler *search.Handler
	if searchService != nil {
		searchHandler = search.NewHandler(searchService, az)
	}

	appInstance := &App{
		Config:  cfg,
		Logger:  logger,
		Storage: fileStorage,
		Drive:   driveRepo,
		DB:      pool,
		Redis:   redisClient,
		NATS:    natsClient,
		Hub:     hub,
		Authz:   az,
		audit:   auditService,
	}

	// Router
	mux := http.NewServeMux()

	// Liveness vs readiness are deliberately different checks.
	//
	// /health touches nothing. It is the liveness (and startup) probe: the only
	// correct answer to "should the kubelet kill this container?" is no, unless
	// the process itself is wedged. Wiring dependency checks into liveness meant
	// a 30s Postgres failover failed liveness on every replica simultaneously
	// and escalated a blip into a cluster-wide CrashLoopBackOff.
	//
	// /ready reports the dependencies. A failing readiness probe removes the
	// replica from the Service endpoints and nothing else, which is the right
	// blast radius for "my database is briefly gone".
	mux.HandleFunc("GET /health", liveHandler)
	mux.HandleFunc("GET /ready", appInstance.readyHandler())
	mux.HandleFunc("GET /metrics", metricsHandler(hub, pool, auditService, cfg.MetricsToken))

	// The API limiter runs outside the mux while auth.Middleware runs inside it,
	// so authctx is always empty at limiter time. Parse the token here without
	// enforcing it — an invalid or absent token just falls back to the IP bucket.
	identifyCaller := func(r *http.Request) string {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" {
			tok = r.URL.Query().Get("token")
		}
		if tok == "" {
			return ""
		}
		claims, err := jwtMgr.Validate(tok)
		if err != nil {
			return ""
		}
		return claims.UserID
	}

	// Rate limiters (Redis-backed). A strict per-IP limit guards the
	// brute-forceable auth endpoints; a generous per-user limit guards the API.
	var loginLimiter func(http.Handler) http.Handler
	if cfg.RateLimit.Enabled {
		loginLimiter = ratelimit.MiddlewareByIP(redisClient, ratelimit.Config{
			RequestsPerMinute: cfg.RateLimit.AuthPerMinute,
			Window:            time.Minute,
			TrustProxy:        cfg.RateLimit.TrustProxy,
			TrustedProxyHops:  cfg.RateLimit.TrustedProxyHops,
		})
	}

	// Auth routes (no auth middleware; login/refresh/accept-invite are rate-limited)
	authHandler.RegisterRoutes(mux, loginLimiter)
	if ssoHandler != nil {
		// The same per-IP budget the password endpoints get: every one of these
		// either triggers an outbound request to a provider or accepts a
		// guessable single-use token.
		ssoHandler.RegisterRoutes(mux, loginLimiter)
	}

	// Auth middleware
	authMw := auth.Middleware(jwtMgr)

	// Protected routes
	authHandler.RegisterProtectedRoutes(mux, authMw)
	userHandler.RegisterRoutes(mux, authMw)
	// Device-token registration only exists when something will send to those
	// tokens. With PUSH_ENABLED off the routes are absent and the client's
	// registration attempt 404s, which app/src/lib/push.ts reads as "this
	// deployment has no push" and stops asking.
	if cfg.Push.IsEnabled() {
		userHandler.RegisterDeviceRoutes(mux, authMw)
		logger.Info("push notifications enabled; device registration available",
			"note", "delivery additionally requires APNs/FCM credentials on the Expo project")
	} else {
		logger.Info("push notifications disabled by configuration (PUSH_ENABLED)")
	}
	workspaceHandler.RegisterRoutes(mux, authMw)
	if ssoHandler != nil {
		// Per-workspace provider administration; the handler re-checks that the
		// caller administers the workspace in the path.
		ssoHandler.RegisterProtectedRoutes(mux, authMw)
	}
	channelHandler.RegisterRoutes(mux, authMw)
	collabHandler.RegisterRoutes(mux, authMw)
	driveHandler.RegisterRoutes(mux, authMw)
	commentHandler.RegisterRoutes(mux, authMw)
	issueHandler.RegisterRoutes(mux, authMw)
	// Resolving a share link is unauthenticated and its path CONTAINS the
	// secret being guessed, so it gets a FIXED rate-limit bucket rather than
	// the default per-path one — which would give every guess its own budget
	// and make the limit decorative. It shares the auth routes' strict budget,
	// because it is the same shape of endpoint: an unauthenticated credential
	// check.
	driveHandler.RegisterLinkRoutes(mux, func(next http.Handler) http.Handler {
		if !cfg.RateLimit.Enabled {
			return next
		}
		return ratelimit.MiddlewareByIPBucket(redisClient, ratelimit.Config{
			RequestsPerMinute: cfg.RateLimit.AuthPerMinute,
			Window:            time.Minute,
			TrustProxy:        cfg.RateLimit.TrustProxy,
			TrustedProxyHops:  cfg.RateLimit.TrustedProxyHops,
		}, "drive-link-resolve")(next)
	})
	presenceHandler.RegisterRoutes(mux, authMw)
	if fileHandler != nil {
		fileHandler.RegisterRoutes(mux, authMw)
	}
	inboxHandler.RegisterRoutes(mux, authMw)
	// The four shipped /api/v1/notifications routes, re-pointed at inbox_items.
	// One of them changes MEANING — unread-count now counts unread ITEMS rather
	// than unread EVENTS — deliberately and without versioning; see
	// internal/inbox/compat.go for the argument and its cost.
	inboxHandler.RegisterCompatRoutes(mux, authMw)
	// Admin endpoints require an authenticated caller who administers a
	// workspace. Each handler re-scopes to the workspaces that caller actually
	// administers, so this is a cheap pre-filter, not the whole authorization.
	adminMw := func(next http.Handler) http.Handler {
		return authMw(rbac.RequireAnyWorkspaceAdmin(az)(next))
	}
	adminHandler.RegisterRoutes(mux, adminMw)
	// The audit read surface. Same paths and the same adminMw the query had when
	// it lived in internal/admin, so nothing about the authorization changes —
	// it moved because the package that owns the table owns the query.
	auditHandler := audit.NewHandler(pool, auditService, az, auditSink)
	auditHandler.RegisterRoutes(mux, adminMw)
	// Export and the sink test get their own budget. An export is the single
	// highest-value request in the product (one request, a lot of rows) and the
	// sink test is an outbound call, so both carry a much lower per-IP limit than
	// the general API's 600/min — the same treatment the mail test gets below.
	auditHandler.RegisterExportRoutes(mux, func(next http.Handler) http.Handler {
		if !cfg.RateLimit.Enabled {
			return adminMw(next)
		}
		return ratelimit.MiddlewareByIP(redisClient, ratelimit.Config{
			RequestsPerMinute: cfg.Audit.ExportPerMinute,
			Window:            time.Minute,
			TrustProxy:        cfg.RateLimit.TrustProxy,
			TrustedProxyHops:  cfg.RateLimit.TrustedProxyHops,
		})(adminMw(next))
	})
	// The mail configuration test gets its own chain. It is an outbound-mail
	// trigger, so it carries a much lower per-IP budget than the general API
	// limit — which at 600/min would let one admin account emit ten test
	// messages a second.
	adminHandler.RegisterMailRoutes(mux, func(next http.Handler) http.Handler {
		if !cfg.RateLimit.Enabled {
			return adminMw(next)
		}
		return ratelimit.MiddlewareByIP(redisClient, ratelimit.Config{
			RequestsPerMinute: cfg.Mail.TestPerMinute,
			Window:            time.Minute,
			TrustProxy:        cfg.RateLimit.TrustProxy,
			TrustedProxyHops:  cfg.RateLimit.TrustedProxyHops,
		})(adminMw(next))
	})
	// The storage configuration test shares the mail test's budget for the same
	// reason: it is a real round trip to a third party, triggered by an HTTP
	// request, and the general API limit is far too generous for that.
	adminHandler.RegisterStorageRoutes(mux, func(next http.Handler) http.Handler {
		if !cfg.RateLimit.Enabled {
			return adminMw(next)
		}
		return ratelimit.MiddlewareByIP(redisClient, ratelimit.Config{
			RequestsPerMinute: cfg.Mail.TestPerMinute,
			Window:            time.Minute,
			TrustProxy:        cfg.RateLimit.TrustProxy,
			TrustedProxyHops:  cfg.RateLimit.TrustedProxyHops,
		})(adminMw(next))
	})
	if searchHandler != nil {
		searchHandler.RegisterRoutes(mux, authMw)
	}
	messageHandler.RegisterRoutes(mux, authMw)
	webhookHandler := webhook.NewHandler(pool, az, natsClient)
	webhookHandler.RegisterRoutes(mux, authMw)
	emoji.NewHandler(pool, az).RegisterRoutes(mux, authMw)
	block.NewHandler(pool).RegisterRoutes(mux, authMw)

	// WebSocket (handles its own auth)
	wsHandler.RegisterRoutes(mux)

	// Middleware chain, written outside-in from the bottom up. RequestID is
	// outermost (after CORS) so the correlation id exists for RecoveryMiddleware
	// and LoggingMiddleware, and for logger.FromContext inside handlers.
	handler := httputil.EnvelopeMuxErrors(mux)
	if cfg.RateLimit.Enabled {
		handler = ratelimit.APIMiddleware(redisClient, ratelimit.Config{
			RequestsPerMinute: cfg.RateLimit.APIPerMinute,
			Window:            time.Minute,
			TrustProxy:        cfg.RateLimit.TrustProxy,
			TrustedProxyHops:  cfg.RateLimit.TrustedProxyHops,
		}, identifyCaller)(handler)
	}
	handler = MetricsMiddleware(handler)
	handler = httputil.LoggingMiddleware(logger)(handler)
	handler = httputil.RecoveryMiddleware(logger)(handler)
	handler = httputil.RequestIDMiddleware(handler)
	handler = cors.New(cors.Options{
		AllowedOrigins: cfg.CORS.AllowedOrigins,
		AllowedMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{"Authorization", "Content-Type", "X-Request-ID"},
		// The header is set on every response; without exposing it a browser
		// client cannot read it and cannot quote it in a bug report.
		ExposedHeaders: []string{"X-Request-ID"},
		// Auth is via Bearer tokens (no cookies), so credentialed CORS is not
		// needed — and "*" origins with credentials is rejected by browsers.
		AllowCredentials: false,
		MaxAge:           86400,
	}).Handler(handler)

	appInstance.Server = &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port),
		Handler:      handler,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	// Seed admin account + default workspace/channel on first boot. A failure
	// here is fatal: the alternative is a running server nobody can log in to,
	// which the old Warn-and-continue produced silently.
	if err := seedAdmin(ctx, pool, cfg, logger); err != nil {
		appInstance.Close()
		return nil, fmt.Errorf("seed admin: %w", err)
	}

	return appInstance, nil
}

// liveRevoker maps authz's object types onto the two things in this process
// that hold already-authorized connections: ws.Hub for channel subscriptions
// and collab.Service for document rooms.
//
// It lives in the composition root because that mapping is a wiring fact, not
// an authorization one — internal/authz must not know that a channel is a hub
// subscription — and because it is the only place that can see both halves.
//
// collab is an atomic pointer for one reason: the collaboration service
// authorizes through the checker, and the checker revokes through the
// collaboration service, so one of the two references has to be filled in after
// construction. An atomic makes that late binding explicit and race-free rather
// than relying on "nothing reads it before the server starts".
type liveRevoker struct {
	hub    *ws.Hub
	collab atomic.Pointer[collab.Service]
}

func (l *liveRevoker) RevokeObjectForSubject(subject authz.SubjectRef, obj authz.ObjectRef) {
	// A group has no connections of its own; its members' sessions are revoked
	// when the group's own grants are, which is a Drive-era concern. Silently
	// doing nothing here is correct, not a gap.
	if subject.Type != authz.SubjectUser {
		return
	}
	switch obj.Type {
	case authz.TypeChannel:
		if l.hub != nil {
			l.hub.RevokeChannelSubscription(subject.ID, obj.ID)
		}
	case authz.TypeDoc:
		if svc := l.collab.Load(); svc != nil {
			svc.RevokeAccess(subject.ID, obj.ID)
		}
	}
}

func (l *liveRevoker) RevokeObjectForAll(obj authz.ObjectRef) {
	switch obj.Type {
	case authz.TypeChannel:
		if l.hub != nil {
			l.hub.RevokeChannelForAll(obj.ID)
		}
	case authz.TypeDoc:
		if svc := l.collab.Load(); svc != nil {
			svc.RevokeDocument(obj.ID)
		}
	}
}

// EventStreamName is the JetStream stream backing every durable domain event.
const EventStreamName = "SUPEROPS"

// EnsureEventStream creates or updates the JetStream stream that
// natspkg.Client.PublishDurable writes into.
//
// Both cmd/worker and app.New call it. When only the worker did, an API replica
// started without a worker had no stream to publish into and every message
// handler paid the full JetStream ack timeout before logging a failure.
func EnsureEventStream(ctx context.Context, nc *natspkg.Client, logger *slog.Logger) error {
	if nc == nil || nc.JetStream == nil {
		return errors.New("JetStream is not configured")
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if _, err := nc.JetStream.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      EventStreamName,
		Subjects:  []string{"superops.>"},
		Retention: jetstream.InterestPolicy,
		Storage:   jetstream.FileStorage,
		MaxAge:    24 * time.Hour,
		// Matches the Nats-Msg-Id dedupe window PublishDurable relies on.
		Duplicates: 2 * time.Minute,
	}); err != nil {
		return fmt.Errorf("create JetStream stream %s: %w", EventStreamName, err)
	}
	if logger != nil {
		logger.Info("JetStream stream ready", "stream", EventStreamName)
	}
	return nil
}

// seedAdvisoryLock keys the Postgres advisory lock that serializes first-boot
// seeding. Concurrent replicas (a Deployment scaling from 0 to 3) otherwise all
// pass the "does the admin exist?" check at once and then race on the inserts.
const seedAdvisoryLock int64 = 0x5EED_ADA1

// seedAdmin creates the admin account, the default workspace and #general on
// first boot.
//
// It runs as one transaction. The previous version issued five independent
// statements, discarded three of their results, and was only Warn-logged by the
// caller — so a failure halfway through left a user with no workspace, or a
// workspace with no members, and the server started anyway.
func seedAdmin(ctx context.Context, pool *pgxpool.Pool, cfg *Config, logger *slog.Logger) error {
	return database.WithTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, seedAdvisoryLock); err != nil {
			return fmt.Errorf("acquire seed lock: %w", err)
		}

		var adminID string
		err := tx.QueryRow(ctx, `SELECT id FROM users WHERE email = $1`, cfg.Admin.Email).Scan(&adminID)
		switch {
		case err == nil:
			return nil // already seeded
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("look up admin user: %w", err)
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(cfg.Admin.Password), 12)
		if err != nil {
			return fmt.Errorf("hash admin password: %w", err)
		}

		adminID = uuid.NewString()
		if _, err := tx.Exec(ctx,
			`INSERT INTO users (id, email, username, full_name, password_hash, is_active)
			 VALUES ($1, $2, $3, 'Admin', $4, true)`,
			adminID, cfg.Admin.Email, cfg.Admin.Username, string(hash),
		); err != nil {
			return fmt.Errorf("create admin user: %w", err)
		}

		// workspaces.slug is globally UNIQUE, so a second admin email against an
		// existing database must adopt the workspace rather than fail the boot.
		wsID, err := upsertReturningID(ctx, tx,
			`INSERT INTO workspaces (id, name, slug, owner_id)
			 VALUES ($1, 'SuperOps', 'superops', $2)
			 ON CONFLICT (slug) DO NOTHING
			 RETURNING id`,
			`SELECT id FROM workspaces WHERE slug = 'superops'`,
			[]any{uuid.NewString(), adminID}, nil)
		if err != nil {
			return fmt.Errorf("create default workspace: %w", err)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, 'owner')
			 ON CONFLICT (workspace_id, user_id) DO NOTHING`,
			wsID, adminID,
		); err != nil {
			return fmt.Errorf("add admin to workspace: %w", err)
		}

		chID, err := upsertReturningID(ctx, tx,
			`INSERT INTO channels (id, workspace_id, name, slug, description, type, creator_id)
			 VALUES ($1, $2, 'general', 'general', 'General discussion', 'public', $3)
			 ON CONFLICT (workspace_id, slug) DO NOTHING
			 RETURNING id`,
			`SELECT id FROM channels WHERE workspace_id = $1 AND slug = 'general'`,
			[]any{uuid.NewString(), wsID, adminID}, []any{wsID})
		if err != nil {
			return fmt.Errorf("create #general channel: %w", err)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO channel_members (channel_id, user_id, role) VALUES ($1, $2, 'admin')
			 ON CONFLICT (channel_id, user_id) DO NOTHING`,
			chID, adminID,
		); err != nil {
			return fmt.Errorf("add admin to #general: %w", err)
		}

		logger.Info("seeded admin account, default workspace and #general channel",
			"email", cfg.Admin.Email, "workspace_id", wsID, "channel_id", chID)
		return nil
	})
}

// upsertReturningID runs an "ON CONFLICT DO NOTHING ... RETURNING id" insert and
// falls back to selectSQL when the conflict swallowed the RETURNING row.
func upsertReturningID(ctx context.Context, tx pgx.Tx, insertSQL, selectSQL string, insertArgs, selectArgs []any) (string, error) {
	var id string
	err := tx.QueryRow(ctx, insertSQL, insertArgs...).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if err := tx.QueryRow(ctx, selectSQL, selectArgs...).Scan(&id); err != nil {
		return "", fmt.Errorf("resolve existing row: %w", err)
	}
	return id, nil
}

// BeginDrain marks the replica as not-ready. Call it as soon as SIGTERM lands so
// the load balancer stops sending new work while the in-flight requests finish.
func (a *App) BeginDrain() {
	if a != nil {
		a.draining.Store(true)
	}
}

func (a *App) Close() {
	// Drain the audit buffer FIRST, while the pool is still open. Closing the
	// pool underneath it would turn every queued record into a write error — the
	// records for the last requests this replica served, which are exactly the
	// ones an incident timeline needs.
	if a.audit != nil {
		a.audit.Close()
	}
	if a.DB != nil {
		a.DB.Close()
	}
	if a.Redis != nil {
		a.Redis.Close()
	}
	if a.NATS != nil {
		a.NATS.Close()
	}
}

// liveHandler is the liveness/startup probe. It answers 200 as soon as the
// process is serving, and deliberately checks no dependency: liveness failure
// means "restart me", and no amount of restarting fixes a database outage.
func liveHandler(w http.ResponseWriter, _ *http.Request) {
	httputil.JSON(w, http.StatusOK, map[string]string{"status": "alive"})
}

// readyHandler is the readiness probe: it reports whether this replica can
// currently serve traffic. Every dependency is probed and named individually so
// the failing one is visible in `kubectl describe` instead of a bare 503.
func (a *App) readyHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Bound the probe: a hung dependency must fail the check, not hold the
		// connection open until the kubelet's own timeout.
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		checks := map[string]string{
			"postgres": "ok",
			"redis":    "ok",
			"nats":     "ok",
		}
		ready := true

		if a.draining.Load() {
			httputil.JSON(w, http.StatusServiceUnavailable, map[string]any{
				"status": "draining",
				"checks": checks,
			})
			return
		}

		if err := a.DB.Ping(ctx); err != nil {
			checks["postgres"] = err.Error()
			ready = false
		}
		if err := a.Redis.Ping(ctx).Err(); err != nil {
			checks["redis"] = err.Error()
			ready = false
		}
		if a.NATS == nil || a.NATS.Conn == nil || !a.NATS.Conn.IsConnected() {
			checks["nats"] = "not connected"
			ready = false
		}

		status, label := http.StatusOK, "ready"
		if !ready {
			status, label = http.StatusServiceUnavailable, "not ready"
		}
		httputil.JSON(w, status, map[string]any{"status": label, "checks": checks})
	}
}

// storageValidator narrows a Backend for the admin surface, preserving nil.
//
// The explicit nil check is load-bearing: assigning a nil storage.Backend to an
// admin.StorageValidator produces a NON-nil interface holding a nil value, so
// `h.storage.Backend == nil` would be false and the handler would call Validate
// on nothing. That is the classic typed-nil trap and it would turn "storage is
// not configured" into a panic.
func storageValidator(b storage.Backend) admin.StorageValidator {
	if b == nil {
		return nil
	}
	return b
}
