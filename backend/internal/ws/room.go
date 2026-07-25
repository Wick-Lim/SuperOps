package ws

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// A collaboration room is a subscription like any other, kept in a map beside
// the channel subscriptions rather than inside it.
//
// Two separate maps and not one namespaced key, because both ids are UUIDs
// drawn from different tables: a single map would let a document id that
// happened to equal a channel id deliver channel traffic to a document
// subscriber and vice versa. The room map's value is the caller's write
// permission at join time, so the keystroke path can reject a read-only editor
// without a database round-trip.

const (
	natsRoomSubjectPrefix = "ws.room."
	// Deliberately not "ws.room.revoke": that would fall inside the
	// "ws.room.>" wildcard the room bridge subscribes to.
	natsRoomRevokeSubject = "ws.roomrevoke"
)

// Errors a RoomHandler returns to say what went wrong in terms the socket can
// turn into a frame. Anything else is an internal failure and is reported as
// one — a database blip must never render as "you are not allowed", which is
// the same rule internal/authz states for (bool, error).
var (
	ErrRoomNotFound        = errors.New("collaboration document not found")
	ErrRoomForbidden       = errors.New("not allowed to open this collaboration document")
	ErrRoomReadOnly        = errors.New("read-only access to this collaboration document")
	ErrRoomInvalidPayload  = errors.New("invalid collaboration payload")
	ErrRoomPayloadTooLarge = errors.New("collaboration payload too large")
)

// RoomAccess is what a client is allowed to do in a room, plus where the log
// currently is.
//
// HeadSeq is informational: it tells a reconnecting client whether it is behind
// and should fetch state over HTTP. It is NOT a watermark — a client's
// watermark is the highest seq below which it has received every update, and
// broadcast order is not commit order (two appends commit in seq order but
// their fan-out goroutines may interleave). Advancing a watermark past a gap
// loses whatever fell in it.
type RoomAccess struct {
	HeadSeq  int64
	CanWrite bool
}

// RoomHandler is the collaboration layer's entry point into the socket.
//
// It is an interface so this package does not import the collaboration package,
// which broadcasts through the hub — the dependency runs one way, collab -> ws.
// Join and Recheck both return ErrRoomForbidden/ErrRoomNotFound to mean "revoke
// or refuse"; every other error means the decision could not be made and the
// caller must leave the existing state alone.
type RoomHandler interface {
	Join(ctx context.Context, documentID, userID string) (RoomAccess, error)
	Recheck(ctx context.Context, documentID, userID string) (RoomAccess, error)
	Update(ctx context.Context, documentID, userID, connID string, payload []byte) error
	Awareness(ctx context.Context, documentID, userID, connID string, payload []byte) error
}

// ---------------------------------------------------------------------------
// Client-side room membership
// ---------------------------------------------------------------------------

// JoinRoom records a room membership and the write permission it was granted
// with.
func (c *Client) JoinRoom(documentID string, canWrite bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rooms[documentID] = canWrite
}

// setRoomWrite refreshes the write flag of an existing membership. It must not
// create one: between listing the rooms and applying a recheck the client may
// have left, and re-adding it would resurrect a membership nobody asked for.
func (c *Client) setRoomWrite(documentID string, canWrite bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.rooms[documentID]; ok {
		c.rooms[documentID] = canWrite
	}
}

func (c *Client) LeaveRoom(documentID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.rooms, documentID)
}

func (c *Client) InRoom(documentID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.rooms[documentID]
	return ok
}

// CanWriteRoom reports whether this connection may append updates to a
// document. A connection that is not in the room can never write.
func (c *Client) CanWriteRoom(documentID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.rooms[documentID]
}

func (c *Client) Rooms() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.rooms))
	for id := range c.rooms {
		out = append(out, id)
	}
	return out
}

// ---------------------------------------------------------------------------
// Inbound collaboration frames
// ---------------------------------------------------------------------------

// handleCollabJoin authorizes a room and records the membership. There is a
// window between the authorization returning and the membership being recorded
// in which a revocation could be missed; it is the same window the channel
// subscribe path has, and it is closed by the periodic recheck rather than by
// holding a lock across a database call.
func (c *Client) handleCollabJoin(ctx context.Context, raw json.RawMessage) {
	documentID, ok := c.documentArg(raw)
	if !ok {
		return
	}
	if c.roomHandler == nil {
		c.sendError("UNSUPPORTED", "collaboration is not enabled on this server")
		return
	}

	ctx, cancel := context.WithTimeout(ctx, collabOpTimeout)
	access, err := c.roomHandler.Join(ctx, documentID, c.userID)
	cancel()
	if err != nil {
		c.sendRoomError("join", documentID, err)
		return
	}

	c.JoinRoom(documentID, access.CanWrite)
	// Acknowledge with the log head so the client knows whether it is behind
	// and must fetch state over HTTP before it starts editing.
	//
	// There is deliberately no roster in this frame. Who is in a room is
	// awareness, awareness is ephemeral, and a roster assembled here would only
	// ever list the members that happen to share this replica. The protocol
	// rule instead is that a client re-announces its own awareness when it sees
	// a state from someone it does not know — which is correct on one replica
	// and on ten, and costs the server no state at all.
	c.SendMessage(TypeCollabJoined, map[string]interface{}{
		"document_id": documentID,
		"head_seq":    access.HeadSeq,
		"can_write":   access.CanWrite,
	})
}

