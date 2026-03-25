package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
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
	logger        *slog.Logger
}

func NewClient(hub *Hub, conn *websocket.Conn, userID string, logger *slog.Logger) *Client {
	return &Client{
		hub:           hub,
		conn:          conn,
		userID:        userID,
		send:          make(chan []byte, 256),
		subscriptions: make(map[string]bool),
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
		if err := json.Unmarshal(msg.Data, &data); err == nil {
			c.hub.BroadcastToChannel(data.ChannelID, TypeTypingIndicator, map[string]string{
				"channel_id": data.ChannelID,
				"user_id":    c.userID,
			}, c.userID)
		}

	default:
		c.SendMessage(TypeError, map[string]string{"message": "unknown message type"})
	}
}
