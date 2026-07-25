package collab

import (
	"errors"
	"time"
)

const (
	// MaxUpdateBytes is the hard ceiling on one update, mirroring the CHECK
	// constraint on collab_updates.payload. The WebSocket path enforces a much
	// smaller bound; this one is the HTTP append path, which exists for a large
	// paste and for an offline client flushing a backlog.
	MaxUpdateBytes = 1 << 20 // 1 MiB
	// MaxSnapshotBytes mirrors the CHECK on collab_snapshots.payload.
	MaxSnapshotBytes = 16 << 20 // 16 MiB

	// DefaultCompactionThreshold is how many uncompacted updates a document may
	// accumulate before the server asks a client for a snapshot. Low enough
	// that a reload is a snapshot plus a short tail; high enough that a busy
	// document is not asking someone to upload a full copy every few seconds.
	DefaultCompactionThreshold = 256

	// compactionCooldown stops a document that nobody snapshots (a client that
	// ignores the request) from being asked on every single append.
	compactionCooldown = 60 * time.Second

	// snapshotRetention is how many snapshots are kept per document. More than
	// one because a snapshot comes from a client and a bad one would otherwise
	// be the only copy of the document; bounded because each is up to 16 MiB.
	snapshotRetention = 3

	// maxStateUpdates bounds one GET /state response. A document whose log has
	// grown past this is paged rather than returned in one body, so a client
	// with a very stale watermark cannot ask the server to materialise an
	// unbounded response.
	maxStateUpdates = 2000
)

// ErrStaleSnapshot is returned when a submitted snapshot no longer describes a
// useful position: another compaction won, or it claims to cover updates that
// do not exist. It is not a failure — the client simply lost a race — so it is
// reported as a conflict rather than an error.
var ErrStaleSnapshot = errors.New("snapshot is stale")

// ErrResourceConflict is returned when the requested (resource_type,
// resource_id) already has a collaborative document in a different workspace.
// It means the caller's workspace id and the object's do not agree, which is a
// bug in the caller, not something to paper over by returning the other
// workspace's document.
var ErrResourceConflict = errors.New("collaboration document belongs to another workspace")

// Document is one collaboratively edited object.
type Document struct {
	ID           string  `json:"id"`
	WorkspaceID  string  `json:"workspace_id"`
	ResourceType string  `json:"resource_type"`
	ResourceID   string  `json:"resource_id"`
	HeadSeq      int64   `json:"head_seq"`
	SnapshotSeq  int64   `json:"snapshot_seq"`
	CreatedBy    *string `json:"created_by,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Update is one opaque CRDT update. Payload is rendered as base64 by
// encoding/json, which is the wire form the client already speaks.
type Update struct {
	Seq       int64     `json:"seq"`
	ActorID   *string   `json:"actor_id,omitempty"`
	Payload   []byte    `json:"payload"`
	CreatedAt time.Time `json:"created_at"`
}

// State is what a client needs to reconstruct a document from a given
// watermark: a snapshot when its watermark predates one, and the updates after
// that.
type State struct {
	DocumentID string `json:"document_id"`
	// Snapshot is nil when the caller's watermark is already at or past the
	// newest snapshot — the common case for a short reconnect, which then costs
	// a handful of small rows rather than a full document copy.
	Snapshot    []byte   `json:"snapshot,omitempty"`
	SnapshotSeq int64    `json:"snapshot_seq"`
	Updates     []Update `json:"updates"`
	// ThroughSeq is what the client's watermark becomes once it has applied
	// everything in this response.
	ThroughSeq int64 `json:"through_seq"`
	// HeadSeq is the log head at read time. Informational: it can already be
	// stale, and the updates that made it stale are being delivered live.
	HeadSeq int64 `json:"head_seq"`
	// HasMore reports that the response was truncated and the caller must ask
	// again with since=ThroughSeq.
	HasMore bool `json:"has_more"`
}
