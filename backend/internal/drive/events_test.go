package drive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/drive/registry"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
)

// A DELETION WITH NO REVISION IS REFUSED, NOT DEFAULTED.
//
// The revision is the JetStream dedupe key. A constant one is the collapse the
// field exists to prevent — trash and purge shared a constant once and the
// destroyed document was never unindexed, answering searches for content that
// had been erased. The code defaulted an empty revision to "deleted", which
// would have reinstated exactly that for whichever caller forgot, silently.
//
// Refusing is the better trade: cmd/reindex converges a missing unindex, and
// nothing converges a collapsed one.
func TestPublishFileDeletionsRefusesAnEmptyRevision(t *testing.T) {
	var buf bytes.Buffer
	p := &Publisher{
		// A nil NATS client is fine: the point is that publishFile is never
		// reached, and reaching it with a nil client would panic — which would
		// itself be a failure this test reports.
		logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})),
	}

	p.PublishFileDeletions(context.Background(), []FileRef{
		{ID: "f-1", WorkspaceID: "w-1", FileType: "file"}, // no Revision
	})

	if !strings.Contains(buf.String(), "no revision") {
		t.Errorf("an empty revision was accepted silently; log was %q", buf.String())
	}
}

// And a ref with no id or workspace is skipped without a word, because that is
// an empty slot rather than a caller mistake.
func TestPublishFileDeletionsSkipsAnEmptyRef(t *testing.T) {
	var buf bytes.Buffer
	p := &Publisher{logger: slog.New(slog.NewTextHandler(&buf, nil))}
	p.PublishFileDeletions(context.Background(), []FileRef{{}})
	if buf.Len() != 0 {
		t.Errorf("an empty ref logged %q", buf.String())
	}
}

var testDBName = fmt.Sprintf("superops_drive_test_%d", os.Getpid())

var (
	dbOnce sync.Once
	dbPool *pgxpool.Pool
	dbErr  error
)

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

func requireInfra() bool {
	b, err := strconv.ParseBool(os.Getenv("SUPEROPS_REQUIRE_INFRA"))
	return err == nil && b
}

func dsn(database string) string {
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s",
		url.QueryEscape(env("DB_USER", "superops")),
		url.QueryEscape(env("DB_PASSWORD", "")),
		env("DB_HOST", "127.0.0.1"),
		env("DB_PORT", "5432"),
		database,
		env("DB_SSLMODE", "disable"),
	)
}

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		dbPool, dbErr = createTestDB(ctx)
	})
	if dbErr != nil {
		if requireInfra() {
			t.Fatalf("SUPEROPS_REQUIRE_INFRA=1 but Postgres is unusable: %v", dbErr)
		}
		t.Skipf("postgres unavailable, skipping drive database tests: %v", dbErr)
	}
	return dbPool
}

func createTestDB(ctx context.Context) (*pgxpool.Pool, error) {
	maint, err := pgx.Connect(ctx, dsn(env("DB_MAINTENANCE_NAME", "postgres")))
	if err != nil {
		return nil, fmt.Errorf("connect to maintenance database: %w", err)
	}
	defer maint.Close(ctx)

	if _, err := maint.Exec(ctx, `DROP DATABASE IF EXISTS "`+testDBName+`"`); err != nil {
		return nil, fmt.Errorf("drop stale test database: %w", err)
	}
	if _, err := maint.Exec(ctx, `CREATE DATABASE "`+testDBName+`"`); err != nil {
		return nil, fmt.Errorf("create test database: %w", err)
	}

	pool, err := pgxpool.New(ctx, dsn(testDBName))
	if err != nil {
		return nil, fmt.Errorf("open test database: %w", err)
	}
	dir := filepath.Join("..", "..", "migrations")
	names, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	sort.Strings(names)
	for _, name := range names {
		body, err := os.ReadFile(name)
		if err != nil {
			pool.Close()
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			pool.Close()
			return nil, fmt.Errorf("apply %s: %w", filepath.Base(name), err)
		}
	}
	return pool, nil
}

func TestMain(m *testing.M) {
	code := m.Run()
	if dbPool != nil {
		dbPool.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if maint, err := pgx.Connect(ctx, dsn(env("DB_MAINTENANCE_NAME", "postgres"))); err == nil {
			_, _ = maint.Exec(ctx, `DROP DATABASE IF EXISTS "`+testDBName+`" WITH (FORCE)`)
			_ = maint.Close(ctx)
		}
	}
	os.Exit(code)
}

