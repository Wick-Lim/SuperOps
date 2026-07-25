package notification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/inbox"
)

// Deliverer is the inbox fan-out. *inbox.Notifier is the implementation; the
// interface is here so this package's tests can observe what it decided to file
// without standing up a database.
//
// It is the ONLY way this package writes a notification. There is deliberately
// no second path: the coalescing, the idempotency gate and the badge all live
// behind Deliver, and a producer that wrote rows itself would be re-deriving
// them.
type Deliverer interface {
	Deliver(ctx context.Context, req inbox.Request) ([]inbox.Delivered, error)
}

type Service struct {
	pool  *pgxpool.Pool
	authz *authz.Checker
	inbox Deliverer

	// logger is carried but not written to from this package any more: every
	// failure here is RETURNED, because the caller is a durable consumer and the
	// return value is its ack decision. It is kept so a future producer-side
	// diagnostic has somewhere to go without changing NewService's signature,
	// and because handler_events_test builds a Service with nothing else.
	logger *slog.Logger
}

// NewService builds the message-domain fan-out.
func NewService(pool *pgxpool.Pool, az *authz.Checker, notifier Deliverer, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{pool: pool, authz: az, inbox: notifier, logger: logger}
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
// kind.
//
// This is the MOST SPECIFIC rung of the preference ladder and it is applied
// here, before inbox.Notifier ever sees the recipient — so a muted channel
// produces no item at all rather than a suppressed one. internal/inbox's
// notification_prefs sit below it and cover kinds rather than channels; see
// inbox.PrefSet.Resolve.
func (p memberPref) wants(kind string) bool {
	if p.muted || p.pref == "none" {
		return false
	}
	if p.pref == "mentions" {
		return kind == KindMention
	}
	return true
}

// fanout carries the per-event state that decides who actually gets notified.
type fanout struct {
	seen    map[string]bool // author + already-notified recipients
	blocked map[string]bool // either direction of a user_blocks row
	prefs   map[string]memberPref
}

func (f *fanout) skip(uid, kind string) bool {
	if uid == "" || f.seen[uid] || f.blocked[uid] {
		return true
	}
	return !f.prefs[uid].wants(kind)
}

// HandleMessage consumes a message.created event and files the directed
// notifications it implies — a DM, a thread reply, a mention.
//
// # What it deliberately does NOT file
//
// An inbox item for every message in a channel. That is the channel unread
// badge's job (internal/channel/unread.go and the unread-fanout durable), it
// already works, and duplicating it here would both double-count the badge and
// create the hot-row failure the plan names: 1000 messages/hour x 500 members is
// 500k updates/hour on rows that all share an index on last_at. Only DIRECTED
// events produce items, which is what lets the two systems be trusted together.
//
// # The error contract
//
// It returns an error because its caller is a durable JetStream consumer and the
// return value is the ack decision. Handlers here used to log and return
// nothing, so a Postgres blip mid-fan-out acked the event and the recipients
// were simply never told — nothing replays these. A plain error means "retry
// me"; a *PermanentError means the event is unprocessable and must be terminated
// instead of redelivered five times.
//
// Retrying is safe because every event id is derived from the event
// (inbox.EventID) and the item upsert is gated on the event insert actually
// having happened.
func (s *Service) HandleMessage(ctx context.Context, msg *nats.Msg) error {
	event, workspaceID, err := decodeMessage(msg)
	if err != nil || event == nil {
		return err
	}

	f, err := s.newFanout(ctx, event.UserID, event.ChannelID)
	if err != nil {
		// Fail closed: without the block list we would route content between two
		// users who have explicitly blocked each other.
		return fmt.Errorf("resolve fan-out state for channel %s: %w", event.ChannelID, err)
	}

	data := map[string]string{"channel_id": event.ChannelID, "message_id": event.ID}

	chType, err := s.channelType(ctx, event.ChannelID)
	if err != nil {
		return err
	}
	if chType == "dm" || chType == "group_dm" {
		members, err := s.channelMemberIDs(ctx, event.ChannelID)
		if err != nil {
			return err
		}
		recipients := make([]string, 0, len(members))
		for _, uid := range members {
			if f.skip(uid, KindDM) {
				continue
			}
			f.seen[uid] = true
			recipients = append(recipients, uid)
		}
		// One Deliver for the whole conversation, not one per member. At a
		// 500-member group DM the loop this replaced was 500 round trips inside
		// cmd/worker's 25-second handler budget.
		return s.deliver(ctx, inbox.Request{
			WorkspaceID: workspaceID,
			Kind:        KindDM,
			ObjectType:  "message",
			ObjectID:    event.ID,
			SubjectType: inbox.SubjectChannel,
			SubjectID:   event.ChannelID,
			ActorID:     event.UserID,
			Title:       "New message",
			Body:        event.Content,
			Data:        data,
			Recipients:  recipients,
		})
	}

	// Thread reply → notify the parent message author.
	if event.ParentID != nil && *event.ParentID != "" {
		author, err := s.messageAuthor(ctx, *event.ParentID)
		if err != nil {
			return err
		}
		if !f.skip(author, KindThreadReply) {
			f.seen[author] = true
			if err := s.deliver(ctx, inbox.Request{
				WorkspaceID: workspaceID,
				Kind:        KindThreadReply,
				ObjectType:  "message",
				ObjectID:    event.ID,
				SubjectType: inbox.SubjectChannel,
				SubjectID:   event.ChannelID,
				ActorID:     event.UserID,
				Title:       "New reply to your thread",
				Body:        event.Content,
				Data:        data,
				Recipients:  []string{author},
			}); err != nil {
				return err
			}
		}
	}

	// @mentions → notify mentioned users that are members of the channel.
	mentioned := make([]string, 0, 4)
	for _, username := range extractMentions(event.Content) {
		uid, err := s.userIDByUsername(ctx, username)
		if err != nil {
			return err
		}
		if f.skip(uid, KindMention) {
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
		mentioned = append(mentioned, uid)
	}
	return s.deliver(ctx, inbox.Request{
		WorkspaceID: workspaceID,
		Kind:        KindMention,
		ObjectType:  "message",
		ObjectID:    event.ID,
		SubjectType: inbox.SubjectChannel,
		SubjectID:   event.ChannelID,
		ActorID:     event.UserID,
		Title:       "You were mentioned",
		Body:        event.Content,
		Data:        data,
		Recipients:  mentioned,
	})
}

// HandleReaction notifies the author of a message someone reacted to. See
// HandleMessage for the error contract.
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
	if f.skip(author, KindReaction) {
		return nil
	}

	return s.deliver(ctx, inbox.Request{
		WorkspaceID: workspaceID,
		Kind:        KindReaction,
		ObjectType:  "message",
		ObjectID:    event.MessageID,
		SubjectType: inbox.SubjectChannel,
		SubjectID:   event.ChannelID,
		ActorID:     event.UserID,
		Title:       "New reaction",
		Body:        event.Emoji,
		Data:        map[string]string{"channel_id": event.ChannelID, "message_id": event.MessageID},
		// The reactor and the emoji are part of the event's identity: two people
		// reacting to the same message are two events, the same person
		// re-reacting with the same emoji is not.
		Discriminator: event.UserID + "\x00" + event.Emoji,
		Recipients:    []string{author},
	})
}

// HandleChannelMemberAdded produces the channel.invited event. See HandleMessage
// for the error contract.
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

	name := event.ChannelName
	if name == "" {
		name, err = s.channelName(ctx, event.ChannelID)
		if err != nil {
			return err
		}
	}

	return s.deliver(ctx, inbox.Request{
		WorkspaceID: workspaceID,
		Kind:        KindInvite,
		ObjectType:  "channel",
		ObjectID:    event.ChannelID,
		SubjectType: inbox.SubjectChannel,
		SubjectID:   event.ChannelID,
		ActorID:     event.ActorID,
		Title:       "You were added to a channel",
		Body:        "#" + name,
		Data:        map[string]string{"channel_id": event.ChannelID},
		// Two different people adding the same person to the same channel are
		// two events; the same person doing it twice is not.
		Discriminator: event.ActorID,
		Recipients:    []string{event.UserID},
	})
}

