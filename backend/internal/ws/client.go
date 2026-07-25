package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/Wick-Lim/SuperOps/backend/internal/presence"
)

const (
	// writeWait bounds a single frame write. Without it a half-open peer parks
	// the write pump forever while the send buffer fills and every later frame
	// is dropped.
	writeWait = 10 * time.Second
	// pingPeriod / pongWait drive server-side liveness. The client is not
	// required to send anything, so this is the only way a dead socket is
	// reaped — and the only thing keeping the presence TTL alive.
	pingPeriod = 30 * time.Second
	pongWait   = 20 * time.Second

	maxMessageSize = 4096
	sendBuffer     = 256

	// maxConsecutiveDrops is how many frames may be dropped for a wedged client
	// before the connection is closed. A client that cannot drain sendBuffer
	// frames is permanently desynced; closing it is the only signal that lets
	// its reconnect logic resync.
	maxConsecutiveDrops = 32

	// Inbound frame budget. Every subscribe/typing frame costs a Postgres
	// round-trip, and the HTTP rate limiter only ever sees the upgrade request.
	inboundRatePerSecond = 5
	inboundBurst         = 20
	maxRateStrikes       = 20

	// membershipRecheckPeriod backstops RevokeChannelSubscription.
	membershipRecheckPeriod = 5 * time.Minute

	// presenceOpTimeout bounds Redis presence writes made off the request path.
	presenceOpTimeout = 5 * time.Second
	// memberCheckTimeout bounds the Postgres membership query made from a pump.
	memberCheckTimeout = 5 * time.Second
)

type Client struct {
	hub  *Hub
	conn *websocket.Conn

	ctx    context.Context
	cancel context.CancelFunc

	userID     string
	connID     string
	workspaces map[string]bool // immutable after construction

	// send is never closed: closing it would race every concurrent broadcast
	// (the race detector flags send-vs-close, and the runtime panics on it).
	// The write pump exits on ctx instead, and a stale buffer is garbage.
	send    chan []byte
	sendMu  sync.Mutex // serialises seq assignment with the enqueue
	seq     atomic.Int64
	drops   atomic.Int64
	strikes atomic.Int64
	closing atomic.Bool

	mu            sync.RWMutex
	subscriptions map[string]bool

	limiter  *tokenBucket
	presence *presence.Service
	isMember MemberChecker
	logger   *slog.Logger
}

func NewClient(
	parent context.Context,
	hub *Hub,
	conn *websocket.Conn,
	userID string,
	workspaceIDs []string,
	presenceSvc *presence.Service,
	isMember MemberChecker,
	logger *slog.Logger,
) *Client {
	ctx, cancel := context.WithCancel(parent)

	if isMember == nil {
		// Fail closed: a missing authorization dependency must never mean
		// "everyone is a member".
		isMember = func(context.Context, string, string) (bool, error) { return false, nil }
	}

	workspaces := make(map[string]bool, len(workspaceIDs))
	for _, id := range workspaceIDs {
		workspaces[id] = true
	}

	return &Client{
		hub:           hub,
		conn:          conn,
		ctx:           ctx,
		cancel:        cancel,
		userID:        userID,
		connID:        uuid.NewString(),
		workspaces:    workspaces,
		send:          make(chan []byte, sendBuffer),
		subscriptions: make(map[string]bool),
		limiter:       newTokenBucket(inboundRatePerSecond, inboundBurst, time.Now()),
		presence:      presenceSvc,
		isMember:      isMember,
		logger:        logger,
	}
}

// ---------------------------------------------------------------------------
// Pumps
// ---------------------------------------------------------------------------

