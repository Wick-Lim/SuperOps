package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/Wick-Lim/SuperOps/backend/internal/presence"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	maxMessageSize = 4096
)

type Client struct {
	hub           *Hub
	conn          *websocket.Conn
	userID        string
	send          chan []byte
	subscriptions map[string]bool
	mu            sync.RWMutex
	seq           atomic.Int64
	presence      *presence.Service
	logger        *slog.Logger
}

func NewClient(hub *Hub, conn *websocket.Conn, userID string, presenceSvc *presence.Service, logger *slog.Logger) *Client {
	return &Client{
		hub:           hub,
		conn:          conn,
		userID:        userID,
		send:          make(chan []byte, 256),
		subscriptions: make(map[string]bool),
		presence:      presenceSvc,
		logger:        logger,
	}
}

func (c *Client) ReadPump(ctx context.Context) {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close(websocket.StatusNormalClosure, "")
	}()

	c.conn.SetReadLimit(maxMessageSize)

	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) != -1 {
				c.logger.Debug("websocket closed", "user_id", c.userID)
			} else {
				c.logger.Warn("websocket read error", "user_id", c.userID, "error", err)
			}
			return
		}

		var msg InboundMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			c.SendMessage(TypeError, map[string]string{"message": "invalid message format"})
			continue
		}

		c.handleMessage(ctx, msg)
	}
}

func (c *Client) WritePump(ctx context.Context) {
	defer c.conn.Close(websocket.StatusNormalClosure, "")

	for {
		select {
		case message, ok := <-c.send:
			if !ok {
				return
			}
			err := c.conn.Write(ctx, websocket.MessageText, message)
			if err != nil {
				c.logger.Warn("websocket write error", "user_id", c.userID, "error", err)
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (c *Client) SendMessage(msgType string, data interface{}) {
	seq := c.seq.Add(1)
	msg := OutboundMessage{
		Type: msgType,
		Seq:  seq,
		Data: data,
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	c.trySend(b)
}

// trySend delivers bytes to the client's buffered send channel without blocking.
// It recovers from a send on a closed channel, which can race when the hub
// evicts this client (re-login / unregister) concurrently with a broadcast or
// a relay-driven BroadcastToUser — that panic would otherwise crash the
// non-HTTP goroutine it runs in.
func (c *Client) trySend(b []byte) {
	defer func() {
		if recover() != nil {
			c.logger.Debug("send to closed client dropped", "user_id", c.userID)
		}
	}()
	select {
	case c.send <- b:
	default:
		c.logger.Warn("client send buffer full, dropping message", "user_id", c.userID)
	}
}

func (c *Client) Subscribe(channelID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subscriptions[channelID] = true
}

func (c *Client) Unsubscribe(channelID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.subscriptions, channelID)
}

func (c *Client) IsSubscribed(channelID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.subscriptions[channelID]
}

func (c *Client) handleMessage(ctx context.Context, msg InboundMessage) {
	switch msg.Type {
	case TypePing:
		if c.presence != nil {
			_ = c.presence.Heartbeat(ctx, c.userID)
		}
		c.SendMessage(TypePong, nil)

	case TypeSubscribe:
		var data SubscribeData
		if err := json.Unmarshal(msg.Data, &data); err == nil && data.ChannelID != "" {
			c.Subscribe(data.ChannelID)
		}

	case TypeUnsubscribe:
		var data SubscribeData
		if err := json.Unmarshal(msg.Data, &data); err == nil && data.ChannelID != "" {
			c.Unsubscribe(data.ChannelID)
		}

	case TypeTypingStart:
		var data TypingData
		if err := json.Unmarshal(msg.Data, &data); err == nil && data.ChannelID != "" {
			if c.presence != nil {
				_ = c.presence.SetTyping(ctx, data.ChannelID, c.userID)
			}
			c.hub.BroadcastToChannel(data.ChannelID, TypeTypingIndicator, map[string]string{
				"channel_id": data.ChannelID,
				"user_id":    c.userID,
			}, c.userID)
		}

	case TypePresence:
		var data PresenceData
		if err := json.Unmarshal(msg.Data, &data); err == nil && data.Status != "" && c.presence != nil {
			_ = c.presence.SetStatus(ctx, c.userID, presence.Status(data.Status))
		}

	default:
		c.SendMessage(TypeError, map[string]string{"message": "unknown message type"})
	}
}
