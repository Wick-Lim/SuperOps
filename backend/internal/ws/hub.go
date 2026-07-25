package ws

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// shutdownGrace bounds how long Shutdown waits for close handshakes to finish
// before the process moves on to tearing down its database and NATS handles.
const shutdownGrace = 5 * time.Second

// Hub is the per-process registry of live WebSocket connections.
//
// Connections are keyed by (userID, connID) rather than by userID alone. Keying
// by userID meant a second connect evicted the first one's send channel with no
// close frame, so a phone and a laptop that landed on the same replica killed
// each other, and every reconnect raced its own predecessor's teardown.
type Hub struct {
	id string // unique per process; used to skip our own NATS broadcasts

	mu      sync.RWMutex
	clients map[string]map[string]*Client // userID -> connID -> client
	total   int

	register   chan *Client
	unregister chan *Client

	natsConn *nats.Conn // nil = single-instance mode
	logger   *slog.Logger

	done      chan struct{} // closed by Shutdown; stops Run and releases pumps
	closeOnce sync.Once
}

func NewHub(logger *slog.Logger) *Hub {
	return &Hub{
		id:         uuid.NewString(),
		clients:    make(map[string]map[string]*Client),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		logger:     logger,
		done:       make(chan struct{}),
	}
}

// Run owns every mutation of the client registry. It exits when Shutdown is
// called.
func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			conns := h.clients[client.userID]
			if conns == nil {
				conns = make(map[string]*Client)
				h.clients[client.userID] = conns
			}
			conns[client.connID] = client
			h.total++
			// Read the counter inside the lock: reading it after Unlock races
			// with the next iteration's map write and with every RLock reader.
			total := h.total
			h.mu.Unlock()
			h.logger.Info("client connected", "user_id", client.userID, "conn_id", client.connID, "total_connections", total)

		case client := <-h.unregister:
			h.mu.Lock()
			conns := h.clients[client.userID]
			if existing, ok := conns[client.connID]; ok && existing == client {
				delete(conns, client.connID)
				if len(conns) == 0 {
					delete(h.clients, client.userID)
				}
				h.total--
			}
			total := h.total
			h.mu.Unlock()
			// Stops the write pump. The send channel is deliberately never
			// closed: closing it races every in-flight broadcast.
			client.cancel()
			h.logger.Info("client disconnected", "user_id", client.userID, "conn_id", client.connID, "total_connections", total)

		case <-h.done:
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Fan-out
// ---------------------------------------------------------------------------

// BroadcastToChannel sends a message to all local subscribers AND publishes
// to NATS for cross-instance delivery in multi-replica deployments. Use it only
// for events that originate on one replica (client-originated ephemeral events
// such as typing); anything already arriving on every replica via the event
// relay must use BroadcastLocal or it will be delivered twice.
func (h *Hub) BroadcastToChannel(channelID, msgType string, data interface{}, excludeUserID string) {
	f, err := newFrame(msgType, data)
	if err != nil {
		h.logger.Warn("broadcast: marshal payload", "type", msgType, "error", err)
		return
	}
	h.localBroadcast(channelID, f, excludeUserID)
	h.publishToNATS(channelID, f, excludeUserID)
}

// BroadcastLocal delivers a message to locally-connected subscribers of a
// channel WITHOUT re-publishing to NATS. It is used by the event relay, which
// already receives each application event on every replica via core NATS — so
// publishing again would cause duplicate delivery. Because a client is
// connected to exactly one replica, local delivery on every replica yields
// exactly-once delivery per client.
func (h *Hub) BroadcastLocal(channelID, msgType string, data interface{}, excludeUserID string) {
	f, err := newFrame(msgType, data)
	if err != nil {
		h.logger.Warn("broadcast: marshal payload", "type", msgType, "error", err)
		return
	}
	h.localBroadcast(channelID, f, excludeUserID)
}

// BroadcastToWorkspaceLocal delivers a message to every local connection whose
// user belongs to the workspace, without a subscription. Workspace-scoped
// events (a channel appearing, a presence transition) have no channel a client
// could have subscribed to yet.
func (h *Hub) BroadcastToWorkspaceLocal(workspaceID, msgType string, data interface{}, excludeUserID string) {
	f, err := newFrame(msgType, data)
	if err != nil {
		h.logger.Warn("broadcast: marshal payload", "type", msgType, "error", err)
		return
	}
	for _, c := range h.workspaceTargets(workspaceID, excludeUserID) {
		c.sendFrame(f)
	}
}

// BroadcastToUser delivers a message to every connection the user holds on this
// replica (phone, laptop, an overlapping reconnect).
func (h *Hub) BroadcastToUser(userID, msgType string, data interface{}) {
	f, err := newFrame(msgType, data)
	if err != nil {
		h.logger.Warn("broadcast: marshal payload", "type", msgType, "error", err)
		return
	}
	for _, c := range h.userTargets(userID) {
		c.sendFrame(f)
	}
}

