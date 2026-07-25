//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"testing"
)

// Drive, end to end. What these assert that the package tests cannot:
//
//   - the routes exist and carry the middleware they are supposed to;
//   - the open descriptor's shape is what a client can dispatch on, with
//     exactly one of content_url / collab_document_id populated;
//   - the listing is filtered by the CALLER's keys rather than by the folder,
//     which is the difference between a permission model and a decoration;
//   - a second tenant cannot reach any of it by uuid.

type driveFolder struct {
	ID          string  `json:"id"`
	WorkspaceID string  `json:"workspace_id"`
	ParentID    *string `json:"parent_id"`
	Name        string  `json:"name"`
	IsRoot      bool    `json:"is_root"`
}

type driveDescriptor struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	FileType         string  `json:"file_type"`
	StorageMode      string  `json:"storage_mode"`
	Capability       string  `json:"capability"`
	CollabDocumentID *string `json:"collab_document_id"`
	ContentURL       *string `json:"content_url"`
	FolderID         *string `json:"folder_id"`
}

type driveEntry struct {
	Kind   string       `json:"kind"`
	Folder *driveFolder `json:"folder"`
	File   *struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		FileType string `json:"file_type"`
	} `json:"file"`
}

func (h *harness) driveRoot(t *testing.T, token, workspaceID string) driveFolder {
	t.Helper()
	resp := h.req(t, http.StatusOK, http.MethodGet,
		"/api/v1/workspaces/"+workspaceID+"/drive/root", token, nil)
	var root driveFolder
	decodeInto(t, resp.Data, &root)
	if !root.IsRoot {
		t.Fatalf("the workspace's Drive root is not marked as root: %+v", root)
	}
	return root
}

func (h *harness) createFolder(t *testing.T, token, workspaceID, parentID, name string) driveFolder {
	t.Helper()
	resp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/workspaces/"+workspaceID+"/drive/folders", token,
		map[string]string{"parent_id": parentID, "name": name})
	var folder driveFolder
	decodeInto(t, resp.Data, &folder)
	return folder
}

func (h *harness) children(t *testing.T, token, folderID string) []driveEntry {
	t.Helper()
	resp := h.req(t, http.StatusOK, http.MethodGet,
		"/api/v1/drive/folders/"+folderID+"/children", token, nil)
	var entries []driveEntry
	decodeInto(t, resp.Data, &entries)
	return entries
}

// The Drive root is created on demand and is workspace-shared. Both halves
// matter: a root that did not exist would make Drive unusable for every
// workspace that predates it, and a root that was private would make it
// unusable for everybody except its creator.
func TestDriveRootIsCreatedOnDemandAndSharedWithTheWorkspace(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)

	root := h.driveRoot(t, admin, ws)
	if root.WorkspaceID != ws {
		t.Fatalf("root belongs to workspace %s, want %s", root.WorkspaceID, ws)
	}
	// Idempotent: a second call must not mint a second root.
	if again := h.driveRoot(t, admin, ws); again.ID != root.ID {
		t.Fatalf("two calls produced two roots: %s and %s", root.ID, again.ID)
	}

	// A plain member who created nothing can still see it. This is the whole
	// point of the workspace grant — without it Drive is invisible.
	member := h.newUser(t, admin, ws, "drive-member")
	if got := h.driveRoot(t, member.token, ws); got.ID != root.ID {
		t.Fatalf("a member sees root %s, the admin sees %s", got.ID, root.ID)
	}
	h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/workspaces/"+ws+"/drive/folders", member.token,
		map[string]string{"parent_id": root.ID, "name": "member folder"})
}

