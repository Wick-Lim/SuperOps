package ws

import "encoding/json"

type InboundMessage struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type OutboundMessage struct {
	Type string      `json:"type"`
	Seq  int64       `json:"seq"`
	Data interface{} `json:"data"`
}

// Inbound types
const (
	TypePing         = "ping"
	TypeSubscribe    = "subscribe"
	TypeUnsubscribe  = "unsubscribe"
	TypeTypingStart  = "typing.start"
	TypeTypingStop   = "typing.stop"
	TypePresence     = "presence.update"
)

// Outbound types
const (
	TypePong             = "pong"
	TypeHello            = "hello"
	TypeMessageNew       = "message.new"
	TypeMessageUpdated   = "message.updated"
	TypeMessageDeleted   = "message.deleted"
	TypeReactionAdded    = "reaction.added"
	TypeReactionRemoved  = "reaction.removed"
	TypeChannelCreated   = "channel.created"
	TypeChannelUpdated   = "channel.updated"
	TypeMemberJoined     = "member.joined"
	TypeMemberLeft       = "member.left"
	TypeTypingIndicator  = "typing.indicator"
	TypePresenceChanged  = "presence.changed"
	TypeNotificationNew  = "notification.new"
	TypeUnreadUpdate     = "unread.update"
	TypeError            = "error"
)

type SubscribeData struct {
	ChannelID string `json:"channel_id"`
}

type TypingData struct {
	ChannelID string `json:"channel_id"`
}

type PresenceData struct {
	Status string `json:"status"`
}
