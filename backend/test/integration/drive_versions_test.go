//go:build integration

package integration

import (
	"bytes"
	"net/http"
	"testing"
)

type driveVersion struct {
	Version   int    `json:"version"`
	SizeBytes int64  `json:"size_bytes"`
	IsCurrent bool   `json:"is_current"`
	CreatedBy string `json:"created_by"`
}

// replaceContent posts a new version.
func (h *harness) replaceContent(t *testing.T, token, fileID, name string, body []byte) (int, apiResp) {
	t.Helper()
	return h.multipart(t, token, http.MethodPost,
		"/api/v1/drive/files/"+fileID+"/content", nil, name, body)
}

func (h *harness) versions(t *testing.T, token, fileID string) []driveVersion {
	t.Helper()
	resp := h.req(t, http.StatusOK, http.MethodGet,
		"/api/v1/drive/files/"+fileID+"/versions", token, nil)
	var out []driveVersion
	decodeInto(t, resp.Data, &out)
	return out
}

func TestDriveVersionsAccumulateAndRestore(t *testing.T) {
	h := getHarness(t)
	tenant := h.newTenant(t, "versions")
	root := h.driveRoot(t, tenant.token, tenant.workspaceID)

	v1 := bytes.Repeat([]byte("1"), 1000)
	code, resp := h.uploadToDrive(t, tenant.token, tenant.workspaceID, root.ID, "doc.txt", v1)
	if code != http.StatusCreated {
		t.Fatalf("upload = %d (%+v)", code, resp.Error)
	}
	var file driveDescriptor
	decodeInto(t, resp.Data, &file)

	v2 := bytes.Repeat([]byte("2"), 2000)
	if code, resp := h.replaceContent(t, tenant.token, file.ID, "doc.txt", v2); code != http.StatusCreated {
		t.Fatalf("new version = %d (%+v)", code, resp.Error)
	}

	versions := h.versions(t, tenant.token, file.ID)
	if len(versions) != 2 {
		t.Fatalf("history = %d entries, want 2: %+v", len(versions), versions)
	}
	if versions[0].Version != 2 || !versions[0].IsCurrent {
		t.Errorf("newest entry = v%d current=%v, want v2 current", versions[0].Version, versions[0].IsCurrent)
	}
	if versions[1].Version != 1 || versions[1].IsCurrent {
		t.Errorf("oldest entry = v%d current=%v, want v1 not current", versions[1].Version, versions[1].IsCurrent)
	}

	// BOTH versions are charged. Every version is an object in the bucket, and a
	// history that was free would be a quota anybody could bypass by uploading
	// the same file repeatedly.
	usage := h.usage(t, tenant.token, tenant.workspaceID)
	if got := int64(usage["blob"].(map[string]any)["bytes"].(float64)); got != 3000 {
		t.Errorf("bytes_used = %d after two versions, want 3000 (1000 + 2000)", got)
	}
	breakdown := usage["breakdown"].(map[string]any)
	if got := int64(breakdown["version_bytes"].(float64)); got != 1000 {
		t.Errorf("version_bytes = %d, want the 1000 held by the superseded version", got)
	}
	if got := int64(breakdown["drift_bytes"].(float64)); got != 0 {
		t.Errorf("drift_bytes = %d; the incremental arithmetic disagrees with the invariant", got)
	}

	// The FILE ID IS STABLE. That is what lets a message attachment, a
	// collaborative document and a search document keep pointing at the same
	// thing across a revision.
	reopened := h.req(t, http.StatusOK, http.MethodGet, "/api/v1/drive/files/"+file.ID, tenant.token, nil)
	var after driveDescriptor
	decodeInto(t, reopened.Data, &after)
	if after.ID != file.ID {
		t.Fatalf("the file id changed across a version: %s -> %s", file.ID, after.ID)
	}

	// Restoring moves the head and stores nothing, so the quota does not move.
	// A restore that copied the row would let a workspace fill up by pressing
	// undo.
	h.req(t, http.StatusOK, http.MethodPost,
		"/api/v1/drive/files/"+file.ID+"/versions/1/restore", tenant.token, nil)

	restored := h.versions(t, tenant.token, file.ID)
	if len(restored) != 2 {
		t.Errorf("restore added a history entry (%d); it must move the head, not store bytes",
			len(restored))
	}
	for _, v := range restored {
		if (v.Version == 1) != v.IsCurrent {
			t.Errorf("after restoring v1, v%d current = %v", v.Version, v.IsCurrent)
		}
	}
	usage = h.usage(t, tenant.token, tenant.workspaceID)
	if got := int64(usage["blob"].(map[string]any)["bytes"].(float64)); got != 3000 {
		t.Errorf("bytes_used = %d after a restore, want an unchanged 3000", got)
	}

	// A subsequent upload takes MAX(version)+1, so it cannot collide with a
	// version the head was rolled back past.
	if code, resp := h.replaceContent(t, tenant.token, file.ID, "doc.txt",
		bytes.Repeat([]byte("3"), 500)); code != http.StatusCreated {
		t.Fatalf("upload after a restore = %d (%+v)", code, resp.Error)
	}
	final := h.versions(t, tenant.token, file.ID)
	if len(final) != 3 || final[0].Version != 3 {
		t.Errorf("after restore-then-upload the history is %+v, want a v3 at the head", final)
	}
}