// ReadPump runs on the HTTP handler goroutine. It recovers panics itself:
// RecoveryMiddleware still wraps this handler, and letting a panic reach it
// would make it write a JSON 500 body to an already-hijacked ResponseWriter.
func (c *Client) ReadPump() {
	defer c.recoverPump("read")
	defer func() {
		// Never block here: after Shutdown nobody is reading h.unregister.
		select {
		case c.hub.unregister <- c:
		case <-c.hub.done:
		}
		c.conn.Close(websocket.StatusNormalClosure, "")
	}()

	c.conn.SetReadLimit(maxMessageSize)

	for {
		// c.ctx is the read deadline: the keepalive cancels it when a ping goes
		// unanswered for pongWait. A plain per-read timeout would be wrong —
		// coder/websocket answers pings inside Reader, so pongs never surface
		// from Read and an idle-but-healthy socket would be killed.
		_, data, err := c.conn.Read(c.ctx)
		if err != nil {
			if websocket.CloseStatus(err) != -1 {
				c.logger.Debug("websocket closed", "user_id", c.userID, "conn_id", c.connID)
			} else {
				c.logger.Warn("websocket read error", "user_id", c.userID, "conn_id", c.connID, "error", err)
			}
			return
		}

		if !c.limiter.allow(time.Now()) {
			c.sendError("RATE_LIMITED", "too many frames")
			if c.strikes.Add(1) >= maxRateStrikes {
				c.hub.kill(c, websocket.StatusPolicyViolation, "inbound rate limit exceeded")
				return
			}
			continue
		}

		var msg InboundMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			c.sendError("BAD_REQUEST", "invalid message format")
			continue
		}

		c.handleMessage(c.ctx, msg)
	}
}

func (c *Client) WritePump() {
	defer c.recoverPump("write")
	defer c.conn.Close(websocket.StatusNormalClosure, "")

	for {
		select {
		case message := <-c.send:
			ctx, cancel := context.WithTimeout(c.ctx, writeWait)
			err := c.conn.Write(ctx, websocket.MessageText, message)
			cancel()
			if err != nil {
				c.logger.Warn("websocket write error", "user_id", c.userID, "conn_id", c.connID, "error", err)
				return
			}
		case <-c.ctx.Done():
			return
		}
	}
}

// KeepAlive pings the peer, refreshes presence server-side and re-verifies
// subscriptions. It replaces the client-driven "ping" frame the mobile app
// never actually sends, which left the 5-minute presence TTL expiring under a
// perfectly live socket.
func (c *Client) KeepAlive() {
	defer c.recoverPump("keepalive")

	ping := time.NewTicker(pingPeriod)
	defer ping.Stop()
	recheck := time.NewTicker(membershipRecheckPeriod)
	defer recheck.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return

		case <-ping.C:
			ctx, cancel := context.WithTimeout(c.ctx, pongWait)
			err := c.conn.Ping(ctx)
			cancel()
			if err != nil {
				if c.ctx.Err() == nil {
					c.hub.kill(c, websocket.StatusGoingAway, "ping timeout")
				}
				return
			}
			c.refreshPresence()

		case <-recheck.C:
			c.recheckSubscriptions()
		}
	}
}

func (c *Client) recoverPump(name string) {
	if r := recover(); r != nil {
		c.logger.Error("websocket pump panic",
			"pump", name, "user_id", c.userID, "conn_id", c.connID,
			"panic", r, "stack", string(debug.Stack()))
		go c.close(websocket.StatusInternalError, "internal error")
	}
}

// close performs the close handshake and stops the pumps. It blocks for the
// duration of the handshake, so hot paths must call it via Hub.kill.
func (c *Client) close(code websocket.StatusCode, reason string) {
	if !c.closing.CompareAndSwap(false, true) {
		return
	}
	if c.conn != nil {
		_ = c.conn.Close(code, reason)
	}
	c.cancel()
}

// ---------------------------------------------------------------------------
// Outbound
// ---------------------------------------------------------------------------

func (c *Client) SendMessage(msgType string, data interface{}) {
	f, err := newFrame(msgType, data)
	if err != nil {
		c.logger.Warn("marshal outbound frame", "user_id", c.userID, "type", msgType, "error", err)
		return
	}
	c.sendFrame(f)
}

func (c *Client) sendError(code, message string) {
	c.SendMessage(TypeError, map[string]string{"code": code, "message": message})
}

