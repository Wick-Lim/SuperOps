package main

import (
	"context"
	"log"
	"log/slog"
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

	// Search indexer — queue group ensures each event is indexed once across
	// multiple worker replicas. Subscribes to all message lifecycle events.
	if cfg.Meili.Host != "" {
		searchSvc, err := search.NewService(cfg.Meili.Host, cfg.Meili.MasterKey, l)
		if err != nil {
			l.Warn("meilisearch not available, search indexing disabled", "error", err)
		} else {
			indexer := search.NewIndexer(searchSvc, l)
			if _, err := natsClient.Conn.QueueSubscribe("superops.*.message.*", "indexer", indexer.HandleMessage); err != nil {
				l.Error("subscribe search indexer", "error", err)
			} else {
				l.Info("search indexer subscribed to message.*")
			}
		}
	}

	// Notification service — queue group ensures each event notifies once.
	notifRepo := notification.NewRepository(pool)
	notifSvc := notification.NewService(notifRepo, pool, natsClient, l)
	if _, err := natsClient.Conn.QueueSubscribe("superops.*.message.created", "notifier", notifSvc.HandleMessage); err != nil {
		l.Error("subscribe notification service", "error", err)
	} else {
		l.Info("notification service subscribed to message.created")
	}

	// Background jobs
	go sessionCleanup(ctx, pool, l)
	go scheduledMessageJob(ctx, pool, natsClient, l)
	go retentionJob(ctx, pool, l)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	l.Info("worker ready, processing events...")
	<-quit

	cancel()
	l.Info("worker shutting down")
}

func sessionCleanup(ctx context.Context, pool *pgxpool.Pool, l *slog.Logger) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if tag, err := pool.Exec(ctx, "DELETE FROM sessions WHERE expires_at < NOW()"); err == nil && tag.RowsAffected() > 0 {
				l.Info("cleaned expired sessions", "count", tag.RowsAffected())
			}
			// Also clean up long-expired invitations.
			pool.Exec(ctx, "UPDATE invitations SET status = 'expired' WHERE status = 'pending' AND expires_at < NOW()")
		case <-ctx.Done():
			return
		}
	}
}

// scheduledMessageJob promotes due scheduled messages to live messages and
// broadcasts them so they appear in real time.
func scheduledMessageJob(ctx context.Context, pool *pgxpool.Pool, nc *natspkg.Client, l *slog.Logger) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			rows, err := pool.Query(ctx,
				`UPDATE messages SET is_scheduled = FALSE, created_at = NOW(), updated_at = NOW()
				 WHERE is_scheduled = TRUE AND scheduled_at IS NOT NULL AND scheduled_at <= NOW()
				 RETURNING id, channel_id, user_id, parent_id, content, content_type, created_at`)
			if err != nil {
				l.Warn("scheduled job: query", "error", err)
				continue
			}
			type due struct {
				id, channelID, userID, content, contentType string
				parentID                                     *string
				createdAt                                    time.Time
			}
			var dues []due
			for rows.Next() {
				var d due
				if err := rows.Scan(&d.id, &d.channelID, &d.userID, &d.parentID, &d.content, &d.contentType, &d.createdAt); err == nil {
					dues = append(dues, d)
				}
			}
			rows.Close()

			for _, d := range dues {
				var wsID string
				_ = pool.QueryRow(ctx, `SELECT workspace_id FROM channels WHERE id = $1`, d.channelID).Scan(&wsID)
				pool.Exec(ctx, `UPDATE channels SET last_message_at = NOW() WHERE id = $1`, d.channelID)
				// Thread replies must bump the parent's reply_count (Create does
				// this for live messages; the scheduled path bypassed it).
				if d.parentID != nil && *d.parentID != "" {
					pool.Exec(ctx, `UPDATE messages SET reply_count = reply_count + 1 WHERE id = $1`, *d.parentID)
				}
				if wsID == "" {
					continue
				}
				_ = nc.Publish("superops."+wsID+".message.created", natspkg.Event{
					Type: "message.new",
					Data: map[string]interface{}{
						"id": d.id, "channel_id": d.channelID, "user_id": d.userID,
						"parent_id": d.parentID, "content": d.content, "content_type": d.contentType,
						"created_at": d.createdAt.Format(time.RFC3339Nano),
					},
				})
			}
			if len(dues) > 0 {
				l.Info("sent scheduled messages", "count", len(dues))
			}
		case <-ctx.Done():
			return
		}
	}
}

// retentionJob enforces per-workspace message retention policies by hard-deleting
// messages older than the configured number of days.
func retentionJob(ctx context.Context, pool *pgxpool.Pool, l *slog.Logger) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	run := func() {
		tag, err := pool.Exec(ctx,
			`DELETE FROM messages m
			 USING channels c, workspaces w
			 WHERE m.channel_id = c.id AND c.workspace_id = w.id
			   AND w.retention_days > 0
			   AND m.created_at < NOW() - (w.retention_days || ' days')::interval`)
		if err != nil {
			l.Warn("retention job", "error", err)
			return
		}
		if tag.RowsAffected() > 0 {
			l.Info("retention: purged messages", "count", tag.RowsAffected())
		}
	}

	// Run once at startup, then hourly.
	run()
	for {
		select {
		case <-ticker.C:
			run()
		case <-ctx.Done():
			return
		}
	}
}
