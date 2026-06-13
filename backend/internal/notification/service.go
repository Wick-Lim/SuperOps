package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
)

type Service struct {
	repo   *Repository
	pool   *pgxpool.Pool
	nats   *natspkg.Client
	logger *slog.Logger
}

func NewService(repo *Repository, pool *pgxpool.Pool, nats *natspkg.Client, logger *slog.Logger) *Service {
	return &Service{repo: repo, pool: pool, nats: nats, logger: logger}
}

type MessageEvent struct {
	ID        string  `json:"id"`
	ChannelID string  `json:"channel_id"`
	UserID    string  `json:"user_id"`
	ParentID  *string `json:"parent_id"`
	Content   string  `json:"content"`
}

// HandleMessage consumes a message.created event and creates the appropriate
// notifications (DM, mention, thread_reply), pushing each to the recipient in
// real time via NATS so the WebSocket relay can deliver it.
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
	if event.ChannelID == "" || event.UserID == "" {
		return
	}

	ctx := context.Background()
	workspaceID := workspaceFromSubject(msg.Subject)

	// author is always excluded; seen dedupes across DM/mention/thread paths.
	seen := map[string]bool{event.UserID: true}

	chType := s.channelType(ctx, event.ChannelID)
	dataJSON := fmt.Sprintf(`{"channel_id":%q,"message_id":%q}`, event.ChannelID, event.ID)

	if chType == "dm" || chType == "group_dm" {
		// Notify every other member of the conversation.
		for _, uid := range s.channelMemberIDs(ctx, event.ChannelID) {
			if seen[uid] {
				continue
			}
			seen[uid] = true
			s.createAndPush(ctx, workspaceID, &Notification{
				ID:     uuid.NewString(),
				UserID: uid,
				Type:   TypeDM,
				Title:  "New message",
				Body:   truncate(event.Content, 140),
				Data:   dataJSON,
			})
		}
		return
	}

	// Thread reply → notify the parent message author.
	if event.ParentID != nil && *event.ParentID != "" {
		if author := s.messageAuthor(ctx, *event.ParentID); author != "" && !seen[author] {
			seen[author] = true
			s.createAndPush(ctx, workspaceID, &Notification{
				ID:     uuid.NewString(),
				UserID: author,
				Type:   TypeThreadReply,
				Title:  "New reply to your thread",
				Body:   truncate(event.Content, 140),
				Data:   dataJSON,
			})
		}
	}

	// @mentions → notify mentioned users that are members of the channel.
	for _, username := range extractMentions(event.Content) {
		uid := s.userIDByUsername(ctx, username)
		if uid == "" || seen[uid] {
			continue
		}
		if !s.isChannelMember(ctx, event.ChannelID, uid) {
			continue
		}
		seen[uid] = true
		s.createAndPush(ctx, workspaceID, &Notification{
			ID:     uuid.NewString(),
			UserID: uid,
			Type:   TypeMention,
			Title:  "You were mentioned",
			Body:   truncate(event.Content, 140),
			Data:   dataJSON,
		})
	}
}

func (s *Service) createAndPush(ctx context.Context, workspaceID string, n *Notification) {
	if err := s.repo.Create(ctx, n); err != nil {
		s.logger.Warn("notification: create", "error", err)
		return
	}
	if s.nats != nil && workspaceID != "" {
		_ = s.nats.Publish("superops."+workspaceID+".notification.created",
			natspkg.Event{Type: "notification.new", Data: n})
	}
}

// --- lookups ---

func (s *Service) channelType(ctx context.Context, channelID string) string {
	var t string
	_ = s.pool.QueryRow(ctx, `SELECT type FROM channels WHERE id = $1`, channelID).Scan(&t)
	return t
}

func (s *Service) channelMemberIDs(ctx context.Context, channelID string) []string {
	rows, err := s.pool.Query(ctx, `SELECT user_id FROM channel_members WHERE channel_id = $1`, channelID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func (s *Service) messageAuthor(ctx context.Context, messageID string) string {
	var author string
	_ = s.pool.QueryRow(ctx, `SELECT user_id FROM messages WHERE id = $1`, messageID).Scan(&author)
	return author
}

func (s *Service) userIDByUsername(ctx context.Context, username string) string {
	var id string
	_ = s.pool.QueryRow(ctx, `SELECT id FROM users WHERE username = $1`, username).Scan(&id)
	return id
}

func (s *Service) isChannelMember(ctx context.Context, channelID, userID string) bool {
	var exists bool
	_ = s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM channel_members WHERE channel_id = $1 AND user_id = $2)`,
		channelID, userID).Scan(&exists)
	return exists
}

// --- helpers ---

func extractMentions(content string) []string {
	var mentions []string
	for _, w := range strings.Fields(content) {
		w = strings.TrimRight(w, ".,!?;:") // strip trailing punctuation
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

func workspaceFromSubject(subject string) string {
	parts := strings.Split(subject, ".")
	if len(parts) >= 2 {
		return parts[1]
	}
	return ""
}
