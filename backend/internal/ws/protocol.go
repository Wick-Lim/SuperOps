package ws

import "encoding/json"

type InboundMessage struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// OutboundMessage is the wire frame. Seq is assigned per connection and is
// strictly monotonic across every frame the connection receives, so a client
// can detect a gap (a frame dropped by backpressure) and resync.
type OutboundMessage struct {
	Type string          `json:"type"`
	Seq  int64           `json:"seq"`
	Data json.RawMessage `json:"data"`
}

// frame is an outbound message whose payload is already marshaled but whose
// sequence number is not. Broadcasts fan one frame out to many connections and
// each recipient stamps it with its own next seq, so the payload is marshaled
// once and only the tiny envelope is re-encoded per connection.
type frame struct {
	msgType string
	data    json.RawMessage
}

func newFrame(msgType string, data interface{}) (frame, error) {
	if raw, ok := data.(json.RawMessage); ok {
		return frame{msgType: msgType, data: raw}, nil
	}
	b, err := json.Marshal(data)
	if err != nil {
		return frame{}, err
	}
	return frame{msgType: msgType, data: b}, nil
}

func (f frame) encode(seq int64) ([]byte, error) {
	return json.Marshal(OutboundMessage{Type: f.msgType, Seq: seq, Data: f.data})
}

// Inbound types
const (
	TypePing        = "ping"
	TypeSubscribe   = "subscribe"
	TypeUnsubscribe = "unsubscribe"
	TypeTypingStart = "typing.start"
	TypeTypingStop  = "typing.stop"
	TypePresence    = "presence.update"

	// Collaboration. collab.update and collab.awareness are also outbound
	// types: an update is relayed to the room verbatim under the same name,
	// which is what lets a client use one apply path for its own echo and for
	// everyone else's.
	TypeCollabJoin      = "collab.join"
	TypeCollabLeave     = "collab.leave"
	TypeCollabUpdate    = "collab.update"
	TypeCollabAwareness = "collab.awareness"
)

// isCollabStreamType reports whether an inbound frame is part of the continuous
// editing stream, which is charged to the larger collaboration budget.
//
// collab.join and collab.leave are deliberately absent: a join costs two
// Postgres queries — more than a subscribe — and happens once per document per
// session, so it belongs on the general budget with everything else that hits
// the database. Only the two frames a person generates by typing get the higher
// rate.
func isCollabStreamType(t string) bool {
	switch t {
	case TypeCollabUpdate, TypeCollabAwareness:
		return true
	}
	return false
}

// Outbound types
const (
	TypePong            = "pong"
	TypeHello           = "hello"
	TypeSubscribed      = "subscribed"
	TypeUnsubscribed    = "unsubscribed"
	TypeMessageNew      = "message.new"
	TypeMessageUpdated  = "message.updated"
	TypeMessageDeleted  = "message.deleted"
	TypeReactionAdded   = "reaction.added"
	TypeReactionRemoved = "reaction.removed"
	TypeChannelCreated  = "channel.created"
	TypeChannelUpdated  = "channel.updated"
	TypeMemberJoined    = "member.joined"
	TypeMemberLeft      = "member.left"
	TypeTypingIndicator = "typing.indicator"
	TypePresenceChanged = "presence.changed"
	TypeNotificationNew = "notification.new"
	TypeUnreadUpdate    = "unread.update"
	TypeError           = "error"

	// Huddles. Delivered to a channel's SUBSCRIBERS, which is the point:
	// subscribing requires read on the channel, so a stranger never learns a
	// call exists. Somebody who can read a public channel but has not
	// subscribed learns about it from GET /channels/{id}/huddle instead — the
	// pull path, which authorizes per request.
	TypeHuddleStarted = "huddle.started"
	TypeHuddleEnded   = "huddle.ended"
	TypeHuddleRoster  = "huddle.roster"

	TypeCollabJoined = "collab.joined"
	TypeCollabLeft   = "collab.left"
	// TypeCollabCompact asks one client in the room to produce a snapshot of
	// the document and POST it back, because the server cannot merge CRDT
	// state itself.
	TypeCollabCompact = "collab.compact"
)

// Reasons carried by an "unsubscribed" frame.
const (
	ReasonClient  = "client"  // the client asked to unsubscribe
	ReasonRevoked = "revoked" // the server revoked the subscription
)

type SubscribeData struct {
	ChannelID string `json:"channel_id"`
}

// TypingData is the payload of typing.start / typing.stop. It is the same shape
// as SubscribeData and is kept as an alias so the two can never drift.
type TypingData = SubscribeData

type PresenceData struct {
	Status string `json:"status"`
}

// CollabDocumentData is the payload of collab.join and collab.leave.
type CollabDocumentData struct {
	DocumentID string `json:"document_id"`
}

// CollabUpdateData carries one opaque CRDT update. encoding/json renders a
// []byte as a base64 string, which is the wire form the Yjs client sends and
// the only reason the payload can ride a JSON frame at all.
type CollabUpdateData struct {
	DocumentID string `json:"document_id"`
	Update     []byte `json:"update"`
}

// CollabAwarenessData carries one awareness (cursor/selection/presence) state.
// It is relayed and never stored.
type CollabAwarenessData struct {
	DocumentID string `json:"document_id"`
	State      []byte `json:"state"`
}
