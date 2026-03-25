package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

type Service struct {
	repo   *Repository
	logger *slog.Logger
}

func NewService(repo *Repository, logger *slog.Logger) *Service {
	return &Service{repo: repo, logger: logger}
}

type MessageEvent struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	UserID    string `json:"user_id"`
	Content   string `json:"content"`
}

func (s *Service) HandleMessage(msg *nats.Msg) {
	var envelope struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		return
	}

	if envelope.Type != "message.new" {
		return
	}

	var event MessageEvent
	if err := json.Unmarshal(envelope.Data, &event); err != nil {
		return
	}

	// Check for @mentions in content
	mentions := extractMentions(event.Content)
	for _, username := range mentions {
		n := &Notification{
			ID:     uuid.NewString(),
			UserID: username, // In real impl, resolve username to user ID
			Type:   TypeMention,
			Title:  "New mention",
			Body:   truncate(event.Content, 100),
			Data:   fmt.Sprintf(`{"channel_id":"%s","message_id":"%s"}`, event.ChannelID, event.ID),
		}
		if err := s.repo.Create(context.Background(), n); err != nil {
			s.logger.Warn("notification: create", "error", err)
		}
	}
}

func extractMentions(content string) []string {
	var mentions []string
	words := strings.Fields(content)
	for _, w := range words {
		if strings.HasPrefix(w, "@") && len(w) > 1 {
			mentions = append(mentions, strings.TrimPrefix(w, "@"))
		}
	}
	return mentions
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
