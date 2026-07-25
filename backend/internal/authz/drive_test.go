package authz

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Drive's permission model, asserted at the layer that decides it.
//
// Migration 025 makes three claims that no other test in this package covers,
// and each is one UNION arm away from being wrong in a way that reads as
// correct:
//
//  1. Drive is the workspace's shared drive — the same statement a public
//     channel makes — so the root folder is not invisible and a new folder is
//     not private by accident.
//  2. The list path and the decision path agree. acl_key_expected arm 6 and
//     derivedCapability's TypeFolder case are one rule written twice; when they
//     disagree, a listing shows what opening refuses, or the reverse.
//  3. Folders are ACL-NATIVE: Register and Move own their acl_object row, which
//     is what makes moving a folder with a large subtree one prefix rewrite.
//
// There is no private folder in v1 — see driveCapability for why that is a cut
// rather than an omission — so nothing here asserts one.

// driveWorld is a Drive tree inside the shared fixture's wsA:
//
//	Drive (root)
//	├── Team     — open.txt
//	└── Private  — held.txt, reachable only by a grant
//
// Built once, like world: the root folder is unique per workspace, so a
// per-test seed would collide on the second test.
type driveWorld struct {
	root, team, private string
	openFile, heldFile  string
}

var (
	driveOnce sync.Once
	driveFix  *driveWorld
)

func seedDrive(t *testing.T, c *Checker, w *world) *driveWorld {
	t.Helper()
	driveOnce.Do(func() {
		d := &driveWorld{}
		d.root = insertFolder(t, c, w.wsA, nil, "Drive", true, w.owner)

		// Creating a workspace's Drive is three writes, and this is the third:
		// the row, the acl_object registration, and the grant that makes it
		// shared. Without it the root is an ordinary ACL-native object, which is
		// deny-by-default, and Drive is invisible to everyone. Migration 025
		// backfills exactly this row for workspaces that already existed;
		// workspace.Handler.Create writes it for the ones that come after.
		if err := c.Grant(context.Background(), UserSubject(w.owner),
			WorkspaceSubject(w.wsA), FolderObject(d.root), CapWrite); err != nil {
			t.Fatalf("grant the workspace read on its own Drive: %v", err)
		}

		d.team = insertFolder(t, c, w.wsA, &d.root, "Team", false, w.owner)
		d.private = insertFolder(t, c, w.wsA, &d.root, "Private", false, w.owner)

		d.openFile = insertDriveFile(t, c, w.wsA, w.owner, d.team, "open.txt")
		d.heldFile = insertDriveFile(t, c, w.wsA, w.owner, d.private, "held.txt")

		// Files are derived, so their acl_object rows and every acl_key row come
		// from the views. Registering the folders above wrote the native half;
		// this materializes the rest exactly as the hourly job does.
		if _, err := c.Rebuild(context.Background()); err != nil {
			t.Fatalf("rebuild: %v", err)
		}
		driveFix = d
	})
	if driveFix == nil {
		t.Fatal("drive fixture unavailable")
	}
	return driveFix
}