func (c *Client) handleCollabLeave(raw json.RawMessage) {
	documentID, ok := c.documentArg(raw)
	if !ok {
		return
	}
	c.LeaveRoom(documentID)
	c.SendMessage(TypeCollabLeft, map[string]string{
		"document_id": documentID,
		"reason":      ReasonClient,
	})
}

// handleCollabUpdate persists and relays one opaque CRDT update.
//
// Authorization is the room membership and the write flag recorded at join
// time, not a query per update: a keystroke batch must not cost a Postgres
// authorization round-trip on top of its append. That is the same trade the
// typing path makes, and it is backed by the same two mechanisms — explicit
// revocation through the hub, and the periodic recheck.
func (c *Client) handleCollabUpdate(ctx context.Context, raw json.RawMessage) {
	var data CollabUpdateData
	if err := json.Unmarshal(raw, &data); err != nil {
		c.sendError("BAD_REQUEST", "invalid collaboration payload")
		return
	}
	if uuid.Validate(data.DocumentID) != nil {
		c.sendError("BAD_REQUEST", "document_id must be a UUID")
		return
	}
	if c.roomHandler == nil || !c.InRoom(data.DocumentID) {
		c.sendError("FORBIDDEN", "join the document before sending updates")
		return
	}
	if !c.CanWriteRoom(data.DocumentID) {
		c.sendRoomError("update", data.DocumentID, ErrRoomReadOnly)
		return
	}
	if len(data.Update) == 0 {
		c.sendRoomError("update", data.DocumentID, ErrRoomInvalidPayload)
		return
	}
	if len(data.Update) > maxCollabPayloadBytes {
		c.sendRoomError("update", data.DocumentID, ErrRoomPayloadTooLarge)
		return
	}

	ctx, cancel := context.WithTimeout(ctx, collabOpTimeout)
	err := c.roomHandler.Update(ctx, data.DocumentID, c.userID, c.connID, data.Update)
	cancel()
	if err != nil {
		c.sendRoomError("update", data.DocumentID, err)
	}
}

// handleCollabAwareness relays a cursor/selection state. It is ephemeral by
// construction: nothing on this path writes to Postgres, and read access is
// enough — a viewer's cursor is exactly as visible as an editor's.
func (c *Client) handleCollabAwareness(ctx context.Context, raw json.RawMessage) {
	var data CollabAwarenessData
	if err := json.Unmarshal(raw, &data); err != nil {
		c.sendError("BAD_REQUEST", "invalid collaboration payload")
		return
	}
	if uuid.Validate(data.DocumentID) != nil {
		c.sendError("BAD_REQUEST", "document_id must be a UUID")
		return
	}
	if c.roomHandler == nil || !c.InRoom(data.DocumentID) {
		c.sendError("FORBIDDEN", "join the document before sending awareness")
		return
	}
	if len(data.State) == 0 || len(data.State) > maxCollabPayloadBytes {
		c.sendRoomError("awareness", data.DocumentID, ErrRoomInvalidPayload)
		return
	}

	if err := c.roomHandler.Awareness(ctx, data.DocumentID, c.userID, c.connID, data.State); err != nil {
		c.sendRoomError("awareness", data.DocumentID, err)
	}
}

// documentArg extracts and validates a document id. Like channelArg, the value
// reaches a NATS subject, so a client string must never get there unvalidated.
func (c *Client) documentArg(raw json.RawMessage) (string, bool) {
	var data CollabDocumentData
	if err := json.Unmarshal(raw, &data); err != nil {
		c.sendError("BAD_REQUEST", "invalid payload")
		return "", false
	}
	if uuid.Validate(data.DocumentID) != nil {
		c.sendError("BAD_REQUEST", "document_id must be a UUID")
		return "", false
	}
	return data.DocumentID, true
}

