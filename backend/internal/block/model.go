package block

import "time"

// Block represents a user-to-user block relationship.
type Block struct {
	BlockedID string    `json:"blocked_id"`
	CreatedAt time.Time `json:"created_at"`
}
