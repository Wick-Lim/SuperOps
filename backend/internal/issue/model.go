package issue

import (
	"errors"
	"regexp"
	"time"
)

// Sentinel errors. Each maps to exactly one status in the handler.
var (
	ErrNotFound        = errors.New("issue: not found")
	ErrCrossProject    = errors.New("issue: that state belongs to another project")
	ErrStateInUse      = errors.New("issue: that state still holds issues")
	ErrDuplicateKey    = errors.New("issue: a project with that key already exists in this workspace")
	ErrBadInput        = errors.New("issue: invalid input")
	ErrSelfRelation    = errors.New("issue: an issue cannot be related to itself")
	ErrProjectArchived = errors.New("issue: the project is archived")
)

// StateCategory is what everything generic reasons about — reports, workflow
// triggers, "is this done". The set is FIXED: a workflow that could invent a
// category is a workflow no report could summarise.
const (
	CategoryBacklog   = "backlog"
	CategoryUnstarted = "unstarted"
	CategoryStarted   = "started"
	CategoryCompleted = "completed"
	CategoryCancelled = "cancelled"
)

// DefaultStates are what a new project gets.
//
// A project with no states is a project nothing can be created in, so this is
// not a convenience — it is what makes "create a project" a complete action.
var DefaultStates = []struct {
	Name     string
	Category string
	Color    string
}{
	{"Backlog", CategoryBacklog, "#64748b"},
	{"Todo", CategoryUnstarted, "#94a3b8"},
	{"In Progress", CategoryStarted, "#f59e0b"},
	{"Done", CategoryCompleted, "#22c55e"},
	{"Cancelled", CategoryCancelled, "#ef4444"},
}

type Project struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	Name        string     `json:"name"`
	Key         string     `json:"key"`
	Description string     `json:"description"`
	ArchivedAt  *time.Time `json:"archived_at,omitempty"`
	CreatedBy   *string    `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

type State struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Name      string    `json:"name"`
	Category  string    `json:"category"`
	Position  int       `json:"position"`
	Color     string    `json:"color"`
	CreatedAt time.Time `json:"created_at"`
}

type Label struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
}

type Issue struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	ProjectID   string `json:"project_id"`
	Number      int    `json:"number"`
	// Key is "PROJ-14" — derived, never stored twice. It is what people paste
	// into chat, so it is in every payload rather than assembled by each client.
	Key         string  `json:"key"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	StateID     string  `json:"state_id"`
	AssigneeID  *string `json:"assignee_id"`
	CycleID     *string `json:"cycle_id"`
	Priority    int     `json:"priority"`
	Rank        string  `json:"rank"`
	// No omitempty: a nil pointer must serialise as `null` rather than vanish.
	// "Not completed" and "the server did not send this field" are different
	// facts, and a client that cannot tell them apart caches the stale one.
	CompletedAt *time.Time `json:"completed_at"`
	ArchivedAt  *time.Time `json:"archived_at"`
	CreatedBy   *string    `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`

	// LabelIDs is filled by the detail read, not by the list: a listing that
	// joined labels would multiply its own rows and break the keyset.
	LabelIDs []string `json:"label_ids,omitempty"`
}

// Activity is one entry in an issue's timeline.
//
// Distinct from internal/audit on purpose: that is a tamper-evident compliance
// record with a hash chain, and this is a product feature. Putting product
// history into the audit log makes the compliance surface a mixture of both.
type Activity struct {
	ID        string    `json:"id"`
	IssueID   string    `json:"issue_id"`
	ActorID   *string   `json:"actor_id"`
	Kind      string    `json:"kind"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	CreatedAt time.Time `json:"created_at"`
}

// projectKeyShape mirrors the column's CHECK. Spelled here so a bad key is a
// message rather than a constraint violation surfacing as a 500.
var projectKeyShape = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)
