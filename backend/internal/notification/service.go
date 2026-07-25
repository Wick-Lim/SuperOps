package notification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
)

// bodyRunes bounds the notification preview. It is a rune budget, not a byte
// budget — see truncate.
const bodyRunes = 140

// notificationNamespace seeds the deterministic notification ids below. Any
// fixed uuid does; this one must simply never change, or the first fan-out
// after the change would duplicate every notification it re-derives.
var notificationNamespace = uuid.MustParse("3d0f5e2a-6c1b-4f7e-9a1d-2b8c5f0e7a41")

// notificationID derives a stable id from what the notification is *about*.
//
// The worker consumes these events from a durable JetStream consumer with
// explicit acks, which is at-least-once: a fan-out that fails halfway through
// (or simply outruns AckWait) is redelivered, and with random ids every
// recipient already processed would get a second row. Deriving the id instead
// makes the INSERT an ON CONFLICT DO NOTHING no-op the second time round, which
// is what allows the handler to nak at all.
//
// The parts are length-prefixed rather than merely delimited, so no choice of
// separator can make two different tuples hash to the same id.
func notificationID(t Type, userID, subject string) string {
	key := fmt.Sprintf("%d:%s|%d:%s|%d:%s", len(t), t, len(userID), userID, len(subject), subject)
	return uuid.NewSHA1(notificationNamespace, []byte(key)).String()
}

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
//
// It returns an error because its caller is a durable JetStream consumer and
// the return value is the ack decision. Handlers here used to log and return
// nothing, so a Postgres blip mid-fan-out acked the event and the recipients
// were simply never told — nothing replays these events. A plain error means
// "retry me"; a *PermanentError means the event is unprocessable and must be
// terminated instead of redelivered five times.
//
// Retrying is safe because every notification id is derived from the event (see
// notificationID) and Repository.Create is an upsert-or-nothing.
func (s *Service) HandleMessage(ctx context.Context, msg *nats.Msg) error {
	var envelope struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		return &PermanentError{Reason: "malformed event envelope on " + msg.Subject, Err: err}
	}
	if envelope.Type != "message.new" {
		return nil
	}

	var event MessageEvent
	if err := json.Unmarshal(envelope.Data, &event); err != nil {
		return &PermanentError{Reason: "malformed message.new payload", Err: err}
	}
	// The message id is required, not just useful: it is what makes every
	// notification this fan-out produces identifiable, and therefore what makes
	// a redelivery idempotent. Without it two different messages would derive
	// the same id and the second would silently vanish.
	if event.ID == "" || event.ChannelID == "" || event.UserID == "" {
		return &PermanentError{Reason: "message.new event has no id, channel_id or user_id"}
	}

	workspaceID, err := workspaceFromSubject(msg.Subject)
	if err != nil {
		return err
	}

	f, err := s.newFanout(ctx, event.UserID, event.ChannelID)
	if err != nil {
		// Fail closed: without the block list we would route content between two
		// users who have explicitly blocked each other.
		return fmt.Errorf("resolve fan-out state for channel %s: %w", event.ChannelID, err)
	}

	data, err := marshalData(map[string]string{"channel_id": event.ChannelID, "message_id": event.ID})
	if err != nil {
		return &PermanentError{Reason: "notification payload is not encodable", Err: err}
	}
	body := truncate(event.Content, bodyRunes)

	chType, err := s.channelType(ctx, event.ChannelID)
	if err != nil {
		return err
	}
	if chType == "dm" || chType == "group_dm" {
		// Notify every other member of the conversation.
		members, err := s.channelMemberIDs(ctx, event.ChannelID)
		if err != nil {
			return err
		}
		for _, uid := range members {
			if f.skip(uid, TypeDM) {
				continue
			}
			f.seen[uid] = true
			if err := s.createAndPush(ctx, workspaceID, &Notification{
				ID:     notificationID(TypeDM, uid, event.ID),
				UserID: uid,
				Type:   TypeDM,
				Title:  "New message",
				Body:   body,
				Data:   data,
			}); err != nil {
				return err
			}
		}
		return nil
	}

	// Thread reply → notify the parent message author.
	if event.ParentID != nil && *event.ParentID != "" {
		author, err := s.messageAuthor(ctx, *event.ParentID)
		if err != nil {
			return err
		}
		if !f.skip(author, TypeThreadReply) {
			f.seen[author] = true
			if err := s.createAndPush(ctx, workspaceID, &Notification{
				ID:     notificationID(TypeThreadReply, author, event.ID),
				UserID: author,
				Type:   TypeThreadReply,
				Title:  "New reply to your thread",
				Body:   body,
				Data:   data,
			}); err != nil {
				return err
			}
		}
	}

	// @mentions → notify mentioned users that are members of the channel.
	for _, username := range extractMentions(event.Content) {
		uid, err := s.userIDByUsername(ctx, username)
		if err != nil {
			return err
		}
		if f.skip(uid, TypeMention) {
			continue
		}
		member, err := s.isChannelMember(ctx, event.ChannelID, uid)
		if err != nil {
			return err
		}
		if !member {
			continue
		}
		f.seen[uid] = true
		if err := s.createAndPush(ctx, workspaceID, &Notification{
			ID:     notificationID(TypeMention, uid, event.ID),
			UserID: uid,
			Type:   TypeMention,
			Title:  "You were mentioned",
			Body:   body,
			Data:   data,
		}); err != nil {
			return err
		}
	}
	return nil
}

