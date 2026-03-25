package ws

import (
	"encoding/json"
	"log/slog"
	"sync"
)

type Hub struct {
	clients    map[string]*Client // userID -> client
	mu         sync.RWMutex
	register   chan *Client
	unregister chan *Client
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
			// Close existing connection for this user (single session per user)
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

func (h *Hub) BroadcastToChannel(channelID, msgType string, data interface{}, excludeUserID string) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	msg, err := json.Marshal(OutboundMessage{Type: msgType, Data: data})
	if err != nil {
		return
	}

	for userID, client := range h.clients {
		if userID == excludeUserID {
			continue
		}
		if client.IsSubscribed(channelID) {
			select {
			case client.send <- msg:
			default:
				h.logger.Warn("dropping message for slow client", "user_id", userID)
			}
		}
	}
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
