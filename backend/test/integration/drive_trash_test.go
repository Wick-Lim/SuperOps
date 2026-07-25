//go:build integration

package integration

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"
)

type trashEntry struct {
	Kind       string     `json:"kind"`
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	PurgeAfter *time.Time `json:"purge_after"`
	SizeBytes  int64      `json:"size_bytes"`
	ItemCount  int        `json:"item_count"`
}

func (h *harness) trash(t *testing.T, token, workspaceID string) []trashEntry {
	t.Helper()
	resp := h.req(t, http.StatusOK, http.MethodGet,
		"/api/v1/workspaces/"+workspaceID+"/drive/trash", token, nil)
	var out []trashEntry
	decodeInto(t, resp.Data, &out)
	return out
}

// The trash shows the TOP of each trashed tree and nothing below it. A folder
// whose parent is also trashed went with its ancestor; listing it separately
// offers a restore that cannot mean anything on its own, and shows somebody who
// deleted one folder a page of two hundred rows.
func TestDriveTrashListsOnlyTheTopOfEachTree(t *testing.T) {
	h := getHarness(t)
	tenant := h.newTenant(t, "trashtop")
	root := h.driveRoot(t, tenant.token, tenant.workspaceID)

	outer := h.createFolder(t, tenant.token, tenant.workspaceID, root.ID, "outer")
	inner := h.createFolder(t, tenant.token, tenant.workspaceID, outer.ID, "inner")
	code, resp := h.uploadToDrive(t, tenant.token, tenant.workspaceID, inner.ID,
		"deep.txt", bytes.Repeat([]byte("d"), 500))
	if code != http.StatusCreated {
		t.Fatalf("upload = %d (%+v)", code, resp.Error)
	}
	var deep driveDescriptor
	decodeInto(t, resp.Data, &deep)

	// One loose file at the root, so the listing has to distinguish the two
	// cases rather than passing by returning everything or nothing.
	code, resp = h.uploadToDrive(t, tenant.token, tenant.workspaceID, root.ID,
		"loose.txt", bytes.Repeat([]byte("l"), 300))
	if code != http.StatusCreated {
		t.Fatalf("upload = %d (%+v)", code, resp.Error)
	}
	var loose driveDescriptor
	decodeInto(t, resp.Data, &loose)

	h.req(t, http.StatusNoContent, http.MethodDelete, "/api/v1/drive/folders/"+outer.ID, tenant.token, nil)
	h.req(t, http.StatusNoContent, http.MethodDelete, "/api/v1/drive/files/"+loose.ID, tenant.token, nil)

	entries := h.trash(t, tenant.token, tenant.workspaceID)
	seen := map[string]trashEntry{}
	for _, e := range entries {
		seen[e.ID] = e
	}
	if _, ok := seen[outer.ID]; !ok {
		t.Error("the trashed folder is not in the trash")
	}
	if _, ok := seen[loose.ID]; !ok {
		t.Error("the trashed file is not in the trash")
	}
	if _, ok := seen[inner.ID]; ok {
		t.Error("a folder inside a trashed folder is listed separately; it went with its ancestor")
	}
	if _, ok := seen[deep.ID]; ok {
		t.Error("a file inside a trashed folder is listed separately")
	}
	if len(entries) != 2 {
		t.Errorf("trash = %d entries, want exactly the two tops: %+v", len(entries), entries)
	}

	// The deadline is stamped and shown. It is the promise made to the person
	// who deleted it, so it has to be in the payload rather than computed by a
	// client from a setting it cannot see.
	if seen[outer.ID].PurgeAfter == nil {
		t.Error("no purge_after on a trashed folder; the client cannot say when it goes")
	}
	// And what goes with it.
	if seen[outer.ID].ItemCount < 2 {
		t.Errorf("item_count = %d for a folder holding a folder and a file, want at least 2",
			seen[outer.ID].ItemCount)
	}
}

