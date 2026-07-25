package collab

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/ws"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fixture is one workspace with one account per role, plus a stranger who is a
// member of nothing. Every test builds its own so they can run in any order and
// so a test that revokes access cannot affect another.
type fixture struct {
	pool *pgxpool.Pool
	repo *Repository
	svc  *Service
	hub  *ws.Hub

	workspaceID string
	owner       string // workspace owner: read + write
	member      string // plain member: read + write
	guest       string // guest: read only
	stranger    string // member of nothing
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := testDB(t)
	ctx := context.Background()

	f := &fixture{
		pool:     pool,
		repo:     NewRepository(pool),
		owner:    createUser(t, pool),
		member:   createUser(t, pool),
		guest:    createUser(t, pool),
		stranger: createUser(t, pool),
	}

	if err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (name, slug, owner_id) VALUES ($1, $2, $3) RETURNING id`,
		"collab test", "collab-"+uuid.NewString(), f.owner,
	).Scan(&f.workspaceID); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	for userID, role := range map[string]string{
		f.owner:  authz.RoleOwner,
		f.member: authz.RoleMember,
		f.guest:  authz.RoleGuest,
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, $3)`,
			f.workspaceID, userID, role,
		); err != nil {
			t.Fatalf("add workspace member: %v", err)
		}
	}

	f.hub = ws.NewHub(testLogger())
	go f.hub.Run()
	t.Cleanup(f.hub.Shutdown)

	f.svc = NewService(f.repo, NewWorkspaceAuthorizer(authz.New(pool)), f.hub, testLogger())
	return f
}

func createUser(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, email, username) VALUES ($1, $2, $3)`,
		id, id+"@example.test", "u"+id[:8]+id[9:13],
	); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return id
}

// newDocument creates a collaborative document for a Drive object.
func (f *fixture) newDocument(t *testing.T) *Document {
	t.Helper()
	doc, err := f.repo.EnsureDocument(context.Background(), f.workspaceID, "document", f.newDriveFile(t), f.owner)
	if err != nil {
		t.Fatalf("create collaboration document: %v", err)
	}
	return doc
}

// newDriveFile returns the id of a real files row IN THE WORKSPACE'S DRIVE.
//
// Two things force that shape, and both are load-bearing.
//
// Migration 025 closed the foreign key migration 015 left open —
// collab_documents.resource_id REFERENCES files(id) — so a document about a
// resource that does not exist is no longer storable. Without it, purging a file
// leaves its update log behind as user-content Postgres bytes with nothing
// pointing at them, invisible to the object GC because they are not objects.
//
// And the file has to be IN A FOLDER, because WorkspaceAuthorizer now
// authorizes a document against its resource rather than against its workspace.
// A files row with no folder and no message is readable by its uploader alone
// (that is file.Handler.canRead, transcribed), so a fixture that skipped the
// folder would give every collaborative document a one-person audience — which
// is a correct answer to the wrong question, and not what a Drive document is.
func (f *fixture) newDriveFile(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	// The workspace's Drive, created the way workspace creation does it: the
	// row, the ACL object, and the grant that makes it shared.
	var rootID string
	err := f.pool.QueryRow(ctx,
		`INSERT INTO drive_folders (workspace_id, name, is_root, created_by)
		 VALUES ($1, 'Drive', TRUE, $2)
		 ON CONFLICT (workspace_id) WHERE is_root DO NOTHING
		 RETURNING id::text`, f.workspaceID, f.owner).Scan(&rootID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := f.pool.QueryRow(ctx,
			`SELECT id::text FROM drive_folders WHERE workspace_id = $1 AND is_root`,
			f.workspaceID).Scan(&rootID); err != nil {
			t.Fatalf("read drive root: %v", err)
		}
	} else if err != nil {
		t.Fatalf("create drive root: %v", err)
	} else {
		az := authz.New(f.pool)
		if err := az.Register(ctx, authz.FolderObject(rootID), authz.WorkspaceObject(f.workspaceID)); err != nil {
			t.Fatalf("register drive root: %v", err)
		}
		if err := az.Grant(ctx, authz.UserSubject(f.owner),
			authz.WorkspaceSubject(f.workspaceID), authz.FolderObject(rootID), authz.CapAdmin); err != nil {
			t.Fatalf("share drive root: %v", err)
		}
	}

	var id string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO files (workspace_id, user_id, folder_id, name, file_type, content_type, size_bytes, storage_key)
		 VALUES ($1, $2, $3, 'doc', 'document', 'application/vnd.superops.document', 0, '')
		 RETURNING id`, f.workspaceID, f.owner, rootID).Scan(&id); err != nil {
		t.Fatalf("create drive file: %v", err)
	}
	// The file is derived, so its ACL comes from the views. Materializing it now
	// is what the handler does in the same transaction as the insert.
	if err := database.WithTx(ctx, f.pool, func(tx pgx.Tx) error {
		return authz.MaterializeTx(ctx, tx, authz.FileObject(id))
	}); err != nil {
		t.Fatalf("materialize drive file ACL: %v", err)
	}
	return id
}