// sendFrame stamps the frame with this connection's next sequence number and
// hands it to the write pump. Assignment and enqueue happen under one mutex:
// assigning outside it would let two goroutines interleave (A takes 1, B takes
// 2, B enqueues first) and the client would see the stream go backwards, which
// defeats the gap detection seq exists for.
func (c *Client) sendFrame(f frame) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	b, err := f.encode(c.seq.Add(1))
	if err != nil {
		c.logger.Warn("encode outbound frame", "user_id", c.userID, "type", f.msgType, "error", err)
		return
	}
	c.trySend(b)
}

// trySend delivers bytes to the client's buffered send channel without blocking.
// A broadcast may still hold a reference to a client the hub has already
// unregistered; those frames land in a buffer nobody drains and are collected
// with the client.
func (c *Client) trySend(b []byte) {
	select {
	case c.send <- b:
		c.drops.Store(0)
	default:
		n := c.drops.Add(1)
		c.logger.Warn("client send buffer full, dropping message",
			"user_id", c.userID, "conn_id", c.connID, "consecutive_drops", n)
		if n >= maxConsecutiveDrops {
			c.hub.kill(c, websocket.StatusTryAgainLater, "client too slow")
		}
	}
}

// ---------------------------------------------------------------------------
// Subscriptions
// ---------------------------------------------------------------------------

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

func (c *Client) Subscriptions() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.subscriptions))
	for id := range c.subscriptions {
		out = append(out, id)
	}
	return out
}

// InWorkspace reports whether this connection's user belongs to a workspace.
// The set is resolved once at upgrade time and never mutated.
func (c *Client) InWorkspace(workspaceID string) bool {
	return c.workspaces[workspaceID]
}

func (c *Client) WorkspaceIDs() []string {
	out := make([]string, 0, len(c.workspaces))
	for id := range c.workspaces {
		out = append(out, id)
	}
	return out
}

// recheckSubscriptions re-validates every live subscription. Hub.RevokeChannel-
// Subscription is the primary revocation path, but it depends on the channel
// package calling it and on the NATS revoke fan-out being healthy; this sweep is
// the backstop that also catches channel deletion, archival and direct database
// edits. It is deliberately infrequent because each check is a Postgres query.
func (c *Client) recheckSubscriptions() {
	if c.isMember == nil {
		return
	}
	for _, channelID := range c.Subscriptions() {
		ctx, cancel := context.WithTimeout(c.ctx, memberCheckTimeout)
		ok, err := c.isMember(ctx, channelID, c.userID)
		cancel()
		if err != nil {
			// A database blip must not revoke a legitimate subscription.
			c.logger.Warn("subscription recheck failed", "user_id", c.userID, "channel_id", channelID, "error", err)
			continue
		}
		if !ok {
			c.Unsubscribe(channelID)
			c.SendMessage(TypeUnsubscribed, map[string]string{
				"channel_id": channelID,
				"reason":     ReasonRevoked,
			})
		}
	}
}

// ---------------------------------------------------------------------------
// Inbound dispatch
// ---------------------------------------------------------------------------

