package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nats-io/nats.go"

	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
)

// Wire/outbound event types this fan-out reads and writes. They mirror the
// internal/ws Type* constants and internal/message's copies of them; a domain
// package cannot import internal/ws without a cycle, so the names are
// duplicated here exactly as they are there.
const (
	evtMessageNew     = "message.new"
	evtMessageDeleted = "message.deleted"
	evtUnreadUpdate   = "unread.update"
)

// PermanentError marks an event that redelivery can never fix: a payload that
// does not parse, a subject with no workspace id.
//
// The worker's durable consumer discovers it structurally — it looks for an
// error implementing `Permanent() bool` rather than importing this package —
// and terminates the JetStream message with a reason instead of Nak-ing it back
// onto the stream until MaxDeliver runs out.
type PermanentError struct {
	Reason string
	Err    error
}

func (e *PermanentError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent identifies this error to the consumer's ack policy. The method, not
// the concrete type, is the contract.
func (e *PermanentError) Permanent() bool { return true }

// unreadMessageEvent is the subset of the message.new / message.deleted
// payloads this fan-out needs. Both are published by internal/message (and, for
// message.new, by cmd/worker's scheduled-message promotion, which republishes a
// fully hydrated message.Message — a superset of these fields).
type unreadMessageEvent struct {
	ID        string  `json:"id"`
	ChannelID string  `json:"channel_id"`
	UserID    string  `json:"user_id"`
	ParentID  *string `json:"parent_id"`
}

// UnreadFanout recomputes and republishes the per-member unread badge whenever
// a message enters or leaves a channel's timeline.
//
// Why it lives behind a JetStream consumer rather than in the REST send path:
// the badge is one query per (channel, member), so publishing it from
// message.Handler.Send would put the whole membership on the synchronous
// request — a 500-member channel would turn every POST /messages into a
// fan-out the sender waits for. Here it is off the request's critical path, it
// inherits the consumer's retry/ack semantics, and it costs nothing to cover
// the other two publishers of the same events: cmd/worker's scheduled-message
// promotion emits message.created like any other send, and message deletion
// emits message.deleted, so both move the badge with no extra code.
//
// Who gets one, and why it is not the notifier's recipient list: every member
// except the author, with no mute, notification_pref or block filtering. Those
// three suppress a *notification* — a buzz — and a muted channel still
// accumulates unread messages; it just should not interrupt anyone. The
// governing constraint is that the pushed count and the count
// GET /api/v1/workspaces/{id}/channels computes must never disagree, and that
// query (Repository.ListByWorkspaceAndUser) filters on none of them. Skipping a
// muted or blocked recipient here would not hide anything from them — the
// payload carries a channel id and an integer, both of which they can already
// read — it would only leave their badge stale-low until their next refresh.
type UnreadFanout struct {
	repo *Repository
	nats *natspkg.Client
	log  *slog.Logger
}

func NewUnreadFanout(repo *Repository, nc *natspkg.Client, log *slog.Logger) *UnreadFanout {
	return &UnreadFanout{repo: repo, nats: nc, log: log}
}

// HandleMessage consumes one message.* event and republishes the badge of every
// member whose count it could have changed.
//
// It returns an error because its caller is a durable JetStream consumer and
// the return value is the ack decision: a plain error means "redeliver me", a
// *PermanentError means the event is unprocessable and must be terminated. A
// redelivery is harmless — the event carries no increment, only an absolute
// count read fresh from Postgres, so re-running it can only republish the same
// (or a newer) truth.
func (f *UnreadFanout) HandleMessage(ctx context.Context, msg *nats.Msg) error {
	var envelope struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		return &PermanentError{Reason: "malformed event envelope on " + msg.Subject, Err: err}
	}

	switch envelope.Type {
	case evtMessageNew, evtMessageDeleted:
	default:
		// The consumer's filter is superops.*.message.*, which also carries
		// message.updated — an edit, a pin, a reply_count bump. None of those
		// move a badge. Nothing to do, and nothing wrong.
		return nil
	}

	var event unreadMessageEvent
	if err := json.Unmarshal(envelope.Data, &event); err != nil {
		return &PermanentError{Reason: "malformed " + envelope.Type + " payload", Err: err}
	}
	if event.ChannelID == "" {
		return &PermanentError{Reason: envelope.Type + " event carries no channel_id"}
	}

	// Thread replies are invisible to the badge: every unread query counts only
	// top-level messages (parent_id IS NULL), matching the timeline the client
	// renders. Fanning out for one would publish N unchanged counts.
	if event.ParentID != nil && *event.ParentID != "" {
		return nil
	}

	workspaceID, err := workspaceFromSubject(msg.Subject)
	if err != nil {
		return err
	}

	// The author's own badge is unaffected by their own message (the count
	// excludes m.user_id = cm.user_id), so they are excluded from the send
	// fan-out. A deletion is different: a moderator deleting somebody else's
	// message lowers their own badge too, and the event does not say who
	// deleted it — so nobody is excluded there.
	exclude := ""
	if envelope.Type == evtMessageNew {
		if event.UserID == "" {
			return &PermanentError{Reason: "message.new event carries no user_id"}
		}
		exclude = event.UserID
	}

	counts, err := f.repo.UnreadCounts(ctx, event.ChannelID, exclude)
	if err != nil {
		// Retry: without the counts there is nothing to publish, and a badge
		// that never rises is the bug this exists to fix.
		return fmt.Errorf("unread fan-out for channel %s: %w", event.ChannelID, err)
	}

	// One failed recipient must not cost the rest of the channel its badge, so
	// every publish is attempted and the first error is reported afterwards.
	var firstErr error
	for _, mu := range counts {
		if err := f.publish(workspaceID, mu); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return fmt.Errorf("publish unread update for channel %s: %w", event.ChannelID, firstErr)
	}
	return nil
}

