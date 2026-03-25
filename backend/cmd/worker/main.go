package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Wick-Lim/SuperOps/backend/internal/app"
	"github.com/Wick-Lim/SuperOps/backend/internal/notification"
	"github.com/Wick-Lim/SuperOps/backend/internal/search"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
	"github.com/Wick-Lim/SuperOps/backend/pkg/logger"
	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
)

func main() {
	cfg, err := app.LoadConfig()
	if err != nil {
		log.Fatal("load config: ", err)
	}

	l := logger.New(cfg.LogLevel)
	l.Info("starting worker")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Database
	pool, err := database.NewPool(ctx, database.Config{
		DSN: cfg.DB.DSN(), MaxConns: cfg.DB.MaxConns, MinConns: cfg.DB.MinConns,
	}, l)
	if err != nil {
		log.Fatal("database: ", err)
	}
	defer pool.Close()

	// NATS
	natsClient, err := natspkg.NewClient(natspkg.Config{URL: cfg.NATS.URL}, l)
	if err != nil {
		log.Fatal("nats: ", err)
	}
	defer natsClient.Close()

	// Create JetStream durable stream for reliable message processing
	_, err = natsClient.JetStream.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      "SUPEROPS",
		Subjects:  []string{"superops.>"},
		Retention: jetstream.InterestPolicy,
		Storage:   jetstream.FileStorage,
		MaxAge:    24 * time.Hour,
	})
	if err != nil {
		l.Warn("JetStream stream creation failed (non-fatal)", "error", err)
	} else {
		l.Info("JetStream stream SUPEROPS ready")
	}

	// Search indexer
	if cfg.Meili.Host != "" {
		searchSvc, err := search.NewService(cfg.Meili.Host, cfg.Meili.MasterKey, l)
		if err != nil {
			l.Warn("meilisearch not available, search indexing disabled", "error", err)
		} else {
			indexer := search.NewIndexer(searchSvc, l)
			_, err := natsClient.Conn.Subscribe("superops.*.message.created", indexer.HandleMessage)
			if err != nil {
				l.Error("subscribe search indexer", "error", err)
			} else {
				l.Info("search indexer subscribed to message.created")
			}
		}
	}

	// Notification service
	notifRepo := notification.NewRepository(pool)
	notifSvc := notification.NewService(notifRepo, l)
	_, err = natsClient.Conn.Subscribe("superops.*.message.created", notifSvc.HandleMessage)
	if err != nil {
		l.Error("subscribe notification service", "error", err)
	} else {
		l.Info("notification service subscribed to message.created")
	}

	// Session cleanup (every 10 minutes)
	go sessionCleanup(ctx, pool, nil)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	l.Info("worker ready, processing events...")
	<-quit

	cancel()
	l.Info("worker shutting down")
}

func sessionCleanup(ctx context.Context, pool *pgxpool.Pool, _ interface{}) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			tag, err := pool.Exec(ctx, "DELETE FROM sessions WHERE expires_at < NOW()")
			if err == nil {
				if tag.RowsAffected() > 0 {
					// log cleaned sessions
					_ = tag
				}
			}
		case <-ctx.Done():
			return
		}
	}
}
