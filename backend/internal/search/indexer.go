package search

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

type MessageEvent struct {
	ID          string `json:"id"`
	ChannelID   string `json:"channel_id"`
	UserID      string `json:"user_id"`
	Content     string `json:"content"`
	CreatedAt   string `json:"created_at"`
}

type Indexer struct {
	service *Service
	logger  *slog.Logger
}

func NewIndexer(service *Service, logger *slog.Logger) *Indexer {
	return &Indexer{service: service, logger: logger}
}

func (idx *Indexer) HandleMessage(msg *nats.Msg) {
	var envelope struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		idx.logger.Warn("indexer: unmarshal envelope", "error", err)
		return
	}

	if envelope.Type != "message.new" {
		return
	}

	var event MessageEvent
	if err := json.Unmarshal(envelope.Data, &event); err != nil {
		idx.logger.Warn("indexer: unmarshal message", "error", err)
		return
	}

	// Extract workspace ID from NATS subject: superops.{workspace_id}.message.created
	parts := splitSubject(msg.Subject)
	workspaceID := ""
	if len(parts) >= 2 {
		workspaceID = parts[1]
	}

	t, _ := time.Parse(time.RFC3339Nano, event.CreatedAt)
	doc := MessageDoc{
		ID:          event.ID,
		ChannelID:   event.ChannelID,
		WorkspaceID: workspaceID,
		UserID:      event.UserID,
		Content:     event.Content,
		CreatedAt:   t.Unix(),
	}

	if err := idx.service.IndexMessage(doc); err != nil {
		idx.logger.Warn("indexer: index message", "error", err, "id", event.ID)
	} else {
		idx.logger.Debug("indexed message", "id", event.ID)
	}
}

func splitSubject(subject string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(subject); i++ {
		if subject[i] == '.' {
			parts = append(parts, subject[start:i])
			start = i + 1
		}
	}
	parts = append(parts, subject[start:])
	return parts
}
