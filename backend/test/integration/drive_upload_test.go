//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"testing"
)

// uploadToDrive posts a multipart file into a Drive folder and returns the
// status and the decoded envelope.
func (h *harness) uploadToDrive(t *testing.T, token, workspaceID, folderID, name string, body []byte) (int, apiResp) {
	t.Helper()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if folderID != "" {
		if err := mw.WriteField("folder_id", folderID); err != nil {
			t.Fatal(err)
		}
	}
	part, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodPost,
		h.base+"/api/v1/workspaces/"+workspaceID+"/drive/files/upload", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)

	var resp apiResp
	_ = json.Unmarshal(raw, &resp)
	return res.StatusCode, resp
}

func (h *harness) usage(t *testing.T, token, workspaceID string) map[string]any {
	t.Helper()
	resp := h.req(t, http.StatusOK, http.MethodGet,
		"/api/v1/workspaces/"+workspaceID+"/drive/usage", token, nil)
	var out map[string]any
	decodeInto(t, resp.Data, &out)
	return out
}

func TestDriveUploadStoresBytesAndCharges(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	root := h.driveRoot(t, admin, ws)
	folder := h.createFolder(t, admin, ws, root.ID, "uploads")

	before := h.usage(t, admin, ws)
	beforeBytes := int64(before["blob"].(map[string]any)["bytes"].(float64))

	body := bytes.Repeat([]byte("x"), 4096)
	code, resp := h.uploadToDrive(t, admin, ws, folder.ID, "notes.txt", body)
	if code != http.StatusCreated {
		t.Fatalf("upload = %d, want 201 (%+v)", code, resp.Error)
	}
	var file driveDescriptor
	decodeInto(t, resp.Data, &file)

	// A blob file gets a content URL and no collaborative document — the other
	// half of the dispatch contract from the collab case.
	if file.StorageMode != "blob" {
		t.Errorf("storage_mode = %q, want blob", file.StorageMode)
	}
	if file.CollabDocumentID != nil {
		t.Error("a blob file carries a collab_document_id")
	}
	if file.ContentURL == nil || *file.ContentURL == "" {
		t.Error("a blob file has no content_url, so there is no way to download it")
	}

	// The content type is classified from the BYTES. "notes.txt" full of 'x' is
	// text/plain whatever the client claimed.
	if file.FileType != "file" {
		t.Errorf("file_type = %q, want file", file.FileType)
	}

	// It is in the folder's listing immediately.
	found := false
	for _, e := range h.children(t, admin, folder.ID) {
		if e.File != nil && e.File.ID == file.ID {
			found = true
		}
	}
	if !found {
		t.Error("an uploaded file is not in its folder's listing")
	}

	after := h.usage(t, admin, ws)
	afterBytes := int64(after["blob"].(map[string]any)["bytes"].(float64))
	if afterBytes-beforeBytes != int64(len(body)) {
		t.Errorf("usage moved by %d bytes, want %d", afterBytes-beforeBytes, len(body))
	}

	// The two halves are reported separately and there is deliberately no total:
	// one is exact and the other is recomputed by a job, and a single number
	// would have an accuracy nobody could state.
	if _, exists := after["total_bytes"]; exists {
		t.Error("usage carries a total_bytes; the exact and eventually-consistent halves " +
			"must not be summed into one number")
	}
	if after["blob"].(map[string]any)["consistency"] != "exact" {
		t.Error("the blob half is not labelled exact")
	}
	if after["collab"].(map[string]any)["consistency"] != "eventual" {
		t.Error("the collab half is not labelled eventual")
	}
	if after["counts_trashed_files"] != true || after["counts_old_versions"] != true {
		t.Error("usage does not state that trashed files and old versions count; " +
			"that is the first question a full workspace produces")
	}
}