// Restore carries the subtree, or it restores an empty shell.
func TestDriveRestoreCarriesTheSubtree(t *testing.T) {
	h := getHarness(t)
	tenant := h.newTenant(t, "restore")
	root := h.driveRoot(t, tenant.token, tenant.workspaceID)

	outer := h.createFolder(t, tenant.token, tenant.workspaceID, root.ID, "project")
	inner := h.createFolder(t, tenant.token, tenant.workspaceID, outer.ID, "notes")
	code, resp := h.uploadToDrive(t, tenant.token, tenant.workspaceID, inner.ID,
		"file.txt", []byte("content"))
	if code != http.StatusCreated {
		t.Fatalf("upload = %d (%+v)", code, resp.Error)
	}
	var file driveDescriptor
	decodeInto(t, resp.Data, &file)

	h.req(t, http.StatusNoContent, http.MethodDelete, "/api/v1/drive/folders/"+outer.ID, tenant.token, nil)
	h.req(t, http.StatusOK, http.MethodPost,
		"/api/v1/drive/folder/"+outer.ID+"/restore", tenant.token, nil)

	// The folder is back...
	back := false
	for _, e := range h.children(t, tenant.token, root.ID) {
		if e.Folder != nil && e.Folder.ID == outer.ID {
			back = true
		}
	}
	if !back {
		t.Fatal("the restored folder is not under its parent")
	}
	// ...and so is everything under it.
	if e := h.children(t, tenant.token, outer.ID); len(e) != 1 || e[0].Folder == nil || e[0].Folder.ID != inner.ID {
		t.Errorf("the nested folder did not come back: %+v", e)
	}
	if e := h.children(t, tenant.token, inner.ID); len(e) != 1 || e[0].File == nil || e[0].File.ID != file.ID {
		t.Errorf("the file inside did not come back: %+v", e)
	}
	if len(h.trash(t, tenant.token, tenant.workspaceID)) != 0 {
		t.Error("the trash is not empty after restoring everything in it")
	}
}

// Restoring a folder whose PARENT is still trashed must land somewhere, and must
// SAY so. Refusing leaves the user with something they can see in the trash and
// cannot have.
//
// This is the reachable half of the "restore into a missing parent" case.
// A purged parent is NOT reachable through the product — Purge removes files
// before folders and both drive_folders.parent_id and files.folder_id are
// ON DELETE RESTRICT, so a row whose parent was destroyed cannot exist — and the
// code handles it anyway because "cannot exist" is a property of today's purge
// ordering rather than of the schema.
func TestDriveRestoreIntoATrashedParentLandsInTheRoot(t *testing.T) {
	h := getHarness(t)
	tenant := h.newTenant(t, "orphanrestore")
	root := h.driveRoot(t, tenant.token, tenant.workspaceID)

	outer := h.createFolder(t, tenant.token, tenant.workspaceID, root.ID, "outer")
	inner := h.createFolder(t, tenant.token, tenant.workspaceID, outer.ID, "inner")

	// Trash the inner one FIRST, so trashing the outer one skips it (the subtree
	// UPDATE is WHERE trashed_at IS NULL) and it keeps its own deadline.
	h.req(t, http.StatusNoContent, http.MethodDelete, "/api/v1/drive/folders/"+inner.ID, tenant.token, nil)
	h.req(t, http.StatusNoContent, http.MethodDelete, "/api/v1/drive/folders/"+outer.ID, tenant.token, nil)

	// Restore only the inner one. Its parent is still in the trash.
	resp := h.req(t, http.StatusOK, http.MethodPost,
		"/api/v1/drive/folder/"+inner.ID+"/restore", tenant.token, nil)
	var restored struct {
		FolderID  string `json:"folder_id"`
		Relocated bool   `json:"relocated"`
		Note      string `json:"note"`
	}
	decodeInto(t, resp.Data, &restored)

	if !restored.Relocated {
		t.Error("restoring into a trashed parent did not report a relocation; the user will " +
			"look for it where they deleted it")
	}
	if restored.FolderID != root.ID {
		t.Errorf("restored into %s, want the Drive root %s", restored.FolderID, root.ID)
	}
	if restored.Note == "" {
		t.Error("no note explaining where it went")
	}

	// And it is genuinely there, not merely reported as there.
	found := false
	for _, e := range h.children(t, tenant.token, root.ID) {
		if e.Folder != nil && e.Folder.ID == inner.ID {
			found = true
		}
	}
	if !found {
		t.Error("the relocated folder is not listed under the Drive root")
	}
}

