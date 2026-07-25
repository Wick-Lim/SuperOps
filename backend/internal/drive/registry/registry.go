// Package registry is the editor registry — what happens when you open a Drive
// object (docs/plans/02-drive.md §4, ROADMAP §3b).
//
// It is the smallest thing in Phase 1 and the one three later phases cannot
// start without, which is why it is written before the CRUD around it.
//
// IT IS TWO REGISTRIES JOINED BY ONE STRING.
//
//	server (here) — type to LIFECYCLE POLICY: where the bytes live, whether a
//	                new version can be posted, what "new" creates, how it is
//	                indexed.
//	client        — type to SURFACE: which component opens the pane.
//
// The string is `file_type`, and it is already constrained identically in two
// tables — files.file_type and collab_documents.resource_type — and appears a
// third time as search.ObjectType. One vocabulary, three tables; the test in
// this package asserts every registered Kind has a matching search.ObjectType
// so the three cannot drift silently.
//
// EACH EDITOR PHASE ADDS ONE Kind AND ONE CLIENT COMPONENT, and gets storage,
// ACLs, sharing, versions, trash, search and activity for free. A later editor
// that needs a new field on Kind is the signal that this contract was
// under-specified — cheap to add now, expensive after three implementations.
//
// WHAT IT DELIBERATELY DOES NOT DO: permissions (that is internal/authz), the
// WebSocket protocol (internal/ws), or rendering.
package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
)

// StorageMode says where the truth is. It is the distinction the whole of Drive
// has to be honest about, because for three of the four types this registry
// will carry, the bytes in the bucket are NOT the document.
type StorageMode string

const (
	// StorageBlob: the object in storage is the truth. Uploads, images, PDFs.
	StorageBlob StorageMode = "blob"

	// StorageCollab: the append-only CRDT log in Postgres is the truth
	// (collab_updates / collab_snapshots, migration 015) and the server CANNOT
	// interpret it — migration 015's own header says a Go re-implementation that
	// disagreed with the client's would be "a corruption bug debuggable from
	// neither side". That decision is not reopened here.
	//
	// Everything downstream is written against "an object in a bucket", so this
	// mode is what forces each of those places to answer the question honestly
	// rather than serving a stale materialization and calling it the document.
	StorageCollab StorageMode = "collab"
)

// TypeFile is the fallback for anything uploaded. It is the only Kind that is
// guaranteed to exist.
const TypeFile = "file"

// ErrNoPreview is returned by Thumb for a type or a file that has none. It is
// not a failure — most files do not have a thumbnail — so the caller records
// "no preview" rather than retrying.
var ErrNoPreview = errors.New("no preview available for this object")

// typeShape is files.file_type's CHECK constraint, and collab_documents'
// resource_type's, spelled once more in Go. Registering a type the database
// would reject has to fail at boot rather than on the first insert.
var typeShape = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// NewRequest is what Kind.New is given. It runs inside the caller's transaction
// — the files row, the acl_object row, Kind.New and the audit entry are one
// commit, so a half-created document is not reachable.
type NewRequest struct {
	WorkspaceID string
	// FileID is the files row's id, already inserted. It is the stable identity
	// across versions and the id collab_documents.resource_id references.
	FileID string
	// FolderID is where it lives. Always set: a Drive object has a folder.
	FolderID string
	ActorID  string
	Name     string
}

// Preview is a generated thumbnail. internal/thumb decides the format; see
// Kind.Thumb.
type Preview struct {
	Bytes  []byte
	Width  int
	Height int
}