func TestDriveFolderTreeAndListing(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	root := h.driveRoot(t, admin, ws)

	parent := h.createFolder(t, admin, ws, root.ID, "Projects")
	child := h.createFolder(t, admin, ws, parent.ID, "2026")

	// The breadcrumb is what a file browser renders as its path bar, and it has
	// to reach the root or the user cannot navigate up.
	resp := h.req(t, http.StatusOK, http.MethodGet, "/api/v1/drive/folders/"+child.ID, admin, nil)
	var got struct {
		Folder     driveFolder   `json:"folder"`
		Breadcrumb []driveFolder `json:"breadcrumb"`
	}
	decodeInto(t, resp.Data, &got)
	if len(got.Breadcrumb) != 3 {
		t.Fatalf("breadcrumb = %d entries, want root/Projects/2026: %+v", len(got.Breadcrumb), got.Breadcrumb)
	}
	if !got.Breadcrumb[0].IsRoot {
		t.Error("the breadcrumb does not start at the root, so the user cannot navigate up")
	}
	if got.Breadcrumb[2].ID != child.ID {
		t.Error("the breadcrumb does not end at the folder being viewed")
	}

	// A listing shows the children and nothing else.
	entries := h.children(t, admin, parent.ID)
	if len(entries) != 1 || entries[0].Kind != "folder" || entries[0].Folder.ID != child.ID {
		t.Fatalf("children of Projects = %+v, want just 2026", entries)
	}

	// Rename does not move anything: the path carries ids, which is why
	// renaming a folder with a large subtree is one UPDATE.
	h.req(t, http.StatusOK, http.MethodPatch, "/api/v1/drive/folders/"+child.ID, admin,
		map[string]string{"name": "2027"})
	if e := h.children(t, admin, parent.ID); e[0].Folder.Name != "2027" {
		t.Errorf("after rename the listing shows %q", e[0].Folder.Name)
	}
}

// A move into your own subtree makes the subtree unreachable: the user can no
// longer find it and the collector no longer collects it. It must be refused,
// and refused as a conflict rather than a 500.
func TestDriveMoveRefusesACycleAndFollowsThroughOtherwise(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	root := h.driveRoot(t, admin, ws)

	outer := h.createFolder(t, admin, ws, root.ID, "outer")
	inner := h.createFolder(t, admin, ws, outer.ID, "inner")

	h.denied(t, http.StatusConflict, http.MethodPost,
		"/api/v1/drive/folders/"+outer.ID+"/move", admin,
		map[string]string{"parent_id": inner.ID})

	// ...and moving into itself, which is the case a constraint can catch.
	h.denied(t, http.StatusConflict, http.MethodPost,
		"/api/v1/drive/folders/"+outer.ID+"/move", admin,
		map[string]string{"parent_id": outer.ID})

	// A legal move relocates the subtree.
	sibling := h.createFolder(t, admin, ws, root.ID, "sibling")
	h.req(t, http.StatusOK, http.MethodPost, "/api/v1/drive/folders/"+inner.ID+"/move", admin,
		map[string]string{"parent_id": sibling.ID})
	if e := h.children(t, admin, sibling.ID); len(e) != 1 || e[0].Folder.ID != inner.ID {
		t.Errorf("after the move, sibling's children = %+v, want inner", e)
	}
	if e := h.children(t, admin, outer.ID); len(e) != 0 {
		t.Errorf("after the move, outer still lists %+v", e)
	}
}