// sendRoomError turns a RoomHandler error into a client-visible code. An error
// that is not one of the sentinels is a failure to decide, not a denial, and is
// reported as an internal error with the detail kept server-side.
func (c *Client) sendRoomError(op, documentID string, err error) {
	switch {
	case errors.Is(err, ErrRoomNotFound):
		c.sendError("NOT_FOUND", "collaboration document not found")
	case errors.Is(err, ErrRoomForbidden):
		c.sendError("FORBIDDEN", "not allowed to open this collaboration document")
	case errors.Is(err, ErrRoomReadOnly):
		c.sendError("FORBIDDEN", "read-only access to this collaboration document")
	case errors.Is(err, ErrRoomInvalidPayload):
		c.sendError("BAD_REQUEST", "invalid collaboration payload")
	case errors.Is(err, ErrRoomPayloadTooLarge):
		c.sendError("PAYLOAD_TOO_LARGE", "collaboration update too large for the socket; use the HTTP append endpoint")
	default:
		c.logger.Warn("collaboration frame failed",
			"op", op, "user_id", c.userID, "document_id", documentID, "error", err)
		c.sendError("INTERNAL_ERROR", "could not process collaboration frame")
	}
}

// recheckRooms re-validates every live room membership. RevokeRoom is the
// primary path; this is the backstop that catches a workspace membership
// withdrawn by a request nobody thought to hook, and it also refreshes the
// write flag so a demotion to guest takes effect without a reconnect.
func (c *Client) recheckRooms() {
	if c.roomHandler == nil {
		return
	}
	for _, documentID := range c.Rooms() {
		ctx, cancel := context.WithTimeout(c.ctx, memberCheckTimeout)
		access, err := c.roomHandler.Recheck(ctx, documentID, c.userID)
		cancel()

		switch {
		case errors.Is(err, ErrRoomForbidden), errors.Is(err, ErrRoomNotFound):
			c.LeaveRoom(documentID)
			c.SendMessage(TypeCollabLeft, map[string]string{
				"document_id": documentID,
				"reason":      ReasonRevoked,
			})
		case err != nil:
			// A database blip must not revoke a legitimate session.
			c.logger.Warn("collaboration recheck failed",
				"user_id", c.userID, "document_id", documentID, "error", err)
		default:
			c.setRoomWrite(documentID, access.CanWrite)
		}
	}
}

// ---------------------------------------------------------------------------
// Fan-out
// ---------------------------------------------------------------------------

// BroadcastToRoom delivers to local room members AND republishes to NATS, the
// same shape as BroadcastToChannel: room traffic is client-originated, so it
// arrives on exactly one replica and the other replicas learn about it only
// through this publish. The origin guard in the bridge stops it coming back.
//
// Nobody is excluded, not even the connection that sent the update. Excluding
// by user id would silence a second device belonging to the same person editing
// the same document, and CRDT updates are idempotent — a client applying its own
// update again is a no-op, and the echo is how it learns the seq the server
// assigned.
func (h *Hub) BroadcastToRoom(documentID, msgType string, data interface{}) {
	f, err := newFrame(msgType, data)
	if err != nil {
		h.logger.Warn("room broadcast: marshal payload", "type", msgType, "error", err)
		return
	}
	h.roomBroadcast(documentID, f)
	h.publishRoomToNATS(documentID, f)
}

// BroadcastToRoomLocal delivers only to this replica's members. It exists for
// the NATS bridge, which must not re-publish what it just received.
func (h *Hub) BroadcastToRoomLocal(documentID, msgType string, data interface{}) {
	f, err := newFrame(msgType, data)
	if err != nil {
		h.logger.Warn("room broadcast: marshal payload", "type", msgType, "error", err)
		return
	}
	h.roomBroadcast(documentID, f)
}

func (h *Hub) roomBroadcast(documentID string, f frame) {
	for _, c := range h.roomTargets(documentID) {
		c.sendFrame(f)
	}
}

func (h *Hub) roomTargets(documentID string) []*Client {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var out []*Client
	for _, conns := range h.clients {
		for _, c := range conns {
			if c.InRoom(documentID) {
				out = append(out, c)
			}
		}
	}
	return out
}

// SendToRoomLeader delivers to at most one local member of a room, chosen
// deterministically by connection id.
//
// Compaction is the caller: the server cannot merge CRDT state, so it has to
// ask a client to produce a snapshot, and asking everyone would have the whole
// room upload a full document copy at once. Every replica picks its own leader,
// so a room spread over three replicas produces up to three snapshots for one
// request — harmless, because the second and third are rejected as stale by the
// compaction transaction, and far cheaper than the cross-replica election that
// would be needed to make it exactly one.
func (h *Hub) SendToRoomLeader(documentID, msgType string, data interface{}) bool {
	targets := h.roomTargets(documentID)
	if len(targets) == 0 {
		return false
	}
	leader := targets[0]
	for _, c := range targets[1:] {
		if c.connID < leader.connID {
			leader = c
		}
	}
	leader.SendMessage(msgType, data)
	return true
}

// ---------------------------------------------------------------------------
// Revocation
// ---------------------------------------------------------------------------