// Kind is one editor's whole server-side contract.
//
// The zero value is not registrable: Register validates, because a Kind with no
// Type or an unknown StorageMode is a programming error that must not become a
// runtime surprise on somebody's first "new document".
type Kind struct {
	// Type is the vocabulary word. It must satisfy typeShape and must be a
	// valid search.ObjectType — see the test in this package.
	Type string

	// DisplayName is what "New ..." says in the client.
	DisplayName string

	Storage StorageMode

	// Extensions and MimeTypes map an UPLOAD onto this type. Both are matched
	// case-insensitively; extensions include the dot.
	//
	// A collab type may claim extensions (an upload of a .superops-doc export),
	// but ForUpload never returns a collab Kind — see there for why.
	Extensions []string
	MimeTypes  []string

	// New creates the initial state of an empty object of this type, inside the
	// caller's transaction.
	//
	// For StorageBlob it writes an object (or nothing, for an empty file); for
	// StorageCollab it writes the collab_documents row. It must be idempotent
	// on (workspace, file id): a retried request must not create a second
	// document for the same file.
	//
	// nil means "nothing to create", which is correct for a plain uploaded file.
	New func(ctx context.Context, tx pgx.Tx, req NewRequest) error

	// Text returns indexable body text, or ("", nil) when the type has none.
	// Called by the worker, never in a request.
	//
	// nil means title-only indexing, which is what every blob type gets today
	// and what collab types get in v1 — a type can only implement this if it
	// ships a Go-side snapshot READER, and full-text search inside documents
	// belongs to the phase that owns the block model.
	Text func(ctx context.Context, src io.Reader) (string, error)

	// Thumb produces a preview image, or ErrNoPreview. internal/thumb decides the
	// format — image/jpeg today — and the thumbnail is the ONE object Drive
	// serves inline from a presigned URL, so what matters is that the type is
	// single, known-safe and produced by the server. It is NOT webp:
	// golang.org/x/image/webp is decode-only.
	Thumb func(ctx context.Context, src io.ReadSeeker) (Preview, error)

	// Versioned reports whether POST /content may create a new version.
	//
	// False for every collab type in v1. A PUT into a CRDT-backed object would
	// be silently discarded by the next merge, which is worse than refusing —
	// so the route answers 409 rather than accepting bytes it will drop.
	Versioned bool
}

// validate is what Register enforces. Separate so a test can state the rules.
func (k Kind) validate() error {
	if !typeShape.MatchString(k.Type) {
		return fmt.Errorf("registry: type %q does not match %s, which is files.file_type's "+
			"CHECK constraint — registering it would fail on the first insert", k.Type, typeShape)
	}
	if k.DisplayName == "" {
		return fmt.Errorf("registry: kind %q has no DisplayName; the client renders it in a menu", k.Type)
	}
	switch k.Storage {
	case StorageBlob, StorageCollab:
	default:
		return fmt.Errorf("registry: kind %q has storage mode %q, want %q or %q",
			k.Type, k.Storage, StorageBlob, StorageCollab)
	}
	if k.Storage == StorageCollab && k.Versioned {
		// Not a style rule. Versioning a collab type means POST /content
		// accepts bytes, and those bytes are discarded by the next CRDT merge.
		return fmt.Errorf("registry: kind %q is collab-backed and Versioned; a new version "+
			"posted to a CRDT-backed object is silently discarded by the next merge, so the "+
			"route must refuse it rather than accept it", k.Type)
	}
	if k.Storage == StorageCollab && k.New == nil {
		return fmt.Errorf("registry: kind %q is collab-backed but has no New; "+
			"nothing would create its collab_documents row and the editor would open on nothing", k.Type)
	}
	for _, ext := range k.Extensions {
		if !strings.HasPrefix(ext, ".") {
			return fmt.Errorf("registry: kind %q extension %q must start with a dot", k.Type, ext)
		}
	}
	return nil
}

// Registry is the set of registered kinds.
//
// A value rather than a package-level singleton, and Register is called
// explicitly from app.New rather than from init(), for the same reason the rest
// of the wiring is explicit: an editor that is not configured in a deployment
// must not be discoverable, and init() ordering is not where an
// authorization-adjacent decision belongs.
type Registry struct {
	mu    sync.RWMutex
	kinds map[string]Kind
	// order preserves registration order so GET /drive/registry is stable and
	// its golden test means something.
	order []string
}