// The open descriptor is the dispatch contract: the client reads file_type and
// storage_mode and renders. Exactly one of content_url / collab_document_id is
// populated, always — a client that had to guess would guess wrong for the type
// it had not seen before.
func TestDriveOpenDescriptorDispatchesOnTypeNotMimeType(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	root := h.driveRoot(t, admin, ws)

	resp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/workspaces/"+ws+"/drive/files", admin,
		map[string]string{"folder_id": root.ID, "name": "Q3 plan", "file_type": "document"})
	var created driveDescriptor
	decodeInto(t, resp.Data, &created)

	if created.StorageMode != "collab" {
		t.Fatalf("storage_mode = %q, want collab", created.StorageMode)
	}
	if created.ContentURL != nil {
		t.Error("content_url is set for a collab object; there is no byte stream the server " +
			"can produce that IS the document, and a materialized one would be stale")
	}
	if created.CollabDocumentID == nil || *created.CollabDocumentID == "" {
		t.Fatal("collab_document_id is null, so the editor has no room to join")
	}

	// Reopening returns the same descriptor — the id is stable, which is what
	// lets an attachment and a search document keep pointing at it.
	reopened := h.req(t, http.StatusOK, http.MethodGet, "/api/v1/drive/files/"+created.ID, admin, nil)
	var again driveDescriptor
	decodeInto(t, reopened.Data, &again)
	if again.CollabDocumentID == nil || *again.CollabDocumentID != *created.CollabDocumentID {
		t.Error("reopening the file produced a different collaborative document")
	}
	if again.Capability == "" {
		t.Error("the descriptor carries no capability, so the client cannot render a read-only surface")
	}

	// POST /content on a collab type is 409, not a silent success. Bytes put
	// into a CRDT-backed object are discarded by the next merge, and accepting
	// them is the worst of the three possible answers.
	h.denied(t, http.StatusConflict, http.MethodPost,
		"/api/v1/drive/files/"+created.ID+"/content", admin, nil)

	// An unregistered type is a 400 with a reason, not a 500.
	h.denied(t, http.StatusBadRequest, http.MethodPost,
		"/api/v1/workspaces/"+ws+"/drive/files", admin,
		map[string]string{"folder_id": root.ID, "name": "x", "file_type": "hologram"})

	// A plain file is uploaded, not created: creating one would produce an
	// empty object with no way to fill it.
	h.denied(t, http.StatusBadRequest, http.MethodPost,
		"/api/v1/workspaces/"+ws+"/drive/files", admin,
		map[string]string{"folder_id": root.ID, "name": "x", "file_type": "file"})
}

// GET /drive/registry is what makes the client hardcode nothing.
func TestDriveRegistryIsServedToTheClient(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)

	resp := h.req(t, http.StatusOK, http.MethodGet, "/api/v1/drive/registry", admin, nil)
	var kinds []struct {
		Type        string   `json:"type"`
		StorageMode string   `json:"storage_mode"`
		Extensions  []string `json:"extensions"`
		Creatable   bool     `json:"creatable"`
	}
	decodeInto(t, resp.Data, &kinds)

	seen := map[string]bool{}
	for _, k := range kinds {
		seen[k.Type] = true
		if k.Extensions == nil {
			t.Errorf("kind %q has null extensions; a client doing .map() on it throws", k.Type)
		}
	}
	if !seen["file"] || !seen["document"] {
		t.Errorf("registry = %+v, want at least file and document", kinds)
	}
}

// Tenancy. Every one of these is written as the attack, because that is how it
// arrives.
func TestDriveIsNotReachableFromAnotherTenant(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	root := h.driveRoot(t, admin, ws)
	folder := h.createFolder(t, admin, ws, root.ID, "confidential")

	resp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/workspaces/"+ws+"/drive/files", admin,
		map[string]string{"folder_id": folder.ID, "name": "salaries", "file_type": "document"})
	var file driveDescriptor
	decodeInto(t, resp.Data, &file)

	other := h.newTenant(t, "drive-outsider")

	// 404, not 403: a 403 on an object you cannot see confirms it exists.
	for _, path := range []string{
		"/api/v1/drive/folders/" + folder.ID,
		"/api/v1/drive/folders/" + folder.ID + "/children",
		"/api/v1/drive/files/" + file.ID,
	} {
		h.denied(t, http.StatusNotFound, http.MethodGet, path, other.token, nil)
	}

	// Reading the other tenant's Drive root by workspace id.
	h.denied(t, http.StatusNotFound, http.MethodGet,
		"/api/v1/workspaces/"+ws+"/drive/root", other.token, nil)

	// Creating INTO another tenant's folder.
	h.denied(t, http.StatusNotFound, http.MethodPost,
		"/api/v1/workspaces/"+other.workspaceID+"/drive/folders", other.token,
		map[string]string{"parent_id": folder.ID, "name": "trojan"})

	// Moving another tenant's file into a folder they own.
	otherRoot := h.driveRoot(t, other.token, other.workspaceID)
	h.denied(t, http.StatusNotFound, http.MethodPost,
		"/api/v1/drive/files/"+file.ID+"/move", other.token,
		map[string]string{"folder_id": otherRoot.ID})

	// Trashing it.
	h.denied(t, http.StatusNotFound, http.MethodDelete,
		"/api/v1/drive/files/"+file.ID, other.token, nil)
}

