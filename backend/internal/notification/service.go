package notification

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
)

// bodyRunes bounds the notification preview. It is a rune budget, not a byte
// budget — see truncate.
const bodyRunes = 140

type Service struct {
	repo   *Repository
	pool   *pgxpool.Pool
	authz  *authz.Checker
	nats   *natspkg.Client
	logger *slog.Logger
}

func NewService(repo *Repository, pool *pgxpool.Pool, az *authz.Checker, nats *natspkg.Client, logger *slog.Logger) *Service {
	return &Service{repo: repo, pool: pool, authz: az, nats: nats, logger: logger}
}

type MessageEvent struct {
	ID        string  `json:"id"`
	ChannelID string  `json:"channel_id"`
	UserID    string  `json:"user_id"`
	ParentID  *string `json:"parent_id"`
	Content   string  `json:"content"`
}

// ReactionEvent mirrors the payload message.Handler.AddReaction publishes.
type ReactionEvent struct {
	ChannelID string `json:"channel_id"`
	MessageID string `json:"message_id"`
	UserID    string `json:"user_id"`
	Emoji     string `json:"emoji"`
}

// ChannelMemberEvent is the payload for `channel.member_added`, published when
// someone is added to a channel by another user.
type ChannelMemberEvent struct {
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	UserID      string `json:"user_id"`  // the member who was added
	ActorID     string `json:"actor_id"` // whoever added them
}

// memberPref is a recipient's per-channel delivery preference.
type memberPref struct {
	muted bool
	pref  string
}

// wants reports whether this member should receive a notification of the given
// type. channel_members.muted and notification_pref were previously read by
// nothing at all, so muting a channel changed nothing.
func (p memberPref) wants(t Type) bool {
	if p.muted || p.pref == "none" {
		return false
	}
	if p.pref == "mentions" {
		return t == TypeMention
	}
	return true
}

// fanout carries the per-event state that decides who actually gets notified.
type fanout struct {
	seen    map[string]bool // author + already-notified recipients
	blocked map[string]bool // either direction of a user_blocks row
	prefs   map[string]memberPref
}

func (f *fanout) skip(uid string, t Type) bool {
	if uid == "" || f.seen[uid] || f.blocked[uid] {
		return true
	}
	return !f.prefs[uid].wants(t)
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

	f, err := s.newFanout(ctx, event.UserID, event.ChannelID)
	if err != nil {
		// Fail closed: without the block list we would route content between two
		// users who have explicitly blocked each other.
		s.logger.Warn("notification: resolve fan-out state", "error", err, "channel_id", event.ChannelID)
		return
	}

	data, err := marshalData(map[string]string{"channel_id": event.ChannelID, "message_id": event.ID})
	if err != nil {
		s.logger.Warn("notification: marshal data", "error", err)
		return
	}
	body := truncate(event.Content, bodyRunes)

	chType := s.channelType(ctx, event.ChannelID)
	if chType == "dm" || chType == "group_dm" {
		// Notify every other member of the conversation.
		for _, uid := range s.channelMemberIDs(ctx, event.ChannelID) {
			if f.skip(uid, TypeDM) {
				continue
			}
			f.seen[uid] = true
			s.createAndPush(ctx, workspaceID, &Notification{
				ID:     uuid.NewString(),
				UserID: uid,
				Type:   TypeDM,
				Title:  "New message",
				Body:   body,
				Data:   data,
			})
		}
		return
	}

	// Thread reply → notify the parent message author.
	if event.ParentID != nil && *event.ParentID != "" {
		author := s.messageAuthor(ctx, *event.ParentID)
		if !f.skip(author, TypeThreadReply) {
			f.seen[author] = true
			s.createAndPush(ctx, workspaceID, &Notification{
				ID:     uuid.NewString(),
				UserID: author,
				Type:   TypeThreadReply,
				Title:  "New reply to your thread",
				Body:   body,
				Data:   data,
			})
		}
	}

	// @mentions → notify mentioned users that are members of the channel.
	for _, username := range extractMentions(event.Content) {
		uid := s.userIDByUsername(ctx, username)
		if f.skip(uid, TypeMention) {
			continue
		}
		if !s.isChannelMember(ctx, event.ChannelID, uid) {
			continue
		}
		f.seen[uid] = true
		s.createAndPush(ctx, workspaceID, &Notification{
			ID:     uuid.NewString(),
			UserID: uid,
			Type:   TypeMention,
			Title:  "You were mentioned",
			Body:   body,
			Data:   data,
		})
	}
}

// HandleReaction notifies the author of a message someone reacted to. Reactions
// previously notified nobody.
func (s *Service) HandleReaction(msg *nats.Msg) {
	var envelope struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		return
	}
	if envelope.Type != "reaction.added" {
		return
	}

	var event ReactionEvent
	if err := json.Unmarshal(envelope.Data, &event); err != nil {
		return
	}
	if event.MessageID == "" || event.UserID == "" || event.ChannelID == "" {
		return
	}

	ctx := context.Background()
	author := s.messageAuthor(ctx, event.MessageID)
	if author == "" || author == event.UserID {
		return
	}

	f, err := s.newFanout(ctx, event.UserID, event.ChannelID)
	if err != nil {
		s.logger.Warn("notification: resolve fan-out state", "error", err, "channel_id", event.ChannelID)
		return
	}
	if f.skip(author, TypeSystem) {
		return
	}

	data, err := marshalData(map[string]string{"channel_id": event.ChannelID, "message_id": event.MessageID})
	if err != nil {
		s.logger.Warn("notification: marshal data", "error", err)
		return
	}

	s.createAndPush(ctx, workspaceFromSubject(msg.Subject), &Notification{
		ID:     uuid.NewString(),
		UserID: author,
		Type:   TypeSystem,
		Title:  "New reaction",
		Body:   truncate(event.Emoji, bodyRunes),
		Data:   data,
	})
}

