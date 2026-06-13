package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"github.com/rs/cors"

	"github.com/Wick-Lim/SuperOps/backend/internal/admin"
	"github.com/Wick-Lim/SuperOps/backend/internal/audit"
	"github.com/Wick-Lim/SuperOps/backend/internal/auth"
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
	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
	redispkg "github.com/Wick-Lim/SuperOps/backend/pkg/redis"

	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

type App struct {
	Config *Config
	Logger *slog.Logger
	DB     *pgxpool.Pool
	Redis  *goredis.Client
	NATS   *natspkg.Client
	Hub    *ws.Hub
	Server *http.Server
}

func New(ctx context.Context, cfg *Config, logger *slog.Logger) (*App, error) {
	// Infrastructure
	pool, err := database.NewPool(ctx, database.Config{
		DSN:      cfg.DB.DSN(),
		MaxConns: cfg.DB.MaxConns,
		MinConns: cfg.DB.MinConns,
	}, logger)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}

	redisClient, err := redispkg.NewClient(ctx, redispkg.Config{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	}, logger)
	if err != nil {
		return nil, fmt.Errorf("redis: %w", err)
	}

	natsClient, err := natspkg.NewClient(natspkg.Config{
		URL: cfg.NATS.URL,
	}, logger)
	if err != nil {
		return nil, fmt.Errorf("nats: %w", err)
	}

	// Repositories
	userRepo := user.NewRepository(pool)
	authRepo := auth.NewRepository(pool)
	workspaceRepo := workspace.NewRepository(pool)
	channelRepo := channel.NewRepository(pool)
	messageRepo := message.NewRepository(pool)

	// Services
	jwtMgr := auth.NewJWTManager(cfg.JWT.Secret, cfg.JWT.AccessTokenTTL)
	authService := auth.NewService(authRepo, userRepo, pool, jwtMgr, cfg.JWT.RefreshTokenTTL)
	presenceService := presence.NewService(redisClient)

	notificationRepo := notification.NewRepository(pool)

	// Search
	var searchService *search.Service
	if cfg.Meili.Host != "" {
		searchService, err = search.NewService(cfg.Meili.Host, cfg.Meili.MasterKey, logger)
		if err != nil {
			logger.Warn("Meilisearch not available, search disabled", "error", err)
		}
	}

	// File Storage
	fileStorage, err := file.NewStorage(file.StorageConfig{
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
	workspaceHandler := workspace.NewHandler(workspaceRepo)
	channelHandler := channel.NewHandler(channelRepo)
	messageHandler := message.NewHandler(messageRepo, channelRepo, natsClient)
	wsHandler := ws.NewWSHandler(hub, jwtMgr, presenceService, logger)
	presenceHandler := presence.NewHandler(presenceService, pool)
	var fileHandler *file.Handler
	if fileStorage != nil {
		fileHandler = file.NewHandler(fileStorage, pool)
	}
	notificationHandler := notification.NewHandler(notificationRepo)
	auditService := audit.NewService(pool)
	adminHandler := admin.NewHandler(pool, auditService)
	var searchHandler *search.Handler
	if searchService != nil {
		searchHandler = search.NewHandler(searchService)
	}

	// Router
	mux := http.NewServeMux()

	// Health + metrics endpoints (no auth)
	mux.HandleFunc("GET /health", healthHandler(pool, redisClient))
	mux.HandleFunc("GET /ready", healthHandler(pool, redisClient))
	mux.HandleFunc("GET /metrics", metricsHandler(hub))

	// Rate limiters (Redis-backed). A strict per-IP limit guards the
	// brute-forceable auth endpoints; a generous per-user limit guards the API.
	var loginLimiter func(http.Handler) http.Handler
	if cfg.RateLimit.Enabled {
		loginLimiter = ratelimit.MiddlewareByIP(redisClient, ratelimit.Config{
			RequestsPerMinute: cfg.RateLimit.AuthPerMinute,
			Window:            time.Minute,
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
	// Admin endpoints require an authenticated caller who administers a workspace.
	adminMw := func(next http.Handler) http.Handler {
		return authMw(rbac.RequireSystemAdmin(pool)(next))
	}
	adminHandler.RegisterRoutes(mux, adminMw)
	if searchHandler != nil {
		searchHandler.RegisterRoutes(mux, authMw)
	}
	messageHandler.RegisterRoutes(mux, authMw)
	webhookHandler := webhook.NewHandler(pool)
	webhookHandler.RegisterRoutes(mux, authMw)
	emoji.NewHandler(pool).RegisterRoutes(mux, authMw)
	block.NewHandler(pool).RegisterRoutes(mux, authMw)

	// WebSocket (handles its own auth)
	wsHandler.RegisterRoutes(mux)

	// Middleware chain
	var handler http.Handler = mux
	if cfg.RateLimit.Enabled {
		handler = ratelimit.APIMiddleware(redisClient, ratelimit.Config{
			RequestsPerMinute: cfg.RateLimit.APIPerMinute,
			Window:            time.Minute,
		})(handler)
	}
	handler = MetricsMiddleware(handler)
	handler = httputil.RequestIDMiddleware(handler)
	handler = httputil.LoggingMiddleware(logger)(handler)
	handler = httputil.RecoveryMiddleware(logger)(handler)
	handler = cors.New(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"*"},
		AllowCredentials: true,
		MaxAge:           86400,
	}).Handler(handler)

	server := &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port),
		Handler:      handler,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	// Seed admin account + default workspace/channel on first boot
	if err := seedAdmin(ctx, pool, cfg, logger); err != nil {
		logger.Warn("admin seed failed", "error", err)
	}

	return &App{
		Config: cfg,
		Logger: logger,
		DB:     pool,
		Redis:  redisClient,
		NATS:   natsClient,
		Hub:    hub,
		Server: server,
	}, nil
}

func seedAdmin(ctx context.Context, pool *pgxpool.Pool, cfg *Config, logger *slog.Logger) error {
	// Check if admin already exists
	var exists bool
	pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM users WHERE email = $1)", cfg.Admin.Email).Scan(&exists)
	if exists {
		return nil
	}

	// Create admin user
	hash, err := bcrypt.GenerateFromPassword([]byte(cfg.Admin.Password), 12)
	if err != nil {
		return fmt.Errorf("hash admin password: %w", err)
	}

	adminID := uuid.NewString()
	_, err = pool.Exec(ctx,
		`INSERT INTO users (id, email, username, full_name, password_hash, is_active)
		 VALUES ($1, $2, $3, 'Admin', $4, true)`,
		adminID, cfg.Admin.Email, cfg.Admin.Username, string(hash),
	)
	if err != nil {
		return fmt.Errorf("create admin user: %w", err)
	}
	logger.Info("admin account created", "email", cfg.Admin.Email)

	// Create default workspace
	wsID := uuid.NewString()
	_, err = pool.Exec(ctx,
		`INSERT INTO workspaces (id, name, slug, owner_id) VALUES ($1, 'SuperOps', 'superops', $2)`,
		wsID, adminID,
	)
	if err != nil {
		return fmt.Errorf("create default workspace: %w", err)
	}

	// Add admin as workspace owner
	pool.Exec(ctx,
		`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`,
		wsID, adminID,
	)

	// Create #general channel
	chID := uuid.NewString()
	pool.Exec(ctx,
		`INSERT INTO channels (id, workspace_id, name, slug, description, type, creator_id) VALUES ($1, $2, 'general', 'general', 'General discussion', 'public', $3)`,
		chID, wsID, adminID,
	)
	pool.Exec(ctx,
		`INSERT INTO channel_members (channel_id, user_id, role) VALUES ($1, $2, 'admin')`,
		chID, adminID,
	)

	logger.Info("default workspace and #general channel created", "workspace_id", wsID)
	return nil
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

func healthHandler(pool *pgxpool.Pool, redis *goredis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		if err := pool.Ping(ctx); err != nil {
			httputil.JSONError(w, http.StatusServiceUnavailable, "UNHEALTHY", "database unavailable")
			return
		}
		if err := redis.Ping(ctx).Err(); err != nil {
			httputil.JSONError(w, http.StatusServiceUnavailable, "UNHEALTHY", "redis unavailable")
			return
		}

		httputil.JSON(w, http.StatusOK, map[string]string{"status": "healthy"})
	}
}