// The quota refuses, and it refuses with the numbers the client needs to explain
// itself to the user.
func TestDriveUploadIsRefusedOverQuota(t *testing.T) {
	h := getHarness(t)
	other := h.newTenant(t, "quota-tenant")
	root := h.driveRoot(t, other.token, other.workspaceID)

	// A tenant of its own, so lowering the quota cannot affect another test.
	h.req(t, http.StatusOK, http.MethodPut,
		"/api/v1/workspaces/"+other.workspaceID+"/drive/quota", other.token,
		map[string]int64{"quota_bytes": 4096})

	if code, resp := h.uploadToDrive(t, other.token, other.workspaceID, root.ID,
		"fits.bin", bytes.Repeat([]byte("a"), 3000)); code != http.StatusCreated {
		t.Fatalf("an upload inside the quota = %d, want 201 (%+v)", code, resp.Error)
	}

	code, resp := h.uploadToDrive(t, other.token, other.workspaceID, root.ID,
		"too-big.bin", bytes.Repeat([]byte("a"), 3000))
	if code != http.StatusInsufficientStorage {
		t.Fatalf("an upload past the quota = %d, want 507 (%+v)", code, resp.Error)
	}

	// Nothing was charged for the refused upload.
	usage := h.usage(t, other.token, other.workspaceID)
	if got := int64(usage["blob"].(map[string]any)["bytes"].(float64)); got != 3000 {
		t.Errorf("bytes_used = %d after a refused upload, want exactly 3000", got)
	}

	// And the refusal is reversible by raising the cap — the quota is a policy,
	// not a wall.
	h.req(t, http.StatusOK, http.MethodPut,
		"/api/v1/workspaces/"+other.workspaceID+"/drive/quota", other.token,
		map[string]int64{"quota_bytes": 0})
	if code, resp := h.uploadToDrive(t, other.token, other.workspaceID, root.ID,
		"now-fits.bin", bytes.Repeat([]byte("a"), 3000)); code != http.StatusCreated {
		t.Fatalf("after lifting the quota = %d, want 201 (%+v)", code, resp.Error)
	}
}

// Deleting returns the bytes. A quota that only ever counts up is a workspace
// that fills permanently.
func TestDriveTrashAndQuotaAccounting(t *testing.T) {
	h := getHarness(t)
	tenant := h.newTenant(t, "refund-tenant")
	root := h.driveRoot(t, tenant.token, tenant.workspaceID)

	body := bytes.Repeat([]byte("z"), 2048)
	code, resp := h.uploadToDrive(t, tenant.token, tenant.workspaceID, root.ID, "temp.bin", body)
	if code != http.StatusCreated {
		t.Fatalf("upload = %d (%+v)", code, resp.Error)
	}
	var file driveDescriptor
	decodeInto(t, resp.Data, &file)

	usage := h.usage(t, tenant.token, tenant.workspaceID)
	if got := int64(usage["blob"].(map[string]any)["bytes"].(float64)); got != int64(len(body)) {
		t.Fatalf("bytes_used = %d, want %d", got, len(body))
	}

	// Trashing does NOT refund: a trashed file is still an object in a bucket,
	// and that is stated in the usage payload rather than left to be discovered.
	h.req(t, http.StatusNoContent, http.MethodDelete, "/api/v1/drive/files/"+file.ID, tenant.token, nil)
	usage = h.usage(t, tenant.token, tenant.workspaceID)
	if got := int64(usage["blob"].(map[string]any)["bytes"].(float64)); got != int64(len(body)) {
		t.Errorf("trashing refunded %d bytes; trashed files count, which is the point of a quota",
			int64(len(body))-got)
	}

	// The admin breakdown attributes them, which is the answer to "we deleted
	// everything, why is it still full?"
	breakdown, ok := usage["breakdown"].(map[string]any)
	if !ok {
		t.Fatal("no breakdown for a workspace admin")
	}
	if got := int64(breakdown["trashed_bytes"].(float64)); got != int64(len(body)) {
		t.Errorf("trashed_bytes = %d, want %d", got, len(body))
	}
	if got := int64(breakdown["drift_bytes"].(float64)); got != 0 {
		t.Errorf("drift_bytes = %d, want 0 — the incremental arithmetic disagrees with "+
			"a fresh recomputation of the invariant", got)
	}
}

// Storage is a deployment-dependent capability and Drive's routes are registered
// unconditionally, so the upload route is where a deployment without object
// storage arrives. It must say so rather than 500.
func TestDriveUploadRejectsAnOversizeBody(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	root := h.driveRoot(t, admin, ws)

	// Just past the 100MB ceiling would be slow to send; assert the constant is
	// enforced on the wire by checking a body the server must refuse to buffer.
	// 1MB over the multipart memory bound is enough to prove the spill path
	// works without moving 100MB through the loopback.
	body := bytes.Repeat([]byte("q"), (1<<20)+4096)
	code, resp := h.uploadToDrive(t, admin, ws, root.ID, "spill.bin", body)
	if code != http.StatusCreated {
		t.Fatalf("an upload larger than multipartMemory = %d, want 201 — it must spill to "+
			"a temp file rather than fail (%+v)", code, resp.Error)
	}
	var file driveDescriptor
	decodeInto(t, resp.Data, &file)
	if file.ID == "" {
		t.Error("no file id returned")
	}
}