// The purge removes rows, objects and bytes, and it must not race the object
// collector — internal/file's ListOrphans excludes trashed rows precisely so
// this job owns them.
func TestDriveTrashPurgeRemovesEverything(t *testing.T) {
	h := getHarness(t)
	tenant := h.newTenant(t, "purge")
	root := h.driveRoot(t, tenant.token, tenant.workspaceID)

	folder := h.createFolder(t, tenant.token, tenant.workspaceID, root.ID, "temp")
	body := bytes.Repeat([]byte("p"), 4096)
	code, resp := h.uploadToDrive(t, tenant.token, tenant.workspaceID, folder.ID, "gone.bin", body)
	if code != http.StatusCreated {
		t.Fatalf("upload = %d (%+v)", code, resp.Error)
	}
	var file driveDescriptor
	decodeInto(t, resp.Data, &file)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var storageKey string
	if err := h.app.DB.QueryRow(ctx,
		`SELECT storage_key FROM files WHERE id = $1`, file.ID).Scan(&storageKey); err != nil {
		t.Fatal(err)
	}

	before := h.usage(t, tenant.token, tenant.workspaceID)
	beforeBytes := int64(before["blob"].(map[string]any)["bytes"].(float64))

	h.req(t, http.StatusNoContent, http.MethodDelete, "/api/v1/drive/folders/"+folder.ID, tenant.token, nil)

	// Trashed is NOT purged: the bytes are still charged, which is what the
	// usage endpoint says out loud.
	mid := h.usage(t, tenant.token, tenant.workspaceID)
	if got := int64(mid["blob"].(map[string]any)["bytes"].(float64)); got != beforeBytes {
		t.Errorf("trashing changed bytes_used by %d; trashed files count", beforeBytes-got)
	}

	resp = h.req(t, http.StatusOK, http.MethodDelete,
		"/api/v1/workspaces/"+tenant.workspaceID+"/drive/trash", tenant.token, nil)
	var purged struct {
		Files      int   `json:"files_purged"`
		Folders    int   `json:"folders_purged"`
		BytesFreed int64 `json:"bytes_freed"`
	}
	decodeInto(t, resp.Data, &purged)
	if purged.Files != 1 || purged.Folders != 1 {
		t.Errorf("purged %d files and %d folders, want 1 and 1", purged.Files, purged.Folders)
	}
	if purged.BytesFreed != int64(len(body)) {
		t.Errorf("bytes_freed = %d, want %d", purged.BytesFreed, len(body))
	}

	// The row is gone.
	var n int
	if err := h.app.DB.QueryRow(ctx, `SELECT count(*) FROM files WHERE id = $1`, file.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("the purged file still has a row")
	}
	// The OBJECT is gone. A purge that only removed rows would be a storage
	// leak that the bucket sweep eventually finds — much later, and only if the
	// key's date has aged past the grace period.
	if _, err := h.storage(t).Head(ctx, storageKey); err == nil {
		t.Error("the purged file's object is still in the bucket")
	}
	// And the acl_object row went with the folder. acl_object.object_id is
	// polymorphic with no foreign key, and Rebuild prunes derived types only,
	// so nothing else would ever remove it — leaving a grant that answers the
	// "what could this person reach" audit with a folder that does not exist.
	if err := h.app.DB.QueryRow(ctx,
		`SELECT count(*) FROM acl_object WHERE object_type = 'folder' AND object_id = $1`,
		folder.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("the purged folder's acl_object row survived")
	}

	after := h.usage(t, tenant.token, tenant.workspaceID)
	if got := int64(after["blob"].(map[string]any)["bytes"].(float64)); got != beforeBytes-int64(len(body)) {
		t.Errorf("bytes_used = %d after the purge, want %d", got, beforeBytes-int64(len(body)))
	}
	if got := int64(after["breakdown"].(map[string]any)["drift_bytes"].(float64)); got != 0 {
		t.Errorf("drift_bytes = %d after a purge; the refund disagrees with the invariant", got)
	}
}
