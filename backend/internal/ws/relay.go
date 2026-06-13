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
// relays via core NATS pub/sub, and each client holds exactly one WebSocket
// connection (to exactly one replica), local-only delivery on every replica
// results in exactly-once delivery per client — without the ws.broadcast
// re-publish path used for client-originated ephemeral events (typing).
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

		switch env.Type {
		case TypeMessageNew, TypeMessageUpdated, TypeMessageDeleted,
			TypeReactionAdded, TypeReactionRemoved,
			TypeChannelUpdated, TypeMemberJoined, TypeMemberLeft:
			var target struct {
				ChannelID string `json:"channel_id"`
			}
			if err := json.Unmarshal(env.Data, &target); err != nil || target.ChannelID == "" {
				return
			}
			h.BroadcastLocal(target.ChannelID, env.Type, env.Data, "")

		case TypeNotificationNew, TypeUnreadUpdate:
			var target struct {
				UserID string `json:"user_id"`
			}
			if err := json.Unmarshal(env.Data, &target); err != nil || target.UserID == "" {
				return
			}
			h.BroadcastToUser(target.UserID, env.Type, env.Data)
		}
	})
	if err != nil {
		logger.Error("event relay: subscribe failed", "error", err)
		return
	}
	logger.Info("WebSocket event relay started", "subject", "superops.*.>")
}
