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
)

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