// deliver hands one request to the inbox, skipping the call entirely when the
// fan-out resolved to nobody.
func (s *Service) deliver(ctx context.Context, req inbox.Request) error {
	if len(req.Recipients) == 0 {
		return nil
	}
	if _, err := s.inbox.Deliver(ctx, req); err != nil {
		return fmt.Errorf("file %s for %d recipient(s): %w", req.Kind, len(req.Recipients), err)
	}
	return nil
}

// decodeMessage validates a message.created envelope and resolves its
// workspace. It returns (nil, "", nil) for an envelope this consumer is not
// interested in.
func decodeMessage(msg *nats.Msg) (*MessageEvent, string, error) {
	var envelope struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		return nil, "", &PermanentError{Reason: "malformed event envelope on " + msg.Subject, Err: err}
	}
	if envelope.Type != "message.new" {
		return nil, "", nil
	}

	var event MessageEvent
	if err := json.Unmarshal(envelope.Data, &event); err != nil {
		return nil, "", &PermanentError{Reason: "malformed message.new payload", Err: err}
	}
	// The message id is required, not just useful: it is what makes every event
	// this fan-out produces identifiable, and therefore what makes a redelivery
	// idempotent. Without it two different messages would derive the same event
	// id and the second would silently vanish.
	if event.ID == "" || event.ChannelID == "" || event.UserID == "" {
		return nil, "", &PermanentError{Reason: "message.new event has no id, channel_id or user_id"}
	}

	workspaceID, err := workspaceFromSubject(msg.Subject)
	if err != nil {
		return nil, "", err
	}
	return &event, workspaceID, nil
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

// workspaceFromSubject pulls the workspace id out of superops.{workspace_id}.*.
//
// A subject without one cannot have come from any publisher in this tree, and
// the realtime push would be addressed to "superops..notification.created" — a
// subject nothing subscribes to. That is a permanently broken event, not a
// transient fault.
func workspaceFromSubject(subject string) (string, error) {
	parts := strings.Split(subject, ".")
	if len(parts) < 2 || parts[1] == "" {
		return "", &PermanentError{Reason: "event subject carries no workspace id: " + subject}
	}
	return parts[1], nil
}