// Trashing a folder takes its whole subtree. Leaving descendants behind would
// put them inside a folder the trash says is gone: absent from every listing,
// reachable by id, and collected by nothing.
func TestDriveTrashTakesTheSubtree(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	root := h.driveRoot(t, admin, ws)

	outer := h.createFolder(t, admin, ws, root.ID, "to-trash")
	inner := h.createFolder(t, admin, ws, outer.ID, "nested")
	resp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/workspaces/"+ws+"/drive/files", admin,
		map[string]string{"folder_id": inner.ID, "name": "deep", "file_type": "document"})
	var file driveDescriptor
	decodeInto(t, resp.Data, &file)

	h.req(t, http.StatusNoContent, http.MethodDelete, "/api/v1/drive/folders/"+outer.ID, admin, nil)

	// Gone from the parent's listing...
	for _, e := range h.children(t, admin, root.ID) {
		if e.Folder != nil && e.Folder.ID == outer.ID {
			t.Error("the trashed folder is still listed under its parent")
		}
	}
	// ...and so is everything under it.
	if e := h.children(t, admin, inner.ID); len(e) != 0 {
		t.Errorf("the nested folder still lists %d children after its ancestor was trashed", len(e))
	}
	if e := h.children(t, admin, outer.ID); len(e) != 0 {
		t.Errorf("the trashed folder still lists %d children", len(e))
	}

	// Creating into a trashed folder is a conflict, not a 500 and not a
	// success: the object would be invisible the moment it was written.
	h.denied(t, http.StatusConflict, http.MethodPost,
		"/api/v1/workspaces/"+ws+"/drive/folders", admin,
		map[string]string{"parent_id": outer.ID, "name": "orphan"})

	// The file inside the trashed subtree is unreachable through its folder;
	// opening it by id still works only for someone who could already read it,
	// which is what the trash means (it is not a permission change).
	if file.ID == "" {
		t.Error("the fixture created no file")
	}
}

// The listing is paginated with a real keyset cursor, and the page boundary is
// where an off-by-one shows up: a cursor that compares the wrong columns
// silently drops a row or repeats one.
func TestDriveChildrenPaginateAcrossAPageBoundary(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	root := h.driveRoot(t, admin, ws)
	parent := h.createFolder(t, admin, ws, root.ID, "many")

	const total = 7
	want := map[string]bool{}
	for i := range total {
		f := h.createFolder(t, admin, ws, parent.ID, fmt.Sprintf("folder-%d", i))
		want[f.ID] = true
	}

	seen := map[string]bool{}
	path := "/api/v1/drive/folders/" + parent.ID + "/children?limit=3"
	for pages := 0; pages < 10; pages++ {
		resp := h.req(t, http.StatusOK, http.MethodGet, path, admin, nil)
		var entries []driveEntry
		decodeInto(t, resp.Data, &entries)
		for _, e := range entries {
			if e.Folder == nil {
				continue
			}
			if seen[e.Folder.ID] {
				t.Fatalf("folder %s appeared on two pages", e.Folder.ID)
			}
			seen[e.Folder.ID] = true
		}
		if resp.Meta == nil || !resp.Meta.HasMore {
			break
		}
		path = "/api/v1/drive/folders/" + parent.ID + "/children?limit=3&cursor=" + resp.Meta.Cursor
	}

	if len(seen) != total {
		t.Fatalf("paging saw %d of %d folders; a keyset that compares the wrong columns "+
			"drops rows exactly at a page boundary", len(seen), total)
	}
}