// HandleChannelMemberAdded produces the channel_invite notification. The type
// has existed in the enum since migration 005 and was never emitted by anything.
func (s *Service) HandleChannelMemberAdded(msg *nats.Msg) {
	var envelope struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		return
	}
	if envelope.Type != "channel.member_added" {
		return
	}

	var event ChannelMemberEvent
	if err := json.Unmarshal(envelope.Data, &event); err != nil {
		return
	}
	if err := s.NotifyChannelInvite(context.Background(), workspaceFromSubject(msg.Subject), event); err != nil {
		s.logger.Warn("notification: channel invite", "error", err, "channel_id", event.ChannelID)
	}
}

// NotifyChannelInvite tells a user they were added to a channel by someone
// else. Self-joins and blocked pairs produce nothing.
func (s *Service) NotifyChannelInvite(ctx context.Context, workspaceID string, event ChannelMemberEvent) error {
	if event.ChannelID == "" || event.UserID == "" || event.ActorID == "" || event.UserID == event.ActorID {
		return nil
	}

	blocked, err := s.authz.IsBlocked(ctx, event.ActorID, event.UserID)
	if err != nil {
		return err
	}
	if blocked {
		return nil
	}

	data, err := marshalData(map[string]string{"channel_id": event.ChannelID})
	if err != nil {
		return err
	}

	name := event.ChannelName
	if name == "" {
		name = s.channelName(ctx, event.ChannelID)
	}

	s.createAndPush(ctx, workspaceID, &Notification{
		ID:     uuid.NewString(),
		UserID: event.UserID,
		Type:   TypeChannelInvite,
		Title:  "You were added to a channel",
		Body:   truncate("#"+name, bodyRunes),
		Data:   data,
	})
	return nil
}

func (s *Service) createAndPush(ctx context.Context, workspaceID string, n *Notification) {
	if err := s.repo.Create(ctx, n); err != nil {
		s.logger.Warn("notification: create", "error", err, "user_id", n.UserID, "type", string(n.Type))
		return
	}
	if s.nats != nil && workspaceID != "" {
		_ = s.nats.Publish("superops."+workspaceID+".notification.created",
			natspkg.Event{Type: "notification.new", Data: n})
	}
}

// newFanout resolves the author exclusion, the block list and the channel
// delivery preferences in one place.
func (s *Service) newFanout(ctx context.Context, authorID, channelID string) (*fanout, error) {
	blocked, err := s.authz.BlockedUserIDs(ctx, authorID)
	if err != nil {
		return nil, err
	}
	prefs, err := s.channelPrefs(ctx, channelID)
	if err != nil {
		return nil, err
	}
	return &fanout{
		seen:    map[string]bool{authorID: true},
		blocked: blocked,
		prefs:   prefs,
	}, nil
}

// --- lookups ---

func (s *Service) channelType(ctx context.Context, channelID string) string {
	var t string
	_ = s.pool.QueryRow(ctx, `SELECT type FROM channels WHERE id = $1`, channelID).Scan(&t)
	return t
}

func (s *Service) channelName(ctx context.Context, channelID string) string {
	var n string
	_ = s.pool.QueryRow(ctx, `SELECT name FROM channels WHERE id = $1`, channelID).Scan(&n)
	return n
}

func (s *Service) channelPrefs(ctx context.Context, channelID string) (map[string]memberPref, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT user_id, muted, notification_pref FROM channel_members WHERE channel_id = $1`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	prefs := map[string]memberPref{}
	for rows.Next() {
		var uid string
		var p memberPref
		if err := rows.Scan(&uid, &p.muted, &p.pref); err != nil {
			return nil, err
		}
		prefs[uid] = p
	}
	return prefs, rows.Err()
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

// marshalData renders the notification payload as JSON. It used to be built
// with fmt.Sprintf and %q, which is Go quoting, not JSON escaping — it only
// happened to survive because every interpolated value was a UUID.
func marshalData(v map[string]string) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// truncate caps a preview at maxRunes runes.
//
// It must not slice by bytes: this deployment is Korean-language, so almost
// every message over 140 bytes was being cut mid-rune, producing invalid UTF-8
// that Postgres rejects on INSERT. createAndPush only logs that failure, so the
// recipient silently received no notification at all.
func truncate(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	count := 0
	for i := range s {
		if count == maxRunes {
			return s[:i] + "..."
		}
		count++
	}
	return s
}

func workspaceFromSubject(subject string) string {
	parts := strings.Split(subject, ".")
	if len(parts) >= 2 {
		return parts[1]
	}
	return ""
}
