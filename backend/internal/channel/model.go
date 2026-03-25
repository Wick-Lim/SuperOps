package channel

import "time"

type ChannelType string

const (
	TypePublic  ChannelType = "public"
	TypePrivate ChannelType = "private"
	TypeDM      ChannelType = "dm"
	TypeGroupDM ChannelType = "group_dm"
)

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