// insertFolder writes the drive_folders row and registers the ACL object, which
// is the pair internal/drive will do in one transaction. Register is used
// rather than a hand-written acl_object INSERT precisely so a regression that
// made folders derived — and therefore un-registerable — fails here.
func insertFolder(t *testing.T, c *Checker, workspaceID string, parent *string, name string, isRoot bool, creator string) string {
	t.Helper()
	ctx := context.Background()

	var id string
	err := c.pool.QueryRow(ctx,
		`INSERT INTO drive_folders (workspace_id, parent_id, name, is_root, created_by)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		workspaceID, parent, name, isRoot, creator).Scan(&id)
	if err != nil {
		t.Fatalf("insert folder %q: %v", name, err)
	}

	parentRef := WorkspaceObject(workspaceID)
	if parent != nil {
		parentRef = FolderObject(*parent)
	}
	if err := c.Register(ctx, FolderObject(id), parentRef); err != nil {
		t.Fatalf("register folder %q: %v", name, err)
	}
	return id
}

func insertDriveFile(t *testing.T, c *Checker, workspaceID, userID, folderID, name string) string {
	t.Helper()
	var id string
	err := c.pool.QueryRow(context.Background(),
		`INSERT INTO files (workspace_id, user_id, folder_id, name, content_type, size_bytes, storage_key)
		 VALUES ($1, $2, $3, $4, 'text/plain', 12, $5) RETURNING id`,
		workspaceID, userID, folderID, name, "k-"+name+"-"+folderID).Scan(&id)
	if err != nil {
		t.Fatalf("insert drive file %q: %v", name, err)
	}
	return id
}

func TestDriveIsReadableByTheWorkspaceAndNobodyElse(t *testing.T) {
	c, w := rebuilt(t)
	d := seedDrive(t, c, w)
	ctx := context.Background()

	objects := []struct {
		label string
		obj   ObjectRef
	}{
		{"root folder", FolderObject(d.root)},
		{"subfolder", FolderObject(d.team)},
		{"file in a folder", FileObject(d.openFile)},
	}

	// The guest belongs to no channel at all, so anything they can read here is
	// Drive's doing and not a channel membership leaking through.
	for _, o := range objects {
		got, err := c.Can(ctx, UserSubject(w.guest), o.obj, CapRead)
		if err != nil {
			t.Fatalf("%s: %v", o.label, err)
		}
		if !got {
			t.Errorf("a workspace member cannot read the %s; Drive is invisible to everyone but its creator", o.label)
		}
	}

	// A member writes by default — Drive is shared, not read-only.
	got, err := c.Can(ctx, UserSubject(w.member), FolderObject(d.team), CapWrite)
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("a member cannot write to the shared drive")
	}
	// ...but a guest does not.
	got, err = c.Can(ctx, UserSubject(w.guest), FolderObject(d.team), CapWrite)
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("a guest can write to the shared drive")
	}

	// Tenancy: the other tenant's owner shares no membership with wsA.
	for _, o := range objects {
		got, err := c.Can(ctx, UserSubject(w.stranger), o.obj, CapRead)
		if err != nil {
			t.Fatalf("%s: %v", o.label, err)
		}
		if got {
			t.Errorf("another tenant can read the %s", o.label)
		}
	}
}

// A grant is how Drive shares outside the default, and it has to reach the
// whole subtree — that is what the materialized path is for.
func TestGrantReachesTheWholeSubtree(t *testing.T) {
	c, w := rebuilt(t)
	d := seedDrive(t, c, w)
	ctx := context.Background()

	// The stranger is in wsB only, so nothing but an explicit grant can give
	// them anything here.
	before, err := c.Can(ctx, UserSubject(w.stranger), FileObject(d.heldFile), CapRead)
	if err != nil {
		t.Fatal(err)
	}
	if before {
		t.Fatal("the fixture proves nothing: the stranger could already read it")
	}

	if err := c.Grant(ctx, UserSubject(w.owner), UserSubject(w.stranger),
		FolderObject(d.private), CapRead); err != nil {
		t.Fatalf("grant: %v", err)
	}

	for _, obj := range []ObjectRef{FolderObject(d.private), FileObject(d.heldFile)} {
		got, err := c.Can(ctx, UserSubject(w.stranger), obj, CapRead)
		if err != nil {
			t.Fatalf("%s: %v", obj, err)
		}
		if !got {
			t.Errorf("a grant on the folder did not reach %s", obj)
		}
	}

	// A read grant must not have handed over write.
	got, err := c.Can(ctx, UserSubject(w.stranger), FileObject(d.heldFile), CapWrite)
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("a read grant conferred write")
	}
}

// KeysFor is the list path: children, trash and search all filter on it rather
// than calling Can per row. If it disagrees with Can, listings show a different
// set than opening allows — and the direction that matters is the wider one.
func TestDriveKeysForAgreesWithCan(t *testing.T) {
	c, w := rebuilt(t)
	d := seedDrive(t, c, w)
	ctx := context.Background()

	keys, err := c.KeysFor(ctx, UserSubject(w.guest), w.wsA)
	if err != nil {
		t.Fatal(err)
	}

	// Every folder the guest can read must contribute its container key, or the
	// files inside it vanish from every listing while remaining openable.
	for _, folder := range []string{d.root, d.team, d.private} {
		key := ContainerKey(FolderObject(folder))
		if key == "" {
			t.Fatal("ContainerKey returned empty for a folder — the f- prefix is not registered")
		}
		canRead, err := c.Can(ctx, UserSubject(w.guest), FolderObject(folder), CapRead)
		if err != nil {
			t.Fatal(err)
		}
		if got := slices.Contains(keys, key); got != canRead {
			t.Errorf("folder %s: KeysFor has its key = %v but Can(read) = %v", folder, got, canRead)
		}
	}

	// And every key is one internal/search will accept. A key that fails its
	// validator is DROPPED from the filter, and a dropped narrowing term widens
	// the query — for a tenancy filter that is a cross-tenant leak.
	for _, k := range keys {
		if !looksLikeAccessKey(k) {
			t.Errorf("KeysFor returned %q, which internal/search's validKey would drop", k)
		}
	}
}

// looksLikeAccessKey mirrors internal/search's validKey without importing it
// (search imports authz, so the dependency only runs one way).
func looksLikeAccessKey(k string) bool {
	if len(k) < 3 || k[1] != '-' {
		return false
	}
	if !slices.Contains([]byte{'w', 'c', 'u', 'g', 'f'}, k[0]) {
		return false
	}
	canonical, ok := canonicalUUID(k[2:])
	return ok && canonical == k[2:]
}

// A folder is registered and moved through this API; the three derived types
// are not. Getting this backwards is silent: Register would succeed, the next
// Rebuild would revert the row, and the folder would relocate itself.
func TestFolderIsNativeAndTheDerivedTypesAreNot(t *testing.T) {
	c, w := rebuilt(t)
	d := seedDrive(t, c, w)
	ctx := context.Background()

	// Moving a folder rewrites its subtree's paths. A file's path is derived
	// from its folder's, so it must follow — a file that kept the old one would
	// authorize against where its folder used to be, which is how a move out of
	// a shared folder fails to take the sharing with it.
	moved := insertFolder(t, c, w.wsA, &d.root, "Moving", false, w.owner)
	inside := insertDriveFile(t, c, w.wsA, w.owner, moved, "inside.txt")
	if _, err := c.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}

	if err := c.Move(ctx, UserSubject(w.owner), FolderObject(moved), FolderObject(d.team)); err != nil {
		t.Fatalf("move a folder: %v — Drive cannot organise a tree it cannot move", err)
	}

	var path string
	if err := c.pool.QueryRow(ctx,
		`SELECT path FROM acl_object WHERE object_type = 'folder' AND object_id = $1`,
		moved).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if want := pathSegment(FolderObject(d.team)); !strings.Contains(path, want) {
		t.Errorf("after the move the folder's path is %q, which does not pass through %s", path, want)
	}
	st, err := c.resolve(ctx, FileObject(inside))
	if err != nil {
		t.Fatal(err)
	}
	if st == nil || !strings.HasPrefix(st.path, path) {
		t.Errorf("the file inside the moved folder resolves under %q, not under its folder's new path %q",
			st.path, path)
	}

	for _, obj := range []ObjectRef{
		WorkspaceObject(w.wsA), ChannelObject(w.chPublic), FileObject(d.openFile),
	} {
		err := c.Register(ctx, obj, WorkspaceObject(w.wsA))
		if err == nil {
			t.Errorf("Register(%s) succeeded; a hand-placed row for a derived type is "+
				"reverted by the next Rebuild, so this must fail loudly instead", obj)
			continue
		}
		if !errors.Is(err, ErrDerivedType) {
			t.Errorf("Register(%s) failed with %v, want ErrDerivedType", obj, err)
		}
	}
}
