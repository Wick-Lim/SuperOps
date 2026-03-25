package ws

import (
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/nats-io/nats.go"
)

const natsSubjectPrefix = "ws.broadcast."

// BroadcastEnvelope is published to NATS for cross-instance delivery.
type BroadcastEnvelope struct {
	ChannelID     string `json:"channel_id"`
	ExcludeUserID string `json:"exclude_user_id"`
	Payload       []byte `json:"payload"`
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

		// Deliver to local clients only (no re-publish)
		h.localBroadcastRaw(env.ChannelID, env.Payload, env.ExcludeUserID)
	})
	if err != nil {
		logger.Error("nats bridge: subscribe failed", "error", err)
	} else {
		logger.Info("WebSocket NATS bridge started", "subject", natsSubjectPrefix+">")
	}
}

// publishToNATS sends a broadcast envelope to NATS for other instances.
func (h *Hub) publishToNATS(channelID string, payload []byte, excludeUserID string) {
	if h.natsConn == nil {
		return
	}
	env := BroadcastEnvelope{
		ChannelID:     channelID,
		ExcludeUserID: excludeUserID,
		Payload:       payload,
	}
	data, err := json.Marshal(env)
	if err != nil {
		return
	}
	h.natsConn.Publish(natsSubjectPrefix+channelID, data)
}

// localBroadcastRaw delivers raw bytes to local subscribed clients.
func (h *Hub) localBroadcastRaw(channelID string, payload []byte, excludeUserID string) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for userID, client := range h.clients {
		if userID == excludeUserID {
			continue
		}
		if client.IsSubscribed(channelID) {
			select {
			case client.send <- payload:
			default:
			}
		}
	}
}

func extractChannelFromSubject(subject string) string {
	// ws.broadcast.{channelId}
	if idx := strings.LastIndex(subject, "."); idx >= 0 {
		return subject[idx+1:]
	}
	return ""
}
