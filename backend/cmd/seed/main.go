// Command seed inserts demo data (idempotent) into the admin's workspace: a few
// users, a #random channel, and some sample messages.
//
// It publishes the same message.created events the REST path publishes. Writing
// straight to Postgres is not enough: search indexing, notifications and the
// WebSocket relay are all driven by those events, so seeded messages used to be
// invisible to search and never appeared in an open client until a reload.
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/app"
	"github.com/Wick-Lim/SuperOps/backend/internal/message"
	"github.com/Wick-Lim/SuperOps/backend/pkg/crypto"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
	"github.com/Wick-Lim/SuperOps/backend/pkg/logger"
	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
)

const demoPassword = "demo_password_123"

func main() { os.Exit(run()) }

func run() int {
	cfg, err := app.LoadConfig()
	if err != nil {
		log.Print("load config: ", err)
		return 1
	}

	l := logger.New(cfg.LogLevel)
	ctx := context.Background()

	pool, err := database.NewPool(ctx, database.Config{
		DSN:                      cfg.DB.DSN(),
		MaxConns:                 cfg.DB.MaxConns,
		MinConns:                 cfg.DB.MinConns,
		StatementTimeout:         cfg.DB.StatementTimeout,
		LockTimeout:              cfg.DB.LockTimeout,
		IdleInTransactionTimeout: cfg.DB.IdleInTransactionTimeout,
	}, l)
	if err != nil {
		l.Error("database", "error", err)
		return 1
	}
	defer pool.Close()

	// NATS is optional for seeding, but without it the seeded messages are not
	// indexed and not relayed — say so loudly rather than reporting success.
	var natsClient *natspkg.Client
	if nc, err := natspkg.NewClient(natspkg.Config{URL: cfg.NATS.URL, DrainTimeout: cfg.NATS.DrainTimeout}, l); err != nil {
		l.Warn("NATS unavailable: seeded messages will NOT be indexed or relayed; run cmd/reindex afterwards", "error", err)
	} else {
		natsClient = nc
		defer natsClient.Close()
		if err := app.EnsureEventStream(ctx, natsClient, l); err != nil {
			l.Warn("JetStream stream unavailable; seeded messages may not be indexed", "error", err)
		}
	}

	// Locate the seeded admin + their workspace.
	var adminID, wsID string
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE email = $1`, cfg.Admin.Email).Scan(&adminID); err != nil {
		l.Error("admin user not found — start the server once first", "email", cfg.Admin.Email, "error", err)
		return 1
	}
	if err := pool.QueryRow(ctx,
		`SELECT workspace_id FROM workspace_members WHERE user_id = $1 ORDER BY joined_at LIMIT 1`,
		adminID).Scan(&wsID); err != nil {
		l.Error("admin workspace not found", "error", err)
		return 1
	}

	userIDs, err := seedUsers(ctx, pool, wsID, l)
	if err != nil {
		l.Error("seed users", "error", err)
		return 1
	}

	chID, err := seedChannel(ctx, pool, wsID, adminID, userIDs)
	if err != nil {
		l.Error("seed channel", "error", err)
		return 1
	}

	created, err := seedMessages(ctx, pool, chID, adminID, userIDs)
	if err != nil {
		l.Error("seed messages", "error", err)
		return 1
	}

	if len(created) > 0 && natsClient != nil {
		publishSeeded(ctx, pool, natsClient, wsID, created, l)
	}

	l.Info(fmt.Sprintf("seed complete: %d demo users, #random channel ready (login demo users with password %q)",
		len(userIDs), demoPassword), "messages_created", len(created))
	return 0
}

func seedUsers(ctx context.Context, pool *pgxpool.Pool, wsID string, l *slog.Logger) (map[string]string, error) {
	hash, err := crypto.HashPassword(demoPassword)
	if err != nil {
		return nil, fmt.Errorf("hash demo password: %w", err)
	}

	demoUsers := []struct{ email, username, name string }{
		{"alice@demo.local", "alice", "Alice Demo"},
		{"bob@demo.local", "bob", "Bob Demo"},
		{"carol@demo.local", "carol", "Carol Demo"},
	}

	userIDs := map[string]string{}
	for _, du := range demoUsers {
		err := database.WithTx(ctx, pool, func(tx pgx.Tx) error {
			var id string
			if err := tx.QueryRow(ctx,
				`INSERT INTO users (id, email, username, full_name, password_hash, is_active)
				 VALUES ($1, $2, $3, $4, $5, true)
				 ON CONFLICT (email) DO UPDATE SET username = EXCLUDED.username
				 RETURNING id`,
				uuid.NewString(), du.email, du.username, du.name, hash,
			).Scan(&id); err != nil {
				return fmt.Errorf("upsert user: %w", err)
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, 'member')
				 ON CONFLICT (workspace_id, user_id) DO NOTHING`, wsID, id); err != nil {
				return fmt.Errorf("add workspace member: %w", err)
			}
			userIDs[du.username] = id
			return nil
		})
		if err != nil {
			l.Warn("seed user", "email", du.email, "error", err)
		}
	}
	return userIDs, nil
}