func TestMoveFolderTransactional(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	userID := uuid.NewString()
	wsID := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, username, full_name, password_hash, is_active)
		 VALUES ($1, $2, $3, 'Drive Owner', 'h', true)`,
		userID, "drive-"+userID+"@example.com", "drv_"+strings.ReplaceAll(userID[:8], "-", ""),
	); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO workspaces (id, name, slug, owner_id) VALUES ($1, 'Drive WS', $2, $3)`,
		wsID, "drv-"+wsID[:8], userID,
	); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`,
		wsID, userID,
	); err != nil {
		t.Fatalf("insert workspace member: %v", err)
	}

	az := authz.New(pool)
	repo := NewRepository(pool, az, nil)

	root, err := repo.EnsureRoot(ctx, wsID, userID)
	if err != nil {
		t.Fatalf("EnsureRoot: %v", err)
	}
	folderA, err := repo.CreateFolder(ctx, wsID, root.ID, "Folder A", userID)
	if err != nil {
		t.Fatalf("CreateFolder A: %v", err)
	}
	folderB, err := repo.CreateFolder(ctx, wsID, root.ID, "Folder B", userID)
	if err != nil {
		t.Fatalf("CreateFolder B: %v", err)
	}

	// Move Folder A into Folder B.
	moved, err := repo.MoveFolder(ctx, userID, folderA.ID, folderB.ID)
	if err != nil {
		t.Fatalf("MoveFolder: %v", err)
	}
	if moved.ParentID == nil || *moved.ParentID != folderB.ID {
		t.Fatalf("moved.ParentID = %v, want %s", moved.ParentID, folderB.ID)
	}

	var aclPath string
	if err := pool.QueryRow(ctx,
		`SELECT path FROM acl_object WHERE object_type = 'folder' AND object_id = $1`,
		folderA.ID).Scan(&aclPath); err != nil {
		t.Fatalf("read acl_object path: %v", err)
	}
	if !strings.Contains(aclPath, "folder:"+folderB.ID) {
		t.Fatalf("acl_object.path = %q, want it under folder:%s", aclPath, folderB.ID)
	}

	// Canceled context must abort the transaction leaving both parent_id and path unchanged.
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := repo.MoveFolder(canceledCtx, userID, folderA.ID, root.ID); err == nil {
		t.Fatal("MoveFolder with canceled context succeeded")
	}

	afterCancel, err := repo.Folder(ctx, folderA.ID)
	if err != nil {
		t.Fatalf("Folder: %v", err)
	}
	if afterCancel.ParentID == nil || *afterCancel.ParentID != folderB.ID {
		t.Fatalf("parent_id after canceled move = %v, want %s", afterCancel.ParentID, folderB.ID)
	}
	var pathAfterCancel string
	if err := pool.QueryRow(ctx,
		`SELECT path FROM acl_object WHERE object_type = 'folder' AND object_id = $1`,
		folderA.ID).Scan(&pathAfterCancel); err != nil {
		t.Fatalf("read acl_object path: %v", err)
	}
	if pathAfterCancel != aclPath {
		t.Fatalf("acl_object.path after canceled move = %q, want %q", pathAfterCancel, aclPath)
	}

	// Trashed source folder cannot be moved and its ACL path must not change.
	if _, err := repo.TrashFolder(ctx, folderA.ID, userID); err != nil {
		t.Fatalf("TrashFolder: %v", err)
	}
	if _, err := repo.MoveFolder(ctx, userID, folderA.ID, root.ID); !errors.Is(err, ErrTrashed) {
		t.Fatalf("MoveFolder on trashed folder = %v, want ErrTrashed", err)
	}
	var pathAfterTrash string
	if err := pool.QueryRow(ctx,
		`SELECT path FROM acl_object WHERE object_type = 'folder' AND object_id = $1`,
		folderA.ID).Scan(&pathAfterTrash); err != nil {
		t.Fatalf("read acl_object path: %v", err)
	}
	if pathAfterTrash != aclPath {
		t.Fatalf("acl_object.path moved despite ErrTrashed: got %q, want %q", pathAfterTrash, aclPath)
	}
}

func TestShareLinkSessionTokenEndToEnd(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	ownerID := uuid.NewString()
	wsID := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, username, full_name, password_hash, is_active)
		 VALUES ($1, $2, $3, 'Link Owner', 'h', true)`,
		ownerID, "link-"+ownerID+"@example.com", "lnk_"+strings.ReplaceAll(ownerID[:8], "-", ""),
	); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO workspaces (id, name, slug, owner_id) VALUES ($1, 'Link WS', $2, $3)`,
		wsID, "lnk-"+wsID[:8], ownerID,
	); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`,
		wsID, ownerID,
	); err != nil {
		t.Fatalf("insert workspace member: %v", err)
	}

	az := authz.New(pool)
	kinds := registry.New()
	if err := kinds.Register(registry.Kind{
		Type:            "document",
		DisplayName:     "Document",
		Storage:         registry.StorageCollab,
		ClientProjected: true,
		New:             func(context.Context, pgx.Tx, registry.NewRequest) error { return nil },
	}); err != nil {
		t.Fatalf("register document kind: %v", err)
	}
	handler := NewHandler(pool, az, kinds, nil, nil, nil, nil)
	handler.SetLinkSecret("test-share-link-secret-key-32bytes!")

	mux := http.NewServeMux()
	authMw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			uid := r.Header.Get("X-Test-User")
			if uid == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(authctx.WithUserID(r.Context(), uid)))
		})
	}
	handler.RegisterRoutes(mux, authMw)
	handler.RegisterLinkRoutes(mux, func(next http.Handler) http.Handler { return next })

	root, err := handler.repo.EnsureRoot(ctx, wsID, ownerID)
	if err != nil {
		t.Fatalf("EnsureRoot: %v", err)
	}
	folder, err := handler.repo.CreateFolder(ctx, wsID, root.ID, "Shared Folder", ownerID)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	file, err := handler.repo.CreateFile(ctx, wsID, folder.ID, "Roadmap", "document", ownerID)
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}

	// 1. Owner creates a read-only link on the folder.
	createReq := httptest.NewRequest(http.MethodPost,
		"/api/v1/drive/folder/"+folder.ID+"/links",
		strings.NewReader(`{"capability":"read"}`))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("X-Test-User", ownerID)
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("CreateLink status = %d, body = %s", createRec.Code, createRec.Body.String())
	}

	var createEnv struct {
		Data struct {
			Link  Link   `json:"link"`
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &createEnv); err != nil {
		t.Fatalf("decode CreateLink: %v", err)
	}
	if createEnv.Data.Token == "" || createEnv.Data.Link.ID == "" {
		t.Fatalf("missing token or link id: %+v", createEnv)
	}

	// 2. Anonymous caller resolves the link and receives session_token.
	resolveReq := httptest.NewRequest(http.MethodPost,
		"/api/v1/drive/links/"+createEnv.Data.Token+"/resolve",
		strings.NewReader(`{}`))
	resolveRec := httptest.NewRecorder()
	mux.ServeHTTP(resolveRec, resolveReq)
	if resolveRec.Code != http.StatusOK {
		t.Fatalf("ResolveLink status = %d, body = %s", resolveRec.Code, resolveRec.Body.String())
	}
	var resolveEnv struct {
		Data struct {
			SessionToken string `json:"session_token"`
			ObjectID     string `json:"object_id"`
			Capability   string `json:"capability"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resolveRec.Body.Bytes(), &resolveEnv); err != nil {
		t.Fatalf("decode ResolveLink: %v", err)
	}
	if resolveEnv.Data.SessionToken == "" {
		t.Fatal("ResolveLink did not return session_token")
	}

	// 3. Anonymous holder reads the shared folder, lists its children, and opens the child file.
	for _, path := range []string{
		"/api/v1/drive/folders/" + folder.ID,
		"/api/v1/drive/folders/" + folder.ID + "/children",
		"/api/v1/drive/files/" + file.ID,
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+resolveEnv.Data.SessionToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s with link session token status = %d, body = %s", path, rec.Code, rec.Body.String())
		}
	}

	// 4. Anonymous holder cannot read the workspace root folder outside the shared subtree.
	rootReq := httptest.NewRequest(http.MethodGet, "/api/v1/drive/folders/"+root.ID, nil)
	rootReq.Header.Set("Authorization", "Bearer "+resolveEnv.Data.SessionToken)
	rootRec := httptest.NewRecorder()
	mux.ServeHTTP(rootRec, rootReq)
	if rootRec.Code != http.StatusNotFound {
		t.Fatalf("GET unshared root folder status = %d, want 404", rootRec.Code)
	}

	// 5. Revoking the link immediately invalidates the issued session_token.
	revokeReq := httptest.NewRequest(http.MethodDelete, "/api/v1/drive/links/"+createEnv.Data.Link.ID, nil)
	revokeReq.Header.Set("X-Test-User", ownerID)
	revokeRec := httptest.NewRecorder()
	mux.ServeHTTP(revokeRec, revokeReq)
	if revokeRec.Code != http.StatusNoContent {
		t.Fatalf("RevokeLink status = %d, body = %s", revokeRec.Code, revokeRec.Body.String())
	}

	afterRevokeReq := httptest.NewRequest(http.MethodGet, "/api/v1/drive/files/"+file.ID, nil)
	afterRevokeReq.Header.Set("Authorization", "Bearer "+resolveEnv.Data.SessionToken)
	afterRevokeRec := httptest.NewRecorder()
	mux.ServeHTTP(afterRevokeRec, afterRevokeReq)
	if afterRevokeRec.Code != http.StatusUnauthorized {
		t.Fatalf("GET after RevokeLink status = %d, want 401", afterRevokeRec.Code)
	}
}
