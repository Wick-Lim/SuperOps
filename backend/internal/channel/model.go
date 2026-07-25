package channel

import "time"

type ChannelType string

const (
	TypePublic  ChannelType = "public"
	TypePrivate ChannelType = "private"
	TypeDM      ChannelType = "dm"
	TypeGroupDM ChannelType = "group_dm"
)

// IsDM reports whether the channel is a conversation between a fixed set of
// participants. DMs have no join/leave/add-member semantics: the roster is
// decided at creation time and never changes.
func (t ChannelType) IsDM() bool { return t == TypeDM || t == TypeGroupDM }

// Channel roles. Mirrors the CHECK constraint on channel_members.role.
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// Notification preferences. Mirrors the CHECK constraint on
// channel_members.notification_pref.
const (
	NotifyAll      = "all"
	NotifyMentions = "mentions"
	NotifyNone     = "none"
	NotifyDefault  = "default"
)

func validNotificationPref(p string) bool {
	switch p {
	case NotifyAll, NotifyMentions, NotifyNone, NotifyDefault:
		return true
	}
	return false
}

// DMKey renders the canonical channels.dm_key for a 1:1 DM: the two participant
// ids sorted lexicographically and joined with ':'. It must produce the same
// string regardless of argument order, and must match the expression migration
// 009 used to backfill the column (MIN(user_id::text) || ':' || MAX(...)), or
// the partial unique index on (workspace_id, dm_key) will not dedupe.
func DMKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + ":" + b
}

type Channel struct {
	ID            string      `json:"id"`
	WorkspaceID   string      `json:"workspace_id"`
	Name          *string     `json:"name"`
	Slug          *string     `json:"slug"`
	Description   string      `json:"description"`
	Type          ChannelType `json:"type"`
	Topic         string      `json:"topic"`
	IsArchived    bool        `json:"is_archived"`
	CreatorID     *string     `json:"creator_id"`
	LastMessageAt *time.Time  `json:"last_message_at,omitempty"`
	CreatedAt     time.Time   `json:"created_at"`
	UpdatedAt     time.Time   `json:"updated_at"`

	// UnreadCount is not a column of `channels`; it is computed per caller and
	// populated only by ListByWorkspaceAndUser. Every other path leaves it at 0.
	UnreadCount int `json:"unread_count"`
}

type ChannelMember struct {
	ChannelID        string    `json:"channel_id"`
	UserID           string    `json:"user_id"`
	Role             string    `json:"role"`
	LastReadAt       time.Time `json:"last_read_at"`
	Muted            bool      `json:"muted"`
	NotificationPref string    `json:"notification_pref"`
	JoinedAt         time.Time `json:"joined_at"`
}

// Unread is the payload of GET /api/v1/channels/{channel_id}/unread.
type Unread struct {
	ChannelID  string    `json:"channel_id"`
	Count      int       `json:"unread_count"`
	LastReadAt time.Time `json:"last_read_at"`
}
