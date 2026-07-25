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
	"github.com/Wick-Lim/SuperOps/backend/internal/emoji"
	"github.com/Wick-Lim/SuperOps/backend/internal/file"
	"github.com/Wick-Lim/SuperOps/backend/internal/message"
	"github.com/Wick-Lim/SuperOps/backend/internal/notification"
	"github.com/Wick-Lim/SuperOps/backend/internal/presence"
	"github.com/Wick-Lim/SuperOps/backend/internal/ratelimit"
	"github.com/Wick-Lim/SuperOps/backend/internal/rbac"
	"github.com/Wick-Lim/SuperOps/backend/internal/search"
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
	notificationRepo := notification.NewRepository(pool)

	// authz.Checker is the single source of truth for membership/role decisions.
	// Every handler that used to hand-write an EXISTS query now shares it.
	az := authz.New(pool)

	// audit must exist before auth: auth.Service records login/logout/password
	// events through it.
	auditService := audit.NewService(pool, logger)

	// Services
	jwtMgr := auth.NewJWTManager(cfg.JWT.Secret, cfg.JWT.AccessTokenTTL)
	authService := auth.NewService(authRepo, userRepo, pool, jwtMgr, cfg.JWT.RefreshTokenTTL, auditService)
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

	// File Storage
	var fileStorage *file.Storage
	if cfg.MinIO.IsEnabled() {
		fileStorage, err = file.NewStorage(file.StorageConfig{
			Endpoint:  cfg.MinIO.Endpoint,
			AccessKey: cfg.MinIO.AccessKey,
			SecretKey: cfg.MinIO.SecretKey,
			Bucket:    cfg.MinIO.Bucket,
			UseSSL:    cfg.MinIO.UseSSL,
		}, logger)
		if err != nil {
			logger.Warn("MinIO not available, file uploads disabled", "error", err)
			fileStorage = nil
		}
	} else {
		logger.Info("file uploads disabled by configuration")
	}

	// WebSocket Hub with NATS bridge for multi-replica support.
	// - NATS bridge: relays client-originated ephemeral events (typing) between replicas.
	// - Event relay: fans application domain events (message/reaction/notification) to local clients.
	hub := ws.NewHub(logger)
	hub.StartNATSBridge(natsClient.Conn, logger)
	hub.StartEventRelay(natsClient.Conn, logger)
	go hub.Run()

	// Handlers
	authHandler := auth.NewHandler(authService)
	userHandler := user.NewHandler(userRepo)
	workspaceHandler := workspace.NewHandler(workspaceRepo, az)
	channelHandler := channel.NewHandler(channelRepo, az)
	messageHandler := message.NewHandler(messageRepo, az, natsClient, logger)
	wsHandler := ws.NewWSHandler(
		hub,
		jwtMgr,
		presenceService,
		// MemberChecker: a DB error must stay distinct from "not a member", or a
		// transient blip revokes every subscription at once.
		func(ctx context.Context, channelID, userID string) (bool, error) {
			return az.IsChannelMember(ctx, channelID, userID)
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
	)
	presenceHandler := presence.NewHandler(presenceService, az, pool)
	var fileHandler *file.Handler
	if fileStorage != nil {
		fileHandler = file.NewHandler(fileStorage, pool, az)
	}
	notificationHandler := notification.NewHandler(notificationRepo)
	adminHandler := admin.NewHandler(pool, auditService, az)
	var searchHandler *search.Handler
	if searchService != nil {
		searchHandler = search.NewHandler(searchService, az)
	}

	appInstance := &App{
		Config: cfg,
		Logger: logger,
		DB:     pool,
		Redis:  redisClient,
		NATS:   natsClient,
		Hub:    hub,
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
	mux.HandleFunc("GET /metrics", metricsHandler(hub, pool, cfg.MetricsToken))

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

	// Auth middleware
	authMw := auth.Middleware(jwtMgr)

	// Protected routes
	authHandler.RegisterProtectedRoutes(mux, authMw)
	userHandler.RegisterRoutes(mux, authMw)
	workspaceHandler.RegisterRoutes(mux, authMw)
	channelHandler.RegisterRoutes(mux, authMw)
	presenceHandler.RegisterRoutes(mux, authMw)
	if fileHandler != nil {
		fileHandler.RegisterRoutes(mux, authMw)
	}
	notificationHandler.RegisterRoutes(mux, authMw)
	// Admin endpoints require an authenticated caller who administers a
	// workspace. Each handler re-scopes to the workspaces that caller actually
	// administers, so this is a cheap pre-filter, not the whole authorization.
	adminMw := func(next http.Handler) http.Handler {
		return authMw(rbac.RequireAnyWorkspaceAdmin(az)(next))
	}
	adminHandler.RegisterRoutes(mux, adminMw)
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
	var handler http.Handler = httputil.EnvelopeMuxErrors(mux)
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
