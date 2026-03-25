package presence

type Status string

const (
	StatusOnline  Status = "online"
	StatusAway    Status = "away"
	StatusDND     Status = "dnd"
	StatusOffline Status = "offline"
)
