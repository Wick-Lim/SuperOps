package main

import (
	"context"
	"fmt"
	"log"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/app"
	"github.com/Wick-Lim/SuperOps/backend/pkg/crypto"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
	"github.com/Wick-Lim/SuperOps/backend/pkg/logger"
)

// Seeds demo data (idempotent) into the admin's workspace: a few users, a
// #random channel, and some sample messages. Safe to run repeatedly.
func main() {
	cfg, err := app.LoadConfig()
	if err != nil {
		log.Fatal("load config: ", err)
	}

	l := logger.New(cfg.LogLevel)
	ctx := context.Background()

	pool, err := database.NewPool(ctx, database.Config{
		DSN:      cfg.DB.DSN(),
		MaxConns: cfg.DB.MaxConns,
		MinConns: cfg.DB.MinConns,
	}, l)
	if err != nil {
		log.Fatal("database: ", err)
	}
	defer pool.Close()

	// Locate the seeded admin + their workspace.
	var adminID, wsID string
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE email = $1`, cfg.Admin.Email).Scan(&adminID); err != nil {
		log.Fatal("admin user not found — start the server once first: ", err)
	}
	if err := pool.QueryRow(ctx, `SELECT workspace_id FROM workspace_members WHERE user_id = $1 LIMIT 1`, adminID).Scan(&wsID); err != nil {
		log.Fatal("admin workspace not found: ", err)
	}

	hash, _ := crypto.HashPassword("demo_password_123")
	demoUsers := []struct{ email, username, name string }{
		{"alice@demo.local", "alice", "Alice Demo"},
		{"bob@demo.local", "bob", "Bob Demo"},
		{"carol@demo.local", "carol", "Carol Demo"},
	}

	userIDs := map[string]string{}
	for _, du := range demoUsers {
		var id string
		err := pool.QueryRow(ctx,
			`INSERT INTO users (id, email, username, full_name, password_hash, is_active)
			 VALUES ($1, $2, $3, $4, $5, true)
			 ON CONFLICT (email) DO UPDATE SET username = EXCLUDED.username
			 RETURNING id`,
			uuid.NewString(), du.email, du.username, du.name, hash,
		).Scan(&id)
		if err != nil {
			l.Warn("seed user", "email", du.email, "error", err)
			continue
		}
		userIDs[du.username] = id
		pool.Exec(ctx, `INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, 'member') ON CONFLICT DO NOTHING`, wsID, id)
	}

	// A #random channel with everyone in it.
	var chID string
	err = pool.QueryRow(ctx,
		`INSERT INTO channels (id, workspace_id, name, slug, description, type, creator_id)
		 VALUES ($1, $2, 'random', 'random', 'Non-work banter', 'public', $3)
		 ON CONFLICT (workspace_id, slug) DO UPDATE SET description = EXCLUDED.description
		 RETURNING id`,
		uuid.NewString(), wsID, adminID,
	).Scan(&chID)
	if err != nil {
		log.Fatal("seed channel: ", err)
	}
	addMember(ctx, pool, chID, adminID, "admin")
	for _, id := range userIDs {
		addMember(ctx, pool, chID, id, "member")
	}

	// Sample messages (only if the channel has none yet).
	var count int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM messages WHERE channel_id = $1`, chID).Scan(&count)
	if count == 0 {
		samples := []struct{ user, text string }{
			{"alice", "Hey team! 👋 Welcome to #random."},
			{"bob", "Anyone up for coffee later?"},
			{"carol", "Just shipped the new release 🚀"},
			{"alice", "Nice work @carol!"},
		}
		for _, s := range samples {
			uid := userIDs[s.user]
			if uid == "" {
				uid = adminID
			}
			pool.Exec(ctx,
				`INSERT INTO messages (id, channel_id, user_id, content, content_type) VALUES ($1, $2, $3, $4, 'markdown')`,
				uuid.NewString(), chID, uid, s.text)
		}
		pool.Exec(ctx, `UPDATE channels SET last_message_at = NOW() WHERE id = $1`, chID)
	}

	l.Info(fmt.Sprintf("seed complete: %d demo users, #random channel ready (login demo users with password 'demo_password_123')", len(userIDs)))
}

func addMember(ctx context.Context, pool *pgxpool.Pool, channelID, userID, role string) {
	pool.Exec(ctx,
		`INSERT INTO channel_members (channel_id, user_id, role) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		channelID, userID, role)
}
