package user

import "time"

// Status field limits, mirroring the CHECK constraint added in migration 009.
const (
	MaxStatusTextLen  = 100
	MaxStatusEmojiLen = 64
)

type User struct {
	ID           string     `json:"id"`
	Email        string     `json:"email"`
	Username     string     `json:"username"`
	FullName     string     `json:"full_name"`
	PasswordHash string     `json:"-"`
	AvatarURL    string     `json:"avatar_url"`
	StatusText   string     `json:"status_text"`
	StatusEmoji  string     `json:"status_emoji"`
	Timezone     string     `json:"timezone"`
	Locale       string     `json:"locale"`
	IsBot        bool       `json:"is_bot"`
	IsActive     bool       `json:"is_active"`
	LastActiveAt *time.Time `json:"last_active_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type PublicUser struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	FullName    string `json:"full_name"`
	AvatarURL   string `json:"avatar_url"`
	StatusText  string `json:"status_text"`
	StatusEmoji string `json:"status_emoji"`
	IsBot       bool   `json:"is_bot"`
}

func (u *User) ToPublic() PublicUser {
	return PublicUser{
		ID:          u.ID,
		Username:    u.Username,
		FullName:    u.FullName,
		AvatarURL:   u.AvatarURL,
		StatusText:  u.StatusText,
		StatusEmoji: u.StatusEmoji,
		IsBot:       u.IsBot,
	}
}
