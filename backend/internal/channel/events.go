package channel

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
)

// Publisher is the subset of *ws.Hub these handlers use. It is an interface so
// this package does not depend on the WebSocket implementation and so the unit
// tests can assert on the payloads without a hub — the same reason
// internal/message keeps its own copy of the wire event names.
//
// Every method is best effort: the row is already committed by the time one is
// called, so a realtime event that cannot be delivered must never turn into a
// failed request.
type Publisher interface {
	PublishChannelCreated(workspaceID string, payload interface{})
	PublishChannelUpdated(workspaceID string, payload interface{})
	PublishMemberJoined(workspaceID string, payload interface{})
	PublishMemberLeft(workspaceID string, payload interface{})
	PublishUnreadUpdate(workspaceID string, payload interface{})
	RevokeChannelSubscription(userID, channelID string)
	RevokeChannelForAll(channelID string)
}

// evtChannelMemberAdded is the domain event
// "superops.{workspace_id}.channel.member_added", consumed by the worker's
// notifier-channel-invite durable to produce a channel_invite notification. It
// is not a WebSocket frame type: the relay ignores it.
const evtChannelMemberAdded = "channel.member_added"

// publishTimeout bounds the JetStream storage ack so a sick NATS cannot stall a
// handler whose write has already committed. Mirrors internal/message.
const publishTimeout = 3 * time.Second

// memberAddedEvent is the payload of channel.member_added. It must stay
// field-for-field compatible with notification.ChannelMemberEvent, which is the
// only consumer.
type memberAddedEvent struct {
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	UserID      string `json:"user_id"`  // the member who was added
	ActorID     string `json:"actor_id"` // whoever added them
}

// ---------------------------------------------------------------------------
// Payload builders
//
// The relay routes each event on exact top-level keys and silently drops a
// payload that does not carry them, so the shapes live in named functions with
// tests rather than inline in the handlers.
// ---------------------------------------------------------------------------

// channelCreatedPayload builds the channel.created payload. "workspace_id"
// names the audience and "type" decides what that audience is: a public channel
// goes to the whole workspace, anything else only to "member_ids" — announcing
// a private channel workspace-wide would leak its existence and name to people
// who cannot see it, and omitting member_ids would deliver it to nobody.
func channelCreatedPayload(ch *Channel, memberIDs []string) map[string]any {
	ids := memberIDs
	if ids == nil {
		ids = []string{}
	}
	return map[string]any{
		"workspace_id": ch.WorkspaceID,
		"type":         string(ch.Type),
		"member_ids":   ids,
		"channel_id":   ch.ID,
		"channel":      ch,
	}
}

// channelUpdatedPayload builds the channel.updated payload, delivered to the
// channel's subscribers by its top-level "channel_id".
func channelUpdatedPayload(ch *Channel) map[string]any {
	return map[string]any{
		"channel_id":   ch.ID,
		"workspace_id": ch.WorkspaceID,
		"channel":      ch,
	}
}

// memberEventPayload builds the member.joined / member.left payload. Both are
// routed to the channel's subscribers by "channel_id"; "role" is empty for a
// departure.
func memberEventPayload(workspaceID, channelID, userID, role string) map[string]any {
	p := map[string]any{
		"channel_id":   channelID,
		"workspace_id": workspaceID,
		"user_id":      userID,
	}
	if role != "" {
		p["role"] = role
	}
	return p
}

// unreadUpdatePayload builds the unread.update payload. It is addressed to one
// person, so the top-level key the relay routes on is "user_id", not
// "channel_id".
func unreadUpdatePayload(userID string, u *Unread) map[string]any {
	return map[string]any{
		"user_id":      userID,
		"channel_id":   u.ChannelID,
		"unread_count": u.Count,
		"last_read_at": u.LastReadAt,
	}
}

// eventKey identifies one logical event for JetStream's duplicate window, so a
// retried publish of the same fact collapses instead of notifying twice.
func eventKey(eventType string, parts ...string) string {
	return eventType + ":" + strings.Join(parts, ":")
}

// ---------------------------------------------------------------------------
// Emitters
//
// Each one tolerates a nil hub: the unit tests build handlers directly, and a
// missing hub must degrade to "no realtime event", never to a panic on a
// request that already succeeded.
// ---------------------------------------------------------------------------

func (h *Handler) emitChannelCreated(ch *Channel, memberIDs []string) {
	if h.hub == nil || ch == nil || ch.WorkspaceID == "" {
		return
	}
	h.hub.PublishChannelCreated(ch.WorkspaceID, channelCreatedPayload(ch, memberIDs))
}

