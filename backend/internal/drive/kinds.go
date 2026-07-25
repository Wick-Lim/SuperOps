package drive

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Wick-Lim/SuperOps/backend/internal/drive/registry"
)

// The kinds this deployment registers.
//
// Register is called from here rather than from init() so that an editor which
// is not built into a deployment is not discoverable in it. Each later phase
// adds one function below and one client component; nothing else in Drive
// changes.

// DocumentKind is the block editor.
//
// It began as a stub — a collab_documents row and nothing else — so that every
// Drive endpoint had to answer "what does this mean for a thing with no bytes?"
// in code rather than in a design document. That worked: download, versioning,
// quota accounting, copy and purge each got a real answer in Phase 1 instead of
// a surprise in Phase 3.
//
// What it gains here is the projection. NOT Text, which the stub's comment
// anticipated "once there is a Go-side snapshot reader" — there will not be
// one. There is no io.Reader that IS a CRDT document, and reconstructing a
// ProseMirror tree from collab_snapshots in Go is the same class of work
// migration 015 refuses for the same reason: an implementation that disagreed
// with the client's would be a corruption bug debuggable from neither side.
// registry.validate now refuses the combination outright.
func DocumentKind(collab CollabCreator) registry.Kind {
	return registry.Kind{
		Type:        "document",
		DisplayName: "Document",
		Storage:     registry.StorageCollab,
		// Deliberately no Extensions: ForUpload never returns a collab kind, and
		// claiming ".md" here would only mislead a reader into thinking it did.
		New: func(ctx context.Context, tx pgx.Tx, req registry.NewRequest) error {
			// resource_type IS file_type — one vocabulary, not a parallel one
			// (ROADMAP §3b). The FK added in migration 025 means this row cannot
			// outlive its file.
			if err := collab.EnsureDocumentTx(ctx, tx, req.WorkspaceID, "document", req.FileID, req.ActorID); err != nil {
				return fmt.Errorf("create collaborative document for file %s: %w", req.FileID, err)
			}
			return nil
		},
		// Versioned is false and validate() enforces that for every collab kind:
		// POST /content answers 409 rather than accepting bytes the next merge
		// would discard.
		Versioned: false,

		// The body comes from the editor that has the document in memory, over
		// POST /drive/files/{id}/projection, and is stored as derived state.
		// This is what makes the document findable by its text; without it the
		// index holds a title and nothing else.
		ClientProjected: true,
	}
}

// CollabCreator is the part of internal/collab that Drive needs.
//
// An interface so the dependency runs drive -> interface <- collab, matching
// the shape already used for collab -> ws. Drive must not import collab
// directly: the editors are meant to be additions to Drive, not the other way
// round, and an import in that direction would make removing one a change here.
type CollabCreator interface {
	// EnsureDocumentTx creates the collaborative document for a resource inside
	// the caller's transaction, idempotently. In the transaction because the
	// files row, the acl_object row and this are one commit — a document that
	// existed for a file that did not would be reachable by id and belong to
	// nobody.
	EnsureDocumentTx(ctx context.Context, tx pgx.Tx, workspaceID, resourceType, resourceID, createdBy string) error
}