// HandleReaction notifies the author of a message someone reacted to. Reactions
// previously notified nobody. See HandleMessage for the error contract.
func (s *Service) HandleReaction(ctx context.Context, msg *nats.Msg) error {
	var envelope struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		return &PermanentError{Reason: "malformed event envelope on " + msg.Subject, Err: err}
	}
	if envelope.Type != "reaction.added" {
		return nil
	}

	var event ReactionEvent
	if err := json.Unmarshal(envelope.Data, &event); err != nil {
		return &PermanentError{Reason: "malformed reaction.added payload", Err: err}
	}
	if event.MessageID == "" || event.UserID == "" || event.ChannelID == "" {
		return &PermanentError{Reason: "reaction.added event has no message_id, user_id or channel_id"}
	}

	workspaceID, err := workspaceFromSubject(msg.Subject)
	if err != nil {
		return err
	}

	author, err := s.messageAuthor(ctx, event.MessageID)
	if err != nil {
		return err
	}
	if author == "" || author == event.UserID {
		return nil
	}

	f, err := s.newFanout(ctx, event.UserID, event.ChannelID)
	if err != nil {
		return fmt.Errorf("resolve fan-out state for channel %s: %w", event.ChannelID, err)
	}
	if f.skip(author, TypeSystem) {
		return nil
	}

	data, err := marshalData(map[string]string{"channel_id": event.ChannelID, "message_id": event.MessageID})
	if err != nil {
		return &PermanentError{Reason: "notification payload is not encodable", Err: err}
	}

	// The reactor and the emoji are part of the identity: two people reacting to
	// the same message are two notifications, the same person re-reacting with
	// the same emoji is not.
	return s.createAndPush(ctx, workspaceID, &Notification{
		ID:     notificationID(TypeSystem, author, event.MessageID+"\x00"+event.UserID+"\x00"+event.Emoji),
		UserID: author,
		Type:   TypeSystem,
		Title:  "New reaction",
		Body:   truncate(event.Emoji, bodyRunes),
		Data:   data,
	})
}

// HandleChannelMemberAdded produces the channel_invite notification. The type
// has existed in the enum since migration 005 and was never emitted by anything.
// See HandleMessage for the error contract.
func (s *Service) HandleChannelMemberAdded(ctx context.Context, msg *nats.Msg) error {
	var envelope struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		return &PermanentError{Reason: "malformed event envelope on " + msg.Subject, Err: err}
	}
	if envelope.Type != "channel.member_added" {
		return nil
	}

	var event ChannelMemberEvent
	if err := json.Unmarshal(envelope.Data, &event); err != nil {
		return &PermanentError{Reason: "malformed channel.member_added payload", Err: err}
	}

	workspaceID, err := workspaceFromSubject(msg.Subject)
	if err != nil {
		return err
	}
	if err := s.NotifyChannelInvite(ctx, workspaceID, event); err != nil {
		return fmt.Errorf("channel invite for %s: %w", event.ChannelID, err)
	}
	return nil
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
		name, err = s.channelName(ctx, event.ChannelID)
		if err != nil {
			return err
		}
	}

	return s.createAndPush(ctx, workspaceID, &Notification{
		ID:     notificationID(TypeChannelInvite, event.UserID, event.ChannelID+"\x00"+event.ActorID),
		UserID: event.UserID,
		Type:   TypeChannelInvite,
		Title:  "You were added to a channel",
		Body:   truncate("#"+name, bodyRunes),
		Data:   data,
	})
}

