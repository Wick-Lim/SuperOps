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

// StubDocumentKind is the collab-backed type that exists BEFORE the Docs phase.
//
// docs/plans/02-drive.md §8 asks for exactly this, and the reasoning is worth
// keeping: everything in Drive is written against "an object in a bucket", and
// for a CRDT-backed type that is false. Download, versioning, quota accounting,
// search extraction, copy and purge each need a different answer, and a design
// document cannot force them to have one.
//
// A stub `document` — a collab_documents row and nothing else, no editor, no
// client component — makes every Drive endpoint answer the question in code.
// Anything that only works because "the file has bytes" fails loudly against
// it, in Phase 1, where fixing it costs a day. Discovered in Phase 3 it costs a
// schema change plus three editors' worth of client code.
//
// The Docs phase replaces this with the real Kind: same Type, same
// StorageMode, plus Text once there is a Go-side snapshot reader.
func StubDocumentKind(collab CollabCreator) registry.Kind {
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