func (h *Hub) localBroadcast(channelID string, f frame, excludeUserID string) {
	for _, c := range h.channelTargets(channelID, excludeUserID) {
		c.sendFrame(f)
	}
}

// channelTargets snapshots the recipients under the hub read lock. Delivery
// happens after the lock is released, via Client.trySend, which tolerates a
// concurrently closed send channel — holding the hub lock across delivery would
// serialise every broadcast behind the slowest client.
func (h *Hub) channelTargets(channelID, excludeUserID string) []*Client {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var out []*Client
	for userID, conns := range h.clients {
		if userID == excludeUserID {
			continue
		}
		for _, c := range conns {
			if c.IsSubscribed(channelID) {
				out = append(out, c)
			}
		}
	}
	return out
}

func (h *Hub) workspaceTargets(workspaceID, excludeUserID string) []*Client {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var out []*Client
	for userID, conns := range h.clients {
		if userID == excludeUserID {
			continue
		}
		for _, c := range conns {
			if c.InWorkspace(workspaceID) {
				out = append(out, c)
			}
		}
	}
	return out
}

func (h *Hub) userTargets(userID string) []*Client {
	h.mu.RLock()
	defer h.mu.RUnlock()

	conns := h.clients[userID]
	out := make([]*Client, 0, len(conns))
	for _, c := range conns {
		out = append(out, c)
	}
	return out
}

// ---------------------------------------------------------------------------
// Introspection
// ---------------------------------------------------------------------------

func (h *Hub) GetOnlineUserIDs() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	ids := make([]string, 0, len(h.clients))
	for id := range h.clients {
		ids = append(ids, id)
	}
	return ids
}

// ConnectionCount is the number of live sockets, which is no longer the same as
// the number of online users now that a user may hold several.
func (h *Hub) ConnectionCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.total
}

func (h *Hub) IsOnline(userID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients[userID]) > 0
}

// ---------------------------------------------------------------------------
// Subscription revocation
// ---------------------------------------------------------------------------

// RevokeChannelSubscription drops channelID from every connection userID holds,
// cluster-wide. Delivery is gated on the in-memory subscription map, which is
// only written at subscribe time, so without this a user removed from a channel
// keeps receiving its messages for the life of their socket. The revocation is
// republished over NATS because the socket may live on a different replica than
// the request that removed the member.
func (h *Hub) RevokeChannelSubscription(userID, channelID string) {
	h.revokeLocal(userID, channelID)
	h.publishRevoke(userID, channelID)
}

// RevokeChannelForAll drops a channel subscription from every connection on
// every replica. Use it when the channel itself goes away (deleted, archived).
func (h *Hub) RevokeChannelForAll(channelID string) {
	h.revokeLocal("", channelID)
	h.publishRevoke("", channelID)
}

