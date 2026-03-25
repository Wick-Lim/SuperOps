package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
	"github.com/rs/cors"

	"github.com/Wick-Lim/SuperOps/backend/internal/auth"
	"github.com/Wick-Lim/SuperOps/backend/internal/channel"
	"github.com/Wick-Lim/SuperOps/backend/internal/message"
	"github.com/Wick-Lim/SuperOps/backend/internal/user"
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
	authService := auth.NewService(authRepo, userRepo, jwtMgr, cfg.JWT.RefreshTokenTTL)

	// WebSocket Hub
	hub := ws.NewHub(logger)
	go hub.Run()

	// Handlers
	authHandler := auth.NewHandler(authService)
	userHandler := user.NewHandler(userRepo)
	workspaceHandler := workspace.NewHandler(workspaceRepo)
	channelHandler := channel.NewHandler(channelRepo)
	messageHandler := message.NewHandler(messageRepo, channelRepo, natsClient)
	wsHandler := ws.NewWSHandler(hub, jwtMgr, logger)

	// Router
	mux := http.NewServeMux()

	// Health endpoints (no auth)
	mux.HandleFunc("GET /health", healthHandler(pool, redisClient))
	mux.HandleFunc("GET /ready", healthHandler(pool, redisClient))

	// Auth routes (no auth middleware)
	authHandler.RegisterRoutes(mux)

	// Auth middleware
	authMw := auth.Middleware(jwtMgr)

	// Protected routes
	userHandler.RegisterRoutes(mux, authMw)
	workspaceHandler.RegisterRoutes(mux, authMw)
	channelHandler.RegisterRoutes(mux, authMw)
	messageHandler.RegisterRoutes(mux, authMw)

	// WebSocket (handles its own auth)
	wsHandler.RegisterRoutes(mux)

	// Middleware chain
	var handler http.Handler = mux
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