// New returns a Registry with the fallback `file` kind already in it.
//
// TypeFile is not optional. ForUpload must never fail — an upload of an unknown
// type is a normal file, not an error — and that guarantee needs something to
// fall back to.
func New() *Registry {
	r := &Registry{kinds: map[string]Kind{}}
	must(r.Register(Kind{
		Type:        TypeFile,
		DisplayName: "File",
		Storage:     StorageBlob,
		Versioned:   true,
	}))
	return r
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// Register adds a kind. It returns an error rather than panicking so app.New
// can fail the boot with a message; a duplicate is an error because two editors
// claiming one type is a silent race over which one opens.
func (r *Registry) Register(k Kind) error {
	if err := k.validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.kinds[k.Type]; exists && k.Type != TypeFile {
		return fmt.Errorf("registry: kind %q is already registered", k.Type)
	}
	if _, exists := r.kinds[k.Type]; !exists {
		r.order = append(r.order, k.Type)
	}
	r.kinds[k.Type] = k
	return nil
}

// Lookup returns a registered kind.
func (r *Registry) Lookup(t string) (Kind, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	k, ok := r.kinds[t]
	return k, ok
}

// All returns every kind in registration order. It is what
// GET /api/v1/drive/registry serves, so the client hardcodes nothing.
func (r *Registry) All() []Kind {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Kind, 0, len(r.order))
	for _, t := range r.order {
		out = append(out, r.kinds[t])
	}
	return out
}

// ForUpload picks the kind for an uploaded file. It NEVER fails: an unknown
// extension is a plain file, which is the whole point of TypeFile.
//
// A COLLAB KIND IS NEVER RETURNED, whatever it claims. Uploading bytes cannot
// produce a CRDT-backed document — there is no server-side merge to feed them
// into — so a `document` kind that claimed ".md" would otherwise turn every
// uploaded markdown file into an empty document that silently discarded its
// contents. Import is a different operation and it belongs to the editor.
func (r *Registry) ForUpload(name, contentType string) Kind {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ext := strings.ToLower(path.Ext(name))
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))

	// Extension first: it is what the user sees, and a browser's guess at a
	// content type is frequently application/octet-stream.
	if ext != "" {
		for _, t := range r.order {
			k := r.kinds[t]
			if k.Storage == StorageCollab {
				continue
			}
			for _, e := range k.Extensions {
				if strings.EqualFold(e, ext) {
					return k
				}
			}
		}
	}
	if ct != "" {
		for _, t := range r.order {
			k := r.kinds[t]
			if k.Storage == StorageCollab {
				continue
			}
			for _, m := range k.MimeTypes {
				if strings.EqualFold(m, ct) {
					return k
				}
			}
		}
	}
	return r.kinds[TypeFile]
}

// Descriptor is the client-facing shape of a Kind, served by
// GET /api/v1/drive/registry. It is deliberately not Kind itself: the funcs
// would not marshal, and the field set is a published contract that should
// change only on purpose.
type Descriptor struct {
	Type        string   `json:"type"`
	DisplayName string   `json:"display_name"`
	StorageMode string   `json:"storage_mode"`
	Extensions  []string `json:"extensions"`
	MimeTypes   []string `json:"mime_types"`
	// Creatable is whether "New <DisplayName>" should appear. A plain file is
	// uploaded, not created.
	Creatable bool `json:"creatable"`
	Versioned bool `json:"versioned"`
	// Previewable is whether a thumbnail may exist for this type at all. The
	// client uses it to decide between a grid and a list, not to decide whether
	// to request one.
	Previewable bool `json:"previewable"`
}

// Descriptors renders All() for the API.
func (r *Registry) Descriptors() []Descriptor {
	kinds := r.All()
	out := make([]Descriptor, 0, len(kinds))
	for _, k := range kinds {
		// Non-nil even when empty. A nil slice marshals to `null`, and a client
		// that does `descriptor.extensions.map(...)` on the type with no
		// extensions — which is the fallback `file` kind, i.e. the one every
		// deployment has — throws. The copy is also what keeps the sort below
		// from reordering the registered Kind in place.
		exts := append([]string{}, k.Extensions...)
		mimes := append([]string{}, k.MimeTypes...)
		sort.Strings(exts)
		sort.Strings(mimes)
		out = append(out, Descriptor{
			Type:        k.Type,
			DisplayName: k.DisplayName,
			StorageMode: string(k.Storage),
			Extensions:  exts,
			MimeTypes:   mimes,
			// A plain file has nothing to create: "New File" would produce an
			// empty object with no way to put bytes in it except upload.
			Creatable:   k.New != nil,
			Versioned:   k.Versioned,
			Previewable: k.Thumb != nil,
		})
	}
	return out
}
