package ws

import (
	"encoding/json"
	"log/slog"

	"github.com/nats-io/nats.go"
)

// StartEventRelay bridges application domain events published on NATS
// (subjects of the form "superops.{workspace_id}.{resource}.{action}") to the
// WebSocket clients connected to this replica.
//
// Every backend replica runs a relay. Because each event is delivered to all
// relays via core NATS pub/sub, and each client is connected to exactly one
// replica, local-only delivery on every replica results in exactly-once
// delivery per client — without the ws.broadcast re-publish path used for
// client-originated ephemeral events (typing).
//
// The envelope Type field is the wire/outbound message type (e.g.
// "message.new"); the relay forwards the event payload verbatim so clients
// receive {type, seq, data:<payload>}.
func (h *Hub) StartEventRelay(nc *nats.Conn, logger *slog.Logger) {
	_, err := nc.Subscribe("superops.*.>", func(msg *nats.Msg) {
		var env struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(msg.Data, &env); err != nil {
			logger.Warn("event relay: unmarshal envelope", "error", err)
			return
		}
		h.dispatchEvent(env.Type, env.Data)
	})
	if err != nil {
		logger.Error("event relay: subscribe failed", "error", err)
		return
	}
	logger.Info("WebSocket event relay started", "subject", "superops.*.>")
}

// dispatchEvent routes one domain event to local clients. It is shared by the
// NATS relay and by the single-instance path in publishDomain (no NATS, no
// loop-back).
func (h *Hub) dispatchEvent(msgType string, data json.RawMessage) {
	switch msgType {
	case TypeMessageNew, TypeMessageUpdated, TypeMessageDeleted,
		TypeReactionAdded, TypeReactionRemoved,
		TypeChannelUpdated, TypeMemberJoined, TypeMemberLeft,
		// dispatchEvent is a CLOSED switch: an event type absent from it is
		// silently dropped. The huddle frames are channel-targeted, so they
		// belong in this arm and nowhere else — "no new fan-out code" would
		// have meant no fan-out at all.
		TypeHuddleStarted, TypeHuddleEnded, TypeHuddleRoster:
		var target struct {
			ChannelID string `json:"channel_id"`
		}
		if err := json.Unmarshal(data, &target); err != nil || target.ChannelID == "" {
			return
		}
		h.BroadcastLocal(target.ChannelID, msgType, data, "")

	case TypeChannelCreated:
		// A channel nobody has subscribed to yet cannot be delivered by
		// subscription. A public channel goes to the whole workspace; a private
		// one goes only to its initial members, or announcing it would leak its
		// existence and name to every workspace member.
		var target struct {
			WorkspaceID string   `json:"workspace_id"`
			Type        string   `json:"type"`
			MemberIDs   []string `json:"member_ids"`
		}
		if err := json.Unmarshal(data, &target); err != nil || target.WorkspaceID == "" {
			return
		}
		if target.Type == "public" {
			h.BroadcastToWorkspaceLocal(target.WorkspaceID, msgType, data, "")
			return
		}
		for _, userID := range target.MemberIDs {
			h.BroadcastToUser(userID, msgType, data)
		}

	case TypePresenceChanged:
		var target struct {
			WorkspaceID string `json:"workspace_id"`
		}
		if err := json.Unmarshal(data, &target); err != nil || target.WorkspaceID == "" {
			return
		}
		h.BroadcastToWorkspaceLocal(target.WorkspaceID, msgType, data, "")

	case TypeNotificationNew, TypeUnreadUpdate:
		var target struct {
			UserID string `json:"user_id"`
		}
		if err := json.Unmarshal(data, &target); err != nil || target.UserID == "" {
			return
		}
		h.BroadcastToUser(target.UserID, msgType, data)
	}
}