// publish emits one recipient's badge on the domain plane, byte-for-byte the
// event ws.Hub.PublishUnreadUpdate produces from the mark-read path: same
// subject, same envelope type, same payload builder. The relay routes it on the
// top-level "user_id" and delivers it to that user's sockets on whichever
// replica they are connected to.
//
// Core NATS, not JetStream, is deliberate and matches the other publisher: the
// badge is a derived value with an authoritative source (GET /channels), so a
// push lost to a momentary disconnect is repaired by the client's next fetch,
// and persisting a stream message per recipient per message is a large amount
// of storage for a number that is stale the moment it is written.
func (f *UnreadFanout) publish(workspaceID string, mu MemberUnread) error {
	if f.nats == nil {
		return nil
	}
	unread := mu.Unread
	subject := "superops." + workspaceID + "." + evtUnreadUpdate
	if err := f.nats.Publish(subject, natspkg.Event{
		Type: evtUnreadUpdate,
		Data: unreadUpdatePayload(mu.UserID, &unread),
	}); err != nil {
		f.logWarn("publish unread update", "subject", subject, "user_id", mu.UserID, "error", err)
		return err
	}
	return nil
}

func (f *UnreadFanout) logWarn(msg string, args ...any) {
	log := f.log
	if log == nil {
		log = slog.Default()
	}
	log.Warn(msg, args...)
}

// workspaceFromSubject pulls the workspace id out of superops.{workspace_id}.*.
// A subject without one cannot have come from any publisher in this tree, and
// the badge would be addressed to "superops..unread.update" — a subject nothing
// subscribes to. That is a permanently broken event, not a transient fault.
func workspaceFromSubject(subject string) (string, error) {
	parts := strings.Split(subject, ".")
	if len(parts) < 2 || parts[1] == "" {
		return "", &PermanentError{Reason: "event subject carries no workspace id: " + subject}
	}
	return parts[1], nil
}