func (h *Hub) revokeLocal(userID, channelID string) {
	if channelID == "" {
		return
	}

	var targets []*Client
	h.mu.RLock()
	if userID == "" {
		for _, conns := range h.clients {
			for _, c := range conns {
				targets = append(targets, c)
			}
		}
	} else {
		for _, c := range h.clients[userID] {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()

	for _, c := range targets {
		if !c.IsSubscribed(channelID) {
			continue
		}
		c.Unsubscribe(channelID)
		c.SendMessage(TypeUnsubscribed, map[string]string{
			"channel_id": channelID,
			"reason":     ReasonRevoked,
		})
	}
}

// ---------------------------------------------------------------------------
// Domain event publishing
// ---------------------------------------------------------------------------

// domainEvent mirrors pkg/nats.Event. It is declared here so the hub can
// publish without importing the NATS client wrapper (which would create an
// import cycle through the packages that own the hub).
type domainEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// PublishChannelCreated announces a new channel to its workspace. The payload
// must carry "workspace_id" and "type"; for a non-public channel it must also
// carry "member_ids", because a private channel must not be announced to the
// whole workspace.
func (h *Hub) PublishChannelCreated(workspaceID string, payload interface{}) {
	h.publishDomain(workspaceID, "channel", "created", TypeChannelCreated, payload)
}

// PublishChannelUpdated announces a channel rename/topic/archive change. The
// payload must carry "channel_id".
func (h *Hub) PublishChannelUpdated(workspaceID string, payload interface{}) {
	h.publishDomain(workspaceID, "channel", "updated", TypeChannelUpdated, payload)
}

// PublishMemberJoined announces a channel join. The payload must carry
// "channel_id".
func (h *Hub) PublishMemberJoined(workspaceID string, payload interface{}) {
	h.publishDomain(workspaceID, "member", "joined", TypeMemberJoined, payload)
}

// PublishMemberLeft announces a channel leave/removal. The payload must carry
// "channel_id". Callers must also call RevokeChannelSubscription for the
// departing user, otherwise their socket keeps receiving the channel.
func (h *Hub) PublishMemberLeft(workspaceID string, payload interface{}) {
	h.publishDomain(workspaceID, "member", "left", TypeMemberLeft, payload)
}

// PublishUnreadUpdate pushes a recomputed unread badge. The payload must carry
// "user_id".
func (h *Hub) PublishUnreadUpdate(workspaceID string, payload interface{}) {
	h.publishDomain(workspaceID, "unread", "update", TypeUnreadUpdate, payload)
}

// PublishPresenceChanged announces a presence transition to every workspace the
// user belongs to.
func (h *Hub) PublishPresenceChanged(userID, status string, workspaceIDs []string) {
	for _, wsID := range workspaceIDs {
		h.publishDomain(wsID, "presence", "changed", TypePresenceChanged, map[string]string{
			"user_id":      userID,
			"status":       status,
			"workspace_id": wsID,
		})
	}
}

// PublishHuddle fans a huddle event out to a channel across every replica.
//
// A thin wrapper over publishDomain rather than a new mechanism: the frame is
// channel-targeted like a message, so it rides the same subject and the same
// dispatch arm. The one thing it adds is the resource name, so an operator
// watching NATS can tell call traffic from chat traffic.
func (h *Hub) PublishHuddle(workspaceID, channelID, eventType string, data any) {
	if workspaceID == "" || channelID == "" {
		return
	}
	h.publishDomain(workspaceID, "huddle", eventType, eventType, data)
}

// publishDomain emits an event on the domain plane
// ("superops.{workspace_id}.{resource}.{action}"). Every replica's relay — this
// one included, because the NATS connection is not opened with NoEcho — then
// delivers it to its own clients, which is exactly-once per client.
func (h *Hub) publishDomain(workspaceID, resource, action, msgType string, payload interface{}) {
	raw, err := json.Marshal(payload)
	if err != nil {
		h.logger.Warn("publish event: marshal payload", "type", msgType, "error", err)
		return
	}

	if h.natsConn == nil {
		// Single-instance mode: there is no NATS loop to carry the event back,
		// so dispatch it straight to the local relay logic.
		h.dispatchEvent(msgType, raw)
		return
	}

	// The workspace id is spliced into a subject; reject anything that is not
	// the uuid the schema guarantees rather than letting it inject tokens.
	if uuid.Validate(workspaceID) != nil {
		h.logger.Warn("publish event: invalid workspace id", "type", msgType, "workspace_id", workspaceID)
		return
	}

	body, err := json.Marshal(domainEvent{Type: msgType, Data: raw})
	if err != nil {
		h.logger.Warn("publish event: marshal envelope", "type", msgType, "error", err)
		return
	}

	subject := fmt.Sprintf("superops.%s.%s.%s", workspaceID, resource, action)
	if err := h.natsConn.Publish(subject, body); err != nil {
		h.logger.Warn("publish event failed", "subject", subject, "error", err)
	}
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// kill drops a connection with a WebSocket close frame so the peer can tell a
// server-side eviction from a network failure and resync. The close handshake
// blocks, so it runs on its own goroutine and never stalls a broadcast.
func (h *Hub) kill(c *Client, code websocket.StatusCode, reason string) {
	h.logger.Warn("closing websocket connection", "user_id", c.userID, "conn_id", c.connID, "reason", reason)
	go c.close(code, reason)
}

// Shutdown closes every client with a "going away" frame and stops Run. It must
// run before the process tears down Postgres/Redis/NATS, because read pumps may
// still be issuing membership queries.
func (h *Hub) Shutdown() {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		clients := make([]*Client, 0, h.total)
		for _, conns := range h.clients {
			for _, c := range conns {
				clients = append(clients, c)
			}
		}
		h.clients = make(map[string]map[string]*Client)
		h.total = 0
		h.mu.Unlock()

		// Releases Run and any pump blocked sending on h.unregister.
		close(h.done)

		var wg sync.WaitGroup
		for _, c := range clients {
			wg.Add(1)
			go func(c *Client) {
				defer wg.Done()
				c.close(websocket.StatusGoingAway, "server shutting down")
			}(c)
		}

		drained := make(chan struct{})
		go func() {
			wg.Wait()
			close(drained)
		}()
		select {
		case <-drained:
		case <-time.After(shutdownGrace):
			h.logger.Warn("hub shutdown: close handshakes did not finish in time")
		}

		h.logger.Info("hub shutdown, all clients disconnected", "connections_closed", len(clients))
	})
}
