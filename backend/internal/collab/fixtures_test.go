package collab

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/ws"
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

// newDriveFile returns the id of a real files row.
//
// It used to be uuid.NewString(). Migration 025 closed the foreign key that
// migration 015 left open on purpose — collab_documents.resource_id REFERENCES
// files(id) — so a document about a resource that does not exist is no longer
// storable. That is the point of the FK: without it, purging a file leaves its
// update log behind as user-content Postgres bytes with nothing pointing at
// them, invisible to the object GC because they are not objects.
func (f *fixture) newDriveFile(t *testing.T) string {
	t.Helper()
	var id string
	err := f.pool.QueryRow(context.Background(),
		`INSERT INTO files (workspace_id, user_id, name, file_type, content_type, size_bytes, storage_key)
		 VALUES ($1, $2, 'doc', 'document', 'application/vnd.superops.document', 0, '')
		 RETURNING id`, f.workspaceID, f.owner).Scan(&id)
	if err != nil {
		t.Fatalf("create drive file: %v", err)
	}
	return id
}
