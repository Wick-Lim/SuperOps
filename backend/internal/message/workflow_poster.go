package message

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
)

// WorkflowPoster posts a message on a workflow's behalf.
//
// IT PUBLISHES THE SAME EVENT THE REST PATH PUBLISHES, and that is the whole
// reason this type exists rather than a bare INSERT. The incoming-webhook
// handler learned this the hard way — its comment records it — and the failure
// is the same here: a message written without the event reaches no connected
// client, is never indexed for search, and notifies nobody even on an @mention.
// It exists in the database and nowhere a person would look.
//
// The message is attributed to the WORKFLOW'S OWNER. Not to a synthetic bot,
// because the product has no bot principal and inventing one would give it a
// row in `users` that nothing else understands; and not to whoever triggered
// the event, because they did not write it. The owner is who authorized the
// post — the same principal internal/workflow checked the capability against —
// so attribution and authorization agree.
type WorkflowPoster struct {
	pool   *pgxpool.Pool
	nats   *natspkg.Client
	logger *slog.Logger
}

func NewWorkflowPoster(pool *pgxpool.Pool, nc *natspkg.Client, logger *slog.Logger) *WorkflowPoster {
	if logger == nil {
		logger = slog.Default()
	}
	return &WorkflowPoster{pool: pool, nats: nc, logger: logger}
}

// posted is the event payload, matching what the REST send path emits so the
// indexer, the notifier and the relay all decode it unchanged.
type posted struct {
	ID          string    `json:"id"`
	ChannelID   string    `json:"channel_id"`
	UserID      string    `json:"user_id"`
	Content     string    `json:"content"`
	ContentType string    `json:"content_type"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// CreateAs writes the message and announces it.
//
// The caller has already checked that userID may write to the channel. This
// does NOT re-check: doing so would be a second authorization with a second
// answer, and the one that matters is the one internal/workflow made against
// the owner. What it does check is that the channel belongs to the workspace
// the run is scoped to — a workflow whose config named a channel in another
// tenant must not post there, and that is a tenancy fact rather than a
// capability one.
func (p *WorkflowPoster) CreateAs(ctx context.Context, workspaceID, channelID, userID, body string) (string, error) {
	if p == nil || p.pool == nil {
		return "", fmt.Errorf("message: no database, so nothing can be posted")
	}

	msg := posted{
		ID:        uuid.NewString(),
		ChannelID: channelID,
		UserID:    userID,
		Content:   body,
		// "system", as the incoming webhook uses: it is not a person typing,
		// and a client that renders it as one would be lying about who is in
		// the conversation.
		ContentType: "system",
	}

	err := database.WithTx(ctx, p.pool, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM channels WHERE id = $1 AND workspace_id = $2)`,
			channelID, workspaceID).Scan(&ok); err != nil {
			return fmt.Errorf("check channel tenancy: %w", err)
		}
		if !ok {
			return fmt.Errorf("channel %s is not in workspace %s", channelID, workspaceID)
		}

		if err := tx.QueryRow(ctx,
			`INSERT INTO messages (id, channel_id, user_id, content, content_type)
			 VALUES ($1, $2, $3, $4, 'system')
			 RETURNING created_at, updated_at`,
			msg.ID, msg.ChannelID, msg.UserID, msg.Content,
		).Scan(&msg.CreatedAt, &msg.UpdatedAt); err != nil {
			return fmt.Errorf("insert message: %w", err)
		}
		_, err := tx.Exec(ctx,
			`UPDATE channels SET last_message_at = $2 WHERE id = $1`, channelID, msg.CreatedAt)
		return err
	})
	if err != nil {
		return "", err
	}

	// Post-commit and best effort. A published event for a message that rolled
	// back would be worse than a missing one — clients would render something
	// the database does not have.
	if p.nats != nil && workspaceID != "" {
		if err := p.nats.Publish("superops."+workspaceID+".message.created",
			natspkg.Event{Type: "message.new", Data: msg}); err != nil {
			// Warn, not fail: the message is committed, and failing here would
			// make the workflow retry a post that already happened. The effects
			// table would prevent the duplicate, but the run would be marked
			// failed for something that succeeded.
			p.logger.Warn("workflow message posted but not announced",
				"message_id", msg.ID, "error", err)
		}
	}
	return msg.ID, nil
}
