package issue

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
)

const publishTimeout = 3 * time.Second

const (
	ActionCreated = "issue.created"
	ActionUpdated = "issue.updated"
	ActionDeleted = "issue.deleted"
)

// Publisher emits issue domain events for search indexing and automation.
// A nil *Publisher is a working no-op.
type Publisher struct {
	nats   *natspkg.Client
	logger *slog.Logger
}

func NewPublisher(nc *natspkg.Client, logger *slog.Logger) *Publisher {
	if nc == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Publisher{nats: nc, logger: logger}
}

type issueEvent struct {
	ID          string `json:"id"`
	Key         string `json:"key"`
	Title       string `json:"title"`
	Description string `json:"description"`
	UserID      string `json:"user_id,omitempty"`
	CreatedAt   string `json:"created_at"`
	IsDeleted   bool   `json:"is_deleted,omitempty"`
}

func (p *Publisher) PublishIssue(ctx context.Context, action string, i *Issue) {
	if p == nil || i == nil || i.ID == "" || i.WorkspaceID == "" {
		return
	}
	userID := ""
	if i.CreatedBy != nil {
		userID = *i.CreatedBy
	}
	payload := issueEvent{
		ID:          i.ID,
		Key:         i.Key,
		Title:       i.Title,
		Description: i.Description,
		UserID:      userID,
		CreatedAt:   i.CreatedAt.UTC().Format(time.RFC3339Nano),
		IsDeleted:   action == ActionDeleted || i.ArchivedAt != nil,
	}

	subject := fmt.Sprintf("superops.%s.%s", i.WorkspaceID, action)
	revision := strconv.FormatInt(i.UpdatedAt.UTC().UnixNano(), 10)
	msgID := fmt.Sprintf("%s:%s:%s", action, i.ID, revision)

	pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout)
	defer cancel()

	if err := p.nats.PublishDurable(pubCtx, subject, msgID, natspkg.Event{
		Type: action,
		Data: payload,
	}); err != nil {
		p.logger.Warn("publish issue event; the row is committed and the index will "+
			"converge on the next reindex", "subject", subject, "issue_id", i.ID, "error", err)
	}
}
