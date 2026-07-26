//go:build integration

// File authorization, end to end.
//
// The file download path had no integration coverage at all, which mattered
// because it is the one place where authorization is NOT workspace-shaped: a
// file is authorized against the channel of the message it hangs off, and only
// its uploader may read it while it is still unattached. Workspace-level
// authorization here is the bug that let any member fetch a file posted in a
// private channel given nothing but its uuid.
//
// docs/plans/00-permissions.md step 4 replaced the handler's three-step dance
// (resolve message, resolve channel, authorize channel, fall back to uploader)
// with a single authz.Can against the file object. These tests exist so that
// swap is checked against real HTTP rather than only against the checker's own
// unit fixtures.
package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"testing"
)

// upload posts a small file and returns its id. It returns "" when file storage
// is not wired (MinIO down or disabled), which unregisters the route entirely —
// asserting "the download was refused" against a 404 from a missing route would
// be a test that can never fail.
func (h *harness) upload(t *testing.T, token, workspaceID, name string) string {
	t.Helper()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("workspace_id", workspaceID); err != nil {
		t.Fatalf("multipart workspace_id: %v", err)
	}
	part, err := mw.CreateFormFile("file", name)
	if err != nil {
		t.Fatalf("multipart file part: %v", err)
	}
	if _, err := part.Write([]byte("integration fixture payload")); err != nil {
		t.Fatalf("multipart write: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("multipart close: %v", err)
	}

	req, err := http.NewRequest("POST", h.base+"/api/v1/files/upload", &body)
	if err != nil {
		t.Fatalf("new upload request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)

	res, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)

	if res.StatusCode == http.StatusNotFound {
		return "" // route absent: file storage disabled
	}
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d, want 201 (%s)", res.StatusCode, string(raw))
	}

	var env struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.Data.ID == "" {
		t.Fatalf("upload returned no file id: %s", string(raw))
	}
	return env.Data.ID
}

func (h *harness) requireFiles(t *testing.T, token, workspaceID string) string {
	t.Helper()
	id := h.upload(t, token, workspaceID, "fixture.txt")
	if id == "" {
		t.Skip("file storage disabled (MinIO unreachable or FILES_ENABLED off)")
	}
	return id
}

// TestFileAccessFollowsTheAttachedChannel is the whole of the file rule in one
// test: unattached means uploader-only, attached means "whoever may read that
// channel", and workspace membership is never enough on its own.
func TestFileAccessFollowsTheAttachedChannel(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	seed := h.firstWorkspace(t, admin)

	owner := h.newTenant(t, "filo")
	// A second member of the same workspace who is deliberately NOT in the
	// private channel: this is the position workspace-level authorization got
	// wrong.
	mate := h.newUser(t, admin, seed, "film")
	h.joinWorkspace(t, owner.token, owner.workspaceID, mate)
	// And an unrelated tenant.
	stranger := h.newTenant(t, "fils")

	fileID := h.requireFiles(t, owner.token, owner.workspaceID)
	download := "/api/v1/files/" + fileID

	// 1. Unattached: the uploader reads it, nobody else does — not a fellow
	//    workspace member, not another tenant.
	h.req(t, http.StatusOK, "GET", download, owner.token, nil)
	h.denied(t, http.StatusForbidden, "GET", download, mate.token, nil)
	h.denied(t, http.StatusForbidden, "GET", download, stranger.token, nil)

	// 2. Attach it to a message in a PRIVATE channel. Readability now follows
	//    the channel, so the uploader still reads it and the workspace member
	//    still does not.
	priv := h.createTypedChannel(t, owner.token, owner.workspaceID, uniqueSlug("filpriv"), "private")
	h.req(t, http.StatusCreated, "POST", "/api/v1/channels/"+priv+"/messages", owner.token,
		map[string]any{"content": "with attachment", "file_ids": []string{fileID}})

	h.req(t, http.StatusOK, "GET", download, owner.token, nil)
	h.denied(t, http.StatusForbidden, "GET", download, mate.token, nil)
	h.denied(t, http.StatusForbidden, "GET", download, stranger.token, nil)

	// 3. Admitted to the channel, the same member can now read the same file —
	//    which proves the refusals above were the channel and not a blanket deny.
	h.req(t, http.StatusCreated, "POST",
		"/api/v1/workspaces/"+owner.workspaceID+"/channels/"+priv+"/members", owner.token,
		map[string]string{"user_id": mate.id})
	h.req(t, http.StatusOK, "GET", download, mate.token, nil)
	h.denied(t, http.StatusForbidden, "GET", download, stranger.token, nil)

	// 4. Removed again, access goes with the membership.
	h.req(t, http.StatusOK, "DELETE",
		"/api/v1/workspaces/"+owner.workspaceID+"/channels/"+priv+"/members/"+mate.id, owner.token, nil)
	h.denied(t, http.StatusForbidden, "GET", download, mate.token, nil)
}

// TestFileInPublicChannelIsWorkspaceReadable is the other half: a file attached
// in a PUBLIC channel is readable by any member of that workspace — because the
// channel is — and still not by anybody outside it.
func TestFileInPublicChannelIsWorkspaceReadable(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	seed := h.firstWorkspace(t, admin)

	owner := h.newTenant(t, "filpo")
	mate := h.newUser(t, admin, seed, "filpm")
	h.joinWorkspace(t, owner.token, owner.workspaceID, mate)
	stranger := h.newTenant(t, "filps")

	fileID := h.requireFiles(t, owner.token, owner.workspaceID)
	download := "/api/v1/files/" + fileID

	pub := h.createChannel(t, owner.token, owner.workspaceID, uniqueSlug("filpub"))
	h.req(t, http.StatusCreated, "POST", "/api/v1/channels/"+pub+"/messages", owner.token,
		map[string]any{"content": "public attachment", "file_ids": []string{fileID}})

	h.req(t, http.StatusOK, "GET", download, owner.token, nil)
	h.req(t, http.StatusOK, "GET", download, mate.token, nil)
	h.denied(t, http.StatusForbidden, "GET", download, stranger.token, nil)

	// Deleting is not reading: a workspace member who is not the uploader and
	// not a workspace admin may read this file and still may not remove it.
	h.denied(t, http.StatusForbidden, "DELETE", download, mate.token, nil)
	h.req(t, http.StatusOK, "DELETE", download, owner.token, nil)
	h.denied(t, http.StatusNotFound, "GET", download, owner.token, nil)
}
