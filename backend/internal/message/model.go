package message

import "time"

type Message struct {
	ID          string    `json:"id"`
	ChannelID   string    `json:"channel_id"`
	UserID      string    `json:"user_id"`
	ParentID    *string   `json:"parent_id,omitempty"`
	Content     string    `json:"content"`
	ContentType string    `json:"content_type"`
	IsEdited    bool      `json:"is_edited"`
	IsDeleted   bool      `json:"is_deleted"`
	ReplyCount  int       `json:"reply_count"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Reaction struct {
	ID        string    `json:"id"`
	MessageID string    `json:"message_id"`
	UserID    string    `json:"user_id"`
	Emoji     string    `json:"emoji"`
	CreatedAt time.Time `json:"created_at"`
}
