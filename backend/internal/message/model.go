package message

import "time"

type Message struct {
	ID          string       `json:"id"`
	ChannelID   string       `json:"channel_id"`
	UserID      string       `json:"user_id"`
	ParentID    *string      `json:"parent_id,omitempty"`
	Content     string       `json:"content"`
	ContentType string       `json:"content_type"`
	IsEdited    bool         `json:"is_edited"`
	IsDeleted   bool         `json:"is_deleted"`
	ReplyCount  int          `json:"reply_count"`
	IsPinned    bool         `json:"is_pinned"`
	PinnedBy    *string      `json:"pinned_by,omitempty"`
	PinnedAt    *time.Time   `json:"pinned_at,omitempty"`
	ScheduledAt *time.Time   `json:"scheduled_at,omitempty"`
	IsScheduled bool         `json:"is_scheduled,omitempty"`
	Reactions   []*Reaction  `json:"reactions,omitempty"`
	Files       []*FileRef   `json:"files,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

// FileRef is a lightweight view of a file attached to a message.
type FileRef struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}

type Reaction struct {
	ID        string    `json:"id"`
	MessageID string    `json:"message_id"`
	UserID    string    `json:"user_id"`
	Emoji     string    `json:"emoji"`
	CreatedAt time.Time `json:"created_at"`
}
