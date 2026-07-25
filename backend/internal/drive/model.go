package drive

import (
	"errors"
	"time"
)

// Sentinel errors. Each maps to exactly one HTTP status in the handler, so
// `err != nil -> 500` and `!ok -> 403/404` stay uncollapsed.
var (
	// ErrNotFound is a folder or file that does not exist, or that the caller
	// may not read — the handler answers 404 for both, because "403" on an
	// object you cannot see tells you it exists.
	ErrNotFound = errors.New("drive: object not found")

	// ErrCycle is a move into the object's own subtree. It is a 409 rather than
	// a 400: the request is well-formed and would have been legal a moment ago.
	ErrCycle = errors.New("drive: a folder cannot be moved into its own subtree")

	// ErrTooDeep is a move that would push a descendant past the path depth cap.
	ErrTooDeep = errors.New("drive: the folder tree would exceed the maximum depth")

	// ErrCrossWorkspace is a move or an attach across tenants. Never a 404: it
	// is a bug in the caller, and answering "not found" would hide it.
	ErrCrossWorkspace = errors.New("drive: objects cannot move between workspaces")

	// ErrNoRoot means the workspace has no Drive root. Every workspace gets one
	// at creation and migration 025 backfilled the rest, so this is a repair
	// job rather than a user error.
	ErrNoRoot = errors.New("drive: workspace has no Drive root folder")

	// ErrTrashed is an operation on something already in the trash. Restore it
	// first; acting on it in place would leave the trash listing lying.
	ErrTrashed = errors.New("drive: object is in the trash")

	// ErrNotVersioned is POST /content against a collab-backed type. See
	// registry.Kind.Versioned: bytes posted to a CRDT-backed object are
	// discarded by the next merge, so refusing is the honest answer.
	ErrNotVersioned = errors.New("drive: this type's content cannot be replaced by upload")

	// ErrUnknownType is a file_type with no registered Kind.
	ErrUnknownType = errors.New("drive: unknown file type")
)

// Folder is a node in the tree.
type Folder struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	ParentID    *string    `json:"parent_id"`
	Name        string     `json:"name"`
	IsRoot      bool       `json:"is_root"`
	CreatedBy   *string    `json:"created_by"`
	TrashedAt   *time.Time `json:"trashed_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// File is a Drive object's metadata. It is deliberately not the open
// descriptor: this is what a listing returns, and a listing must not do the
// per-row work (a capability check, a presign) that opening one does.
type File struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	FolderID    *string    `json:"folder_id"`
	Name        string     `json:"name"`
	FileType    string     `json:"file_type"`
	ContentType string     `json:"content_type"`
	SizeBytes   int64      `json:"size_bytes"`
	Version     int        `json:"current_version"`
	HasThumb    bool       `json:"has_thumbnail"`
	CreatedBy   string     `json:"created_by"`
	TrashedAt   *time.Time `json:"trashed_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`

	// storageKey is unexported on purpose: an object key is not a capability,
	// but publishing it invites a client to construct one, and the bucket may
	// be reachable in ways the API is not.
	storageKey string
}

// Descriptor is what GET /drive/files/{id} returns — the whole dispatch
// contract in one payload (docs/plans/02-drive.md §4).
//
// The client looks up FileType in its own registry and renders. It never
// branches on MIME type, and it never has to know that a document is CRDT-backed
// except to pick a component.
type Descriptor struct {
	File

	// StorageMode is "blob" or "collab". It decides which of the two fields
	// below is populated; exactly one of them is always null.
	StorageMode string `json:"storage_mode"`

	// Capability is what the CALLER may do: "read", "comment", "write",
	// "share", "admin". Sent so the client can render a read-only surface
	// without a second round trip — and so it renders one at all, which a
	// client that only knew "you could open it" would not.
	Capability string `json:"capability"`

	// CollabDocumentID is set for storage_mode "collab": the room the editor
	// joins. Null for a blob.
	CollabDocumentID *string `json:"collab_document_id"`

	// ContentURL is set for storage_mode "blob": where the bytes are. Null for
	// a collab object, because there is no byte stream the server can produce
	// that IS the document — see plan 02 §8.
	ContentURL *string `json:"content_url"`

	// ThumbnailURL is set when a preview exists, for either mode.
	ThumbnailURL *string `json:"thumbnail_url"`

	// Projection is how far the client-published body has fallen behind the
	// CRDT log. Null for a blob kind, which has no projection at all — a zeroed
	// one would read as "projected, and empty".
	//
	// Not debug output. The client compares head_seq to projection_seq to
	// decide whether to re-project on open, and an operator reads the same gap
	// to see the pipeline failing before a user reports unsearchable documents.
	Projection *ProjectionStatus `json:"projection"`
}

// Entry is one row of a folder listing. Folders and files come back in one
// stream because that is how a file browser renders them, and paginating two
// lists against one scrollbar is how "the last folder is on page 3" happens.
type Entry struct {
	// Kind is "folder" or "file" — the discriminator the client switches on.
	Kind   string  `json:"kind"`
	Folder *Folder `json:"folder,omitempty"`
	File   *File   `json:"file,omitempty"`

	// sortAt and sortID carry the keyset cursor. Unexported: the cursor the
	// client gets is the opaque encoding, not these.
	sortAt time.Time
	sortID string
}

const (
	EntryFolder = "folder"
	EntryFile   = "file"
)

// MaxFolderDepth caps nesting. It is below internal/authz's MaxPathDepth (32
// segments) because a folder path also carries the workspace segment and,
// for a file, the file's own — so a folder at this depth still leaves room for
// the object inside it.
const MaxFolderDepth = 30

// MaxNameLength mirrors drive_folders.name's CHECK constraint.
const MaxNameLength = 255