func (c *Client) handleMessage(ctx context.Context, msg InboundMessage) {
	switch msg.Type {
	case TypePing:
		c.refreshPresence()
		c.SendMessage(TypePong, nil)

	case TypeSubscribe:
		channelID, ok := c.channelArg(msg.Data)
		if !ok {
			return
		}
		allowed, err := c.isMember(ctx, channelID, c.userID)
		if err != nil {
			c.logger.Warn("subscribe: membership check", "user_id", c.userID, "channel_id", channelID, "error", err)
			c.sendError("INTERNAL_ERROR", "could not verify channel membership")
			return
		}
		if !allowed {
			c.sendError("FORBIDDEN", "not a member of this channel")
			return
		}
		c.Subscribe(channelID)
		// Acknowledge: without this a client cannot know when its subscription
		// is live, and everything posted in the gap is lost.
		c.SendMessage(TypeSubscribed, map[string]string{"channel_id": channelID})

	case TypeUnsubscribe:
		channelID, ok := c.channelArg(msg.Data)
		if !ok {
			return
		}
		c.Unsubscribe(channelID)
		c.SendMessage(TypeUnsubscribed, map[string]string{
			"channel_id": channelID,
			"reason":     ReasonClient,
		})

	case TypeTypingStart:
		c.handleTyping(ctx, msg.Data, true)

	case TypeTypingStop:
		c.handleTyping(ctx, msg.Data, false)

	case TypePresence:
		var data PresenceData
		if err := json.Unmarshal(msg.Data, &data); err != nil {
			c.sendError("BAD_REQUEST", "invalid presence payload")
			return
		}
		status, ok := presence.ParseStatus(data.Status)
		if !ok {
			// Anything else would be stored verbatim and echoed to every
			// workspace member by GET /workspaces/{id}/presence.
			c.sendError("BAD_REQUEST", "invalid presence status")
			return
		}
		if c.presence != nil {
			pctx, cancel := context.WithTimeout(context.Background(), presenceOpTimeout)
			err := c.presence.SetStatus(pctx, c.userID, status)
			cancel()
			if err != nil {
				c.logger.Warn("presence update failed", "user_id", c.userID, "error", err)
				c.sendError("INTERNAL_ERROR", "could not update presence")
				return
			}
		}
		c.hub.PublishPresenceChanged(c.userID, string(status), c.WorkspaceIDs())

	default:
		c.sendError("UNKNOWN_TYPE", "unknown message type")
	}
}

// handleTyping serves typing.start and typing.stop. Both are membership-gated:
// without the check any authenticated user could write an arbitrary typing key
// for a private channel (a membership oracle for its real members) and splice
// an unvalidated client string into a NATS subject.
func (c *Client) handleTyping(ctx context.Context, raw json.RawMessage, typing bool) {
	channelID, ok := c.channelArg(raw)
	if !ok {
		return
	}

	// A live subscription is proof of membership: it was validated at subscribe
	// time and is re-validated periodically. This keeps the common keystroke
	// path off the database.
	if !c.IsSubscribed(channelID) {
		allowed, err := c.isMember(ctx, channelID, c.userID)
		if err != nil {
			c.logger.Warn("typing: membership check", "user_id", c.userID, "channel_id", channelID, "error", err)
			c.sendError("INTERNAL_ERROR", "could not verify channel membership")
			return
		}
		if !allowed {
			c.sendError("FORBIDDEN", "not a member of this channel")
			return
		}
	}

	if c.presence != nil {
		pctx, cancel := context.WithTimeout(ctx, presenceOpTimeout)
		var err error
		if typing {
			err = c.presence.SetTyping(pctx, channelID, c.userID)
		} else {
			err = c.presence.ClearTyping(pctx, channelID, c.userID)
		}
		cancel()
		if err != nil {
			c.logger.Warn("typing state write failed", "user_id", c.userID, "channel_id", channelID, "error", err)
		}
	}

	c.hub.BroadcastToChannel(channelID, TypeTypingIndicator, map[string]interface{}{
		"channel_id": channelID,
		"user_id":    c.userID,
		"typing":     typing,
	}, c.userID)
}

// channelArg extracts and validates a channel id. The value ends up in a NATS
// subject (ws.broadcast.{id}) and a Redis key, so a client string must never
// reach either unvalidated.
func (c *Client) channelArg(raw json.RawMessage) (string, bool) {
	var data SubscribeData
	if err := json.Unmarshal(raw, &data); err != nil {
		c.sendError("BAD_REQUEST", "invalid payload")
		return "", false
	}
	if uuid.Validate(data.ChannelID) != nil {
		c.sendError("BAD_REQUEST", "channel_id must be a UUID")
		return "", false
	}
	return data.ChannelID, true
}

// refreshPresence keeps the presence key alive under a live socket. It uses a
// background context because the write must not be cancelled by the socket
// closing mid-refresh.
func (c *Client) refreshPresence() {
	if c.presence == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), presenceOpTimeout)
	defer cancel()
	if err := c.presence.Heartbeat(ctx, c.userID); err != nil {
		c.logger.Warn("presence heartbeat failed", "user_id", c.userID, "error", err)
	}
}
