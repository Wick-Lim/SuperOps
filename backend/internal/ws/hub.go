package ws

import (
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/nats-io/nats.go"
)

type Hub struct {
	clients    map[string]*Client // userID -> client
	mu         sync.RWMutex
	register   chan *Client
	unregister chan *Client
	natsConn   *nats.Conn // nil = single-instance mode
	logger     *slog.Logger
}

func NewHub(logger *slog.Logger) *Hub {
	return &Hub{
		clients:    make(map[string]*Client),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		logger:     logger,
	}
}

func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			if existing, ok := h.clients[client.userID]; ok {
				close(existing.send)
				delete(h.clients, client.userID)
			}
			h.clients[client.userID] = client
			h.mu.Unlock()
			h.logger.Info("client connected", "user_id", client.userID, "total_clients", len(h.clients))

		case client := <-h.unregister:
			h.mu.Lock()
			if existing, ok := h.clients[client.userID]; ok && existing == client {
				close(client.send)
				delete(h.clients, client.userID)
			}
			h.mu.Unlock()
			h.logger.Info("client disconnected", "user_id", client.userID, "total_clients", len(h.clients))
		}
	}
}

// BroadcastToChannel sends a message to all local subscribers AND publishes
// to NATS for cross-instance delivery in multi-replica deployments.
func (h *Hub) BroadcastToChannel(channelID, msgType string, data interface{}, excludeUserID string) {
	msg, err := json.Marshal(OutboundMessage{Type: msgType, Data: data})
	if err != nil {
		return
	}

	// 1. Deliver to local clients
	h.localBroadcastRaw(channelID, msg, excludeUserID)

	// 2. Publish to NATS for other instances
	h.publishToNATS(channelID, msg, excludeUserID)
}

func (h *Hub) BroadcastToUser(userID, msgType string, data interface{}) {
	h.mu.RLock()
	client, ok := h.clients[userID]
	h.mu.RUnlock()

	if ok {
		client.SendMessage(msgType, data)
	}
}

func (h *Hub) GetOnlineUserIDs() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	ids := make([]string, 0, len(h.clients))
	for id := range h.clients {
		ids = append(ids, id)
	}
	return ids
}

func (h *Hub) IsOnline(userID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.clients[userID]
	return ok
}

// Shutdown gracefully closes all client connections.
func (h *Hub) Shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()

	for userID, client := range h.clients {
		close(client.send)
		delete(h.clients, userID)
	}
	h.logger.Info("hub shutdown, all clients disconnected")
}
