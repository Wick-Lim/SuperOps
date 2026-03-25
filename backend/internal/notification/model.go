package notification

import "time"

type Type string

const (
	TypeMention       Type = "mention"
	TypeDM            Type = "dm"
	TypeThreadReply   Type = "thread_reply"
	TypeChannelInvite Type = "channel_invite"
	TypeSystem        Type = "system"
)

type Notification struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Type      Type      `json:"type"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Data      string    `json:"data"`
	IsRead    bool      `json:"is_read"`
	CreatedAt time.Time `json:"created_at"`
}
