package presence

type Status string

const (
	StatusOnline  Status = "online"
	StatusAway    Status = "away"
	StatusDND     Status = "dnd"
	StatusOffline Status = "offline"
)

// ParseStatus validates a client-supplied status against the enum. Anything
// else would be stored verbatim under presence:{userID} and echoed to every
// workspace member by GET /workspaces/{id}/presence.
func ParseStatus(s string) (Status, bool) {
	switch Status(s) {
	case StatusOnline, StatusAway, StatusDND, StatusOffline:
		return Status(s), true
	default:
		return "", false
	}
}
