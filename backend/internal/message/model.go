package message

import "time"

type Message struct {
	ID          string     `json:"id"`
	ChannelID   string     `json:"channel_id"`
	UserID      string     `json:"user_id"`
	ParentID    *string    `json:"parent_id,omitempty"`
	Content     string     `json:"content"`
	ContentType string     `json:"content_type"`
	IsEdited    bool       `json:"is_edited"`
	IsDeleted   bool       `json:"is_deleted"`
	ReplyCount  int        `json:"reply_count"`
	IsPinned    bool       `json:"is_pinned"`
	PinnedBy    *string    `json:"pinned_by,omitempty"`
	PinnedAt    *time.Time `json:"pinned_at,omitempty"`
	ScheduledAt *time.Time `json:"scheduled_at,omitempty"`
	// IsScheduled must not be omitempty: a client cannot tell "not scheduled"
	// from "field missing", and the same applies to the two slices below —
	// omitempty made them vanish instead of serializing as [].
	IsScheduled bool        `json:"is_scheduled"`
	Metadata    Metadata    `json:"metadata"`
	Reactions   []*Reaction `json:"reactions"`
	Files       []*FileRef  `json:"files"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

// Metadata is the typed view of messages.metadata (JSONB). It carries
// provenance that must survive a round-trip through the database rather than
// being encoded into the message text.
type Metadata struct {
	ForwardedFrom *ForwardRef `json:"forwarded_from,omitempty"`
}

// ForwardRef attributes a forwarded copy to the message it was copied from, so
// a client can render "forwarded from @author in #channel" instead of trusting
// a "[Forwarded] " prefix that any user could type by hand.
type ForwardRef struct {
	MessageID string    `json:"message_id"`
	ChannelID string    `json:"channel_id"`
	UserID    string    `json:"user_id"`
	CreatedAt time.Time `json:"created_at"`
}

// FileRef is a lightweight view of a file attached to a message.
type FileRef struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`

	// FileType is the EDITOR KIND — "file" for an ordinary upload, "document",
	// "spreadsheet" or "design" for a Drive object somebody attached.
	//
	// It is here because the search index derives a document id from it, and the
	// re-index published when a message claims its attachments used to hardcode
	// "file". A Drive document attached to a message then got a SECOND index
	// document written beside its own, and search returned the same file twice —
	// the twin sweep only removes the untyped copy, never the other way round.
	FileType string `json:"file_type"`
}

type Reaction struct {
	ID        string    `json:"id"`
	MessageID string    `json:"message_id"`
	UserID    string    `json:"user_id"`
	Emoji     string    `json:"emoji"`
	CreatedAt time.Time `json:"created_at"`
}

// Bookmark is a saved message together with the time the caller saved it.
type Bookmark struct {
	Message      *Message  `json:"message"`
	BookmarkedAt time.Time `json:"bookmarked_at"`
}