// RevokeRoom drops a document from every connection userID holds, cluster-wide.
// Delivery is gated on the in-memory room map, so without this a user whose
// access is withdrawn mid-session keeps receiving every keystroke of a document
// they can no longer open, for the life of their socket.
func (h *Hub) RevokeRoom(userID, documentID string) {
	h.revokeRoomLocal(userID, documentID)
	h.publishRoomRevoke(userID, documentID)
}

// RevokeRoomForAll drops a document from every connection on every replica. Use
// it when the document itself goes away.
func (h *Hub) RevokeRoomForAll(documentID string) {
	h.revokeRoomLocal("", documentID)
	h.publishRoomRevoke("", documentID)
}

func (h *Hub) revokeRoomLocal(userID, documentID string) {
	if documentID == "" {
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
		if !c.InRoom(documentID) {
			continue
		}
		c.LeaveRoom(documentID)
		c.SendMessage(TypeCollabLeft, map[string]string{
			"document_id": documentID,
			"reason":      ReasonRevoked,
		})
	}
}

// ---------------------------------------------------------------------------
// Cross-replica transport
// ---------------------------------------------------------------------------

// RoomEnvelope carries a room frame between replicas. Like BroadcastEnvelope it
// ships the payload rather than an encoded frame, because seq is per-connection
// and must be stamped by the replica that owns the socket.
type RoomEnvelope struct {
	OriginID   string          `json:"origin_id"`
	DocumentID string          `json:"document_id"`
	Type       string          `json:"type"`
	Data       json.RawMessage `json:"data"`
}

// RoomRevokeEnvelope withdraws a room membership across replicas.
type RoomRevokeEnvelope struct {
	OriginID   string `json:"origin_id"`
	UserID     string `json:"user_id"` // "" = every connection
	DocumentID string `json:"document_id"`
}

// StartRoomBridge subscribes the hub to the collaboration room subjects. It is
// separate from StartNATSBridge so a deployment that has not wired the
// collaboration layer carries none of its traffic.
func (h *Hub) StartRoomBridge(nc *nats.Conn, logger *slog.Logger) {
	h.natsConn = nc

	_, err := nc.Subscribe(natsRoomSubjectPrefix+">", func(msg *nats.Msg) {
		var env RoomEnvelope
		if err := json.Unmarshal(msg.Data, &env); err != nil {
			logger.Warn("room bridge: unmarshal", "error", err)
			return
		}
		// Our own publish, echoed back: the origin replica already delivered it
		// locally in BroadcastToRoom.
		if env.OriginID == h.id {
			return
		}
		h.roomBroadcast(env.DocumentID, frame{msgType: env.Type, data: env.Data})
	})
	if err != nil {
		logger.Error("room bridge: subscribe failed", "error", err)
	} else {
		logger.Info("WebSocket collaboration room bridge started", "subject", natsRoomSubjectPrefix+">")
	}

	_, err = nc.Subscribe(natsRoomRevokeSubject, func(msg *nats.Msg) {
		var env RoomRevokeEnvelope
		if err := json.Unmarshal(msg.Data, &env); err != nil {
			logger.Warn("room bridge: unmarshal revoke", "error", err)
			return
		}
		if env.OriginID == h.id {
			return
		}
		h.revokeRoomLocal(env.UserID, env.DocumentID)
	})
	if err != nil {
		logger.Error("room bridge: revoke subscribe failed", "error", err)
	} else {
		logger.Info("WebSocket collaboration revoke bridge started", "subject", natsRoomRevokeSubject)
	}
}

func (h *Hub) publishRoomToNATS(documentID string, f frame) {
	if h.natsConn == nil {
		return
	}
	env := RoomEnvelope{OriginID: h.id, DocumentID: documentID, Type: f.msgType, Data: f.data}
	data, err := json.Marshal(env)
	if err != nil {
		h.logger.Warn("room bridge: marshal envelope", "error", err)
		return
	}
	if err := h.natsConn.Publish(natsRoomSubjectPrefix+documentID, data); err != nil {
		h.logger.Warn("room bridge: publish failed", "document_id", documentID, "error", err)
	}
}

func (h *Hub) publishRoomRevoke(userID, documentID string) {
	if h.natsConn == nil {
		return
	}
	data, err := json.Marshal(RoomRevokeEnvelope{OriginID: h.id, UserID: userID, DocumentID: documentID})
	if err != nil {
		// Three string fields; unmarshalable input is not reachable.
		h.logger.Warn("room bridge: marshal revoke", "error", err)
		return
	}
	if err := h.natsConn.Publish(natsRoomRevokeSubject, data); err != nil {
		h.logger.Warn("room bridge: publish revoke failed", "document_id", documentID, "error", err)
	}
}
