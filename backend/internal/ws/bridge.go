package ws

import (
	"encoding/json"
	"log/slog"

	"github.com/nats-io/nats.go"
)

const (
	natsSubjectPrefix = "ws.broadcast."
	natsRevokeSubject = "ws.revoke"
)

// BroadcastEnvelope is published to NATS for cross-instance delivery. It
// carries the frame type and payload rather than a fully-encoded frame, because
// the sequence number is per-connection and must be stamped by the receiving
// replica.
type BroadcastEnvelope struct {
	OriginID      string          `json:"origin_id"`
	ChannelID     string          `json:"channel_id"`
	ExcludeUserID string          `json:"exclude_user_id"`
	Type          string          `json:"type"`
	Data          json.RawMessage `json:"data"`
}

// RevokeEnvelope withdraws a channel subscription across replicas. A member can
// be removed by a request served by a replica the member's socket is not on.
type RevokeEnvelope struct {
	OriginID  string `json:"origin_id"`
	UserID    string `json:"user_id"` // "" = every connection
	ChannelID string `json:"channel_id"`
}

// StartNATSBridge subscribes the hub to NATS broadcast subjects
// so messages from other backend instances are delivered to local clients.
func (h *Hub) StartNATSBridge(nc *nats.Conn, logger *slog.Logger) {
	h.natsConn = nc

	_, err := nc.Subscribe(natsSubjectPrefix+">", func(msg *nats.Msg) {
		var env BroadcastEnvelope
		if err := json.Unmarshal(msg.Data, &env); err != nil {
			logger.Warn("nats bridge: unmarshal", "error", err)
			return
		}

		// Skip envelopes we published ourselves — the origin replica already
		// delivered them locally in BroadcastToChannel, and core NATS echoes a
		// connection's own publishes back to it.
		if env.OriginID == h.id {
			return
		}

		// Deliver to local clients only (no re-publish)
		h.localBroadcast(env.ChannelID, frame{msgType: env.Type, data: env.Data}, env.ExcludeUserID)
	})
	if err != nil {
		logger.Error("nats bridge: subscribe failed", "error", err)
	} else {
		logger.Info("WebSocket NATS bridge started", "subject", natsSubjectPrefix+">")
	}

	_, err = nc.Subscribe(natsRevokeSubject, func(msg *nats.Msg) {
		var env RevokeEnvelope
		if err := json.Unmarshal(msg.Data, &env); err != nil {
			logger.Warn("nats bridge: unmarshal revoke", "error", err)
			return
		}
		if env.OriginID == h.id {
			return
		}
		h.revokeLocal(env.UserID, env.ChannelID)
	})
	if err != nil {
		logger.Error("nats bridge: revoke subscribe failed", "error", err)
	} else {
		logger.Info("WebSocket revoke bridge started", "subject", natsRevokeSubject)
	}
}

// publishToNATS sends a broadcast envelope to NATS for other instances.
func (h *Hub) publishToNATS(channelID string, f frame, excludeUserID string) {
	if h.natsConn == nil {
		return
	}
	env := BroadcastEnvelope{
		OriginID:      h.id,
		ChannelID:     channelID,
		ExcludeUserID: excludeUserID,
		Type:          f.msgType,
		Data:          f.data,
	}
	data, err := json.Marshal(env)
	if err != nil {
		h.logger.Warn("nats bridge: marshal envelope", "error", err)
		return
	}
	if err := h.natsConn.Publish(natsSubjectPrefix+channelID, data); err != nil {
		h.logger.Warn("nats bridge: publish failed", "channel_id", channelID, "error", err)
	}
}

func (h *Hub) publishRevoke(userID, channelID string) {
	if h.natsConn == nil {
		return
	}
	data, err := json.Marshal(RevokeEnvelope{OriginID: h.id, UserID: userID, ChannelID: channelID})
	if err != nil {
		return
	}
	if err := h.natsConn.Publish(natsRevokeSubject, data); err != nil {
		h.logger.Warn("nats bridge: publish revoke failed", "channel_id", channelID, "error", err)
	}
}