func (h *Handler) emitChannelUpdated(ch *Channel) {
	if h.hub == nil || ch == nil || ch.WorkspaceID == "" {
		return
	}
	h.hub.PublishChannelUpdated(ch.WorkspaceID, channelUpdatedPayload(ch))
}

func (h *Handler) emitMemberJoined(workspaceID, channelID, userID, role string) {
	if h.hub == nil || workspaceID == "" {
		return
	}
	h.hub.PublishMemberJoined(workspaceID, memberEventPayload(workspaceID, channelID, userID, role))
}

// emitMemberLeft announces the departure and then drops the departing user's
// subscription. Delivery is gated on an in-memory map written only at subscribe
// time, so without the revocation a removed member keeps receiving every
// message in the channel for the life of their socket. The order matters: the
// revocation is what stops delivery, so the announcement goes first and the
// leaver sees their own member.left before the "unsubscribed" frame.
func (h *Handler) emitMemberLeft(workspaceID, channelID, userID string) {
	if h.hub == nil {
		return
	}
	if workspaceID != "" {
		h.hub.PublishMemberLeft(workspaceID, memberEventPayload(workspaceID, channelID, userID, ""))
	}
	h.hub.RevokeChannelSubscription(userID, channelID)
}

func (h *Handler) emitUnreadUpdate(workspaceID, userID string, u *Unread) {
	if h.hub == nil || workspaceID == "" || u == nil {
		return
	}
	h.hub.PublishUnreadUpdate(workspaceID, unreadUpdatePayload(userID, u))
}

// revokeChannelForAll drops the channel from every subscription on every
// replica. Used when the channel stops existing as far as clients are concerned
// (archived), where leaving the subscriptions in place would keep delivering
// its traffic to everyone who was subscribed when it went away.
func (h *Handler) revokeChannelForAll(channelID string) {
	if h.hub == nil {
		return
	}
	h.hub.RevokeChannelForAll(channelID)
}

// announceMemberAdded emits everything that must follow a successful add:
// member.joined for the people already in the channel, the durable
// channel.member_added the worker turns into a channel_invite notification,
// and — for a non-public channel — a channel.created addressed to the new
// member alone, which is the only way a client learns about a channel it could
// not previously see. A public channel was already announced workspace-wide
// when it was created, so re-announcing it would only duplicate.
func (h *Handler) announceMemberAdded(ctx context.Context, info *authz.ChannelInfo, targetID, actorID, role string) {
	h.emitMemberJoined(info.WorkspaceID, info.ID, targetID, role)

	// Best effort: the name only enriches the notification (the consumer looks
	// it up itself when it is empty) and the row only feeds the targeted
	// channel.created.
	ch, err := h.repo.GetByID(ctx, info.ID)
	if err != nil {
		h.logWarn("channel member added: load channel", "channel_id", info.ID, "error", err)
	}

	var name string
	if ch != nil && ch.Name != nil {
		name = *ch.Name
	}
	h.publishMemberAdded(ctx, info.WorkspaceID, memberAddedEvent{
		ChannelID:   info.ID,
		ChannelName: name,
		UserID:      targetID,
		ActorID:     actorID,
	})

	if ch != nil && ch.Type != TypePublic {
		h.emitChannelCreated(ch, []string{targetID})
	}
}

// publishMemberAdded emits channel.member_added durably: it is the only trigger
// for the channel_invite notification, so losing it silently loses the
// notification with no reconciliation path. The publish deliberately outlives
// the request context — the membership row is already committed, and a client
// that hung up must not also cost the invitee their notification.
func (h *Handler) publishMemberAdded(ctx context.Context, workspaceID string, ev memberAddedEvent) {
	if h.nats == nil || workspaceID == "" {
		return
	}
	subject := "superops." + workspaceID + "." + evtChannelMemberAdded

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout)
	defer cancel()

	// The dedupe key is the fact itself — "this actor added this user to this
	// channel" — so a retried publish collapses instead of notifying twice.
	msgID := eventKey(evtChannelMemberAdded, ev.ChannelID, ev.UserID, ev.ActorID)
	if err := h.nats.PublishDurable(ctx, subject, msgID, natspkg.Event{Type: evtChannelMemberAdded, Data: ev}); err != nil {
		h.logWarn("publish channel member added", "subject", subject, "channel_id", ev.ChannelID, "error", err)
	}
}

// logWarn tolerates a handler built without a logger, which the unit tests do.
func (h *Handler) logWarn(msg string, args ...any) {
	log := h.log
	if log == nil {
		log = slog.Default()
	}
	log.Warn(msg, args...)
}
