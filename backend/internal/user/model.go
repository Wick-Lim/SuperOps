package user

import "time"

type User struct {
	ID           string     `json:"id"`
	Email        string     `json:"email"`
	Username     string     `json:"username"`
	FullName     string     `json:"full_name"`
	PasswordHash string     `json:"-"`
	AvatarURL    string     `json:"avatar_url"`
	Timezone     string     `json:"timezone"`
	Locale       string     `json:"locale"`
	IsBot        bool       `json:"is_bot"`
	IsActive     bool       `json:"is_active"`
	LastActiveAt *time.Time `json:"last_active_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type PublicUser struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	FullName  string `json:"full_name"`
	AvatarURL string `json:"avatar_url"`
	IsBot     bool   `json:"is_bot"`
}

func (u *User) ToPublic() PublicUser {
	return PublicUser{
		ID:        u.ID,
		Username:  u.Username,
		FullName:  u.FullName,
		AvatarURL: u.AvatarURL,
		IsBot:     u.IsBot,
	}
}