func seedChannel(ctx context.Context, pool *pgxpool.Pool, wsID, adminID string, userIDs map[string]string) (string, error) {
	var chID string
	err := database.WithTx(ctx, pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO channels (id, workspace_id, name, slug, description, type, creator_id)
			 VALUES ($1, $2, 'random', 'random', 'Non-work banter', 'public', $3)
			 ON CONFLICT (workspace_id, slug) DO UPDATE SET description = EXCLUDED.description
			 RETURNING id`,
			uuid.NewString(), wsID, adminID,
		).Scan(&chID); err != nil {
			return fmt.Errorf("upsert channel: %w", err)
		}

		members := map[string]string{adminID: "admin"}
		for _, id := range userIDs {
			if _, ok := members[id]; !ok {
				members[id] = "member"
			}
		}
		for userID, role := range members {
			if _, err := tx.Exec(ctx,
				`INSERT INTO channel_members (channel_id, user_id, role) VALUES ($1, $2, $3)
				 ON CONFLICT (channel_id, user_id) DO NOTHING`, chID, userID, role); err != nil {
				return fmt.Errorf("add channel member: %w", err)
			}
		}
		return nil
	})
	return chID, err
}

// seedMessages inserts the sample conversation, but only into an empty channel,
// and returns the ids it created so they can be published.
func seedMessages(ctx context.Context, pool *pgxpool.Pool, chID, adminID string, userIDs map[string]string) ([]string, error) {
	var created []string

	err := database.WithTx(ctx, pool, func(tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM messages WHERE channel_id = $1`, chID).Scan(&count); err != nil {
			return fmt.Errorf("count messages: %w", err)
		}
		if count > 0 {
			return nil
		}

		samples := []struct{ user, text string }{
			{"alice", "Hey team! 👋 Welcome to #random."},
			{"bob", "Anyone up for coffee later?"},
			{"carol", "Just shipped the new release 🚀"},
			{"alice", "Nice work @carol!"},
		}

		// A distinct created_at per row: identical timestamps leave the keyset
		// cursor's id tie-break as the only thing ordering the conversation,
		// which renders the demo transcript in an arbitrary order.
		base := time.Now().Add(-time.Duration(len(samples)) * time.Minute)
		for i, s := range samples {
			uid := userIDs[s.user]
			if uid == "" {
				uid = adminID
			}
			id := uuid.NewString()
			if _, err := tx.Exec(ctx,
				`INSERT INTO messages (id, channel_id, user_id, content, content_type, created_at, updated_at)
				 VALUES ($1, $2, $3, $4, 'markdown', $5, $5)`,
				id, chID, uid, s.text, base.Add(time.Duration(i)*time.Minute)); err != nil {
				return fmt.Errorf("insert message: %w", err)
			}
			created = append(created, id)
		}

		if _, err := tx.Exec(ctx, `UPDATE channels SET last_message_at = NOW() WHERE id = $1`, chID); err != nil {
			return fmt.Errorf("bump channel activity: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// publishSeeded emits message.created for every seeded message with the same
// hydrated payload message.Handler publishes, so the search indexer, the
// notifier and the WebSocket relay treat them as ordinary messages.
func publishSeeded(
	ctx context.Context,
	pool *pgxpool.Pool,
	nc *natspkg.Client,
	wsID string,
	ids []string,
	l *slog.Logger,
) {
	repo := message.NewRepository(pool)
	subject := "superops." + wsID + ".message.created"

	for _, id := range ids {
		msg, err := repo.GetByID(ctx, id)
		if err != nil || msg == nil {
			l.Warn("seed: reload message for publish", "id", id, "error", err)
			continue
		}
		if err := repo.Hydrate(ctx, []*message.Message{msg}); err != nil {
			l.Warn("seed: hydrate message", "id", id, "error", err)
			continue
		}

		pubCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = nc.PublishDurable(pubCtx, subject, "message.new:"+msg.ID,
			natspkg.Event{Type: "message.new", Data: msg})
		cancel()
		if err != nil {
			l.Warn("seed: publish message", "id", id, "error", err)
		}
	}
}