// createAndPush persists a notification and pushes it to the recipient.
//
// A failed INSERT is returned, not logged and shrugged off: the caller is a
// durable consumer that decides whether to ack on the strength of it. A failed
// *push* is only logged — the row is committed, the recipient will see it on
// their next fetch, and naking the event to retry a fire-and-forget publish
// would be a worse trade.
func (s *Service) createAndPush(ctx context.Context, workspaceID string, n *Notification) error {
	if err := s.repo.Create(ctx, n); err != nil {
		return fmt.Errorf("create %s notification for %s: %w", n.Type, n.UserID, err)
	}
	if s.nats != nil && workspaceID != "" {
		if err := s.nats.Publish("superops."+workspaceID+".notification.created",
			natspkg.Event{Type: "notification.new", Data: n}); err != nil {
			s.logger.Warn("notification: push", "error", err, "user_id", n.UserID, "type", string(n.Type))
		}
	}
	return nil
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
//
// Every one of these used to discard its error, which turned a database blip
// into a silently wrong fan-out: an unreadable channels row made a DM look like
// a regular channel, an unreadable messages row made a thread reply look like
// it had no parent. They now separate "no such row" (a legitimately empty
// answer) from "could not ask" (retry the whole event).

func (s *Service) channelType(ctx context.Context, channelID string) (string, error) {
	var t string
	err := s.pool.QueryRow(ctx, `SELECT type FROM channels WHERE id = $1`, channelID).Scan(&t)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("load channel type %s: %w", channelID, err)
	}
	return t, nil
}

func (s *Service) channelName(ctx context.Context, channelID string) (string, error) {
	var n string
	err := s.pool.QueryRow(ctx, `SELECT name FROM channels WHERE id = $1`, channelID).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("load channel name %s: %w", channelID, err)
	}
	return n, nil
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

func (s *Service) channelMemberIDs(ctx context.Context, channelID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT user_id FROM channel_members WHERE channel_id = $1`, channelID)
	if err != nil {
		return nil, fmt.Errorf("list channel members %s: %w", channelID, err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan channel member: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list channel members %s: %w", channelID, err)
	}
	return ids, nil
}

func (s *Service) messageAuthor(ctx context.Context, messageID string) (string, error) {
	var author string
	err := s.pool.QueryRow(ctx, `SELECT user_id FROM messages WHERE id = $1`, messageID).Scan(&author)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("load message author %s: %w", messageID, err)
	}
	return author, nil
}

func (s *Service) userIDByUsername(ctx context.Context, username string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id FROM users WHERE username = $1`, username).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve username: %w", err)
	}
	return id, nil
}

func (s *Service) isChannelMember(ctx context.Context, channelID, userID string) (bool, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM channel_members WHERE channel_id = $1 AND user_id = $2)`,
		channelID, userID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check channel membership: %w", err)
	}
	return exists, nil
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

// workspaceFromSubject pulls the workspace id out of superops.{workspace_id}.*.
//
// A subject without one cannot have come from any publisher in this tree, and
// the realtime push would be addressed to "superops..notification.created" —
// a subject nothing subscribes to. That is a permanently broken event, not a
// transient fault.
func workspaceFromSubject(subject string) (string, error) {
	parts := strings.Split(subject, ".")
	if len(parts) < 2 || parts[1] == "" {
		return "", &PermanentError{Reason: "event subject carries no workspace id: " + subject}
	}
	return parts[1], nil
}