// Every historical version stays downloadable. If it did not, "version history"
// would be a list of things you cannot have.
func TestDriveHistoricalVersionIsDownloadable(t *testing.T) {
	h := getHarness(t)
	tenant := h.newTenant(t, "vdownload")
	root := h.driveRoot(t, tenant.token, tenant.workspaceID)

	code, resp := h.uploadToDrive(t, tenant.token, tenant.workspaceID, root.ID, "a.txt",
		bytes.Repeat([]byte("old"), 100))
	if code != http.StatusCreated {
		t.Fatalf("upload = %d (%+v)", code, resp.Error)
	}
	var file driveDescriptor
	decodeInto(t, resp.Data, &file)

	if code, _ := h.replaceContent(t, tenant.token, file.ID, "a.txt",
		bytes.Repeat([]byte("new"), 100)); code != http.StatusCreated {
		t.Fatal("new version rejected")
	}

	// A 302 to a presigned URL. Following it is what proves the old object was
	// not overwritten by the new one — a version list over a single key would
	// pass every other assertion in this file.
	body, ct := h.followRedirect(t, tenant.token,
		"/api/v1/drive/files/"+file.ID+"/versions/1/content")
	if !bytes.Equal(body, bytes.Repeat([]byte("old"), 100)) {
		t.Errorf("version 1 served %d bytes of the wrong content; a new version must write a "+
			"NEW object, because the bucket has no undo", len(body))
	}
	if ct != "application/octet-stream" {
		t.Errorf("historical version served as %q; originals are always attachments with an "+
			"opaque type, because a presigned URL carries no CSP", ct)
	}
}

// A collab-backed type refuses new bytes in every route that would accept them.
func TestDriveCollabTypeRefusesNewBytes(t *testing.T) {
	h := getHarness(t)
	tenant := h.newTenant(t, "collabver")
	root := h.driveRoot(t, tenant.token, tenant.workspaceID)

	resp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/workspaces/"+tenant.workspaceID+"/drive/files", tenant.token,
		map[string]string{"folder_id": root.ID, "name": "notes", "file_type": "document"})
	var doc driveDescriptor
	decodeInto(t, resp.Data, &doc)

	code, _ := h.replaceContent(t, tenant.token, doc.ID, "notes", []byte("plain text"))
	if code != http.StatusConflict {
		t.Errorf("POST /content on a collab type = %d, want 409; bytes put into a CRDT-backed "+
			"object are discarded by the next merge, and accepting them tells the client it saved",
			code)
	}
	h.denied(t, http.StatusConflict, http.MethodPost,
		"/api/v1/drive/files/"+doc.ID+"/versions/1/restore", tenant.token, nil)

	// It can still LIST — an empty history rather than an error, because a
	// collab document genuinely has none.
	if got := h.versions(t, tenant.token, doc.ID); len(got) != 0 {
		t.Errorf("a collab document reports %d versions, want none", len(got))
	}
}
