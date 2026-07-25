//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// Revoking a share must end the editing session NOW, not in five minutes.
//
// authz's live-revocation fan-out covers channels and collaborative documents
// and deliberately not files: liveTypes is {channel, doc}, and object.go is
// honest about why — "there is no such thing as an open file session; the next
// download re-authorizes". That was true until a file could be an open editor.
//
// So `DELETE /drive/file/{id}/shares/user/{id}` withdrew the grant and dropped
// no session. The backstop was ws.membershipRecheckPeriod, five minutes, on the
// pillar whose entire premise is live collaboration. It was latency rather than
// a leak — the recheck does authorize against the file — but five minutes of
// continued typing after somebody is removed is the wrong answer.
//
// This test is deliberately written against an IMMEDIATE collab.left with
// reason=revoked and a short deadline. A version that waited on wall-clock
// would take five minutes and would pass against the bug.
func TestRevokingAShareEndsTheEditorSessionImmediately(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)

	owner := h.newTenant(t, "revoke-owner")
	doc := h.newDocument(t, owner.token, owner.workspaceID,
		fmt.Sprintf("live-%d", time.Now().UnixNano()))
	if doc.CollabDocumentID == nil {
		t.Fatal("the document has no room to join")
	}

	editor := h.newUser(t, admin, ws, "revoke-editor")
	h.joinWorkspace(t, owner.token, owner.workspaceID, editor)
	h.req(t, http.StatusOK, http.MethodPut,
		"/api/v1/drive/file/"+doc.ID+"/shares", owner.token,
		map[string]string{"subject_id": editor.id, "capability": "write"})

	conn := h.dialWS(t, editor.token)
	conn.send(map[string]any{
		"type": "collab.join",
		"data": map[string]string{"document_id": *doc.CollabDocumentID},
	})
	joined := conn.waitFor(10*time.Second, "collab.joined")
	var joinData struct {
		DocumentID string `json:"document_id"`
		CanWrite   bool   `json:"can_write"`
	}
	if err := json.Unmarshal(joined.data, &joinData); err != nil {
		t.Fatal(err)
	}
	if !joinData.CanWrite {
		t.Fatal("the shared editor joined without write; the fixture is not testing revocation")
	}

	// Withdraw it.
	h.req(t, http.StatusNoContent, http.MethodDelete,
		"/api/v1/drive/file/"+doc.ID+"/shares/user/"+editor.id, owner.token, nil)

	// Five seconds, not five minutes. The membership recheck cannot produce
	// this frame inside the window, so a pass here is the targeted revocation
	// and nothing else.
	left := conn.waitFor(5*time.Second, "collab.left")
	var leftData struct {
		DocumentID string `json:"document_id"`
		Reason     string `json:"reason"`
	}
	if err := json.Unmarshal(left.data, &leftData); err != nil {
		t.Fatal(err)
	}
	if leftData.DocumentID != *doc.CollabDocumentID {
		t.Fatalf("left the wrong room: %s", leftData.DocumentID)
	}
	if leftData.Reason != "revoked" {
		t.Errorf("reason = %q, want revoked — the client renders a different message for "+
			"'you were removed' than for 'you closed the tab'", leftData.Reason)
	}
}

// Revoking a share on a FOLDER cuts the sessions inside it too.
//
// This is why the revocation takes an acl_object PATH rather than an id.
// Sharing is inherited down the tree, so withdrawing it at the folder withdraws
// it for every document beneath — and a fan-out keyed on the folder's own id
// would drop nothing at all, which is the failure that looks like it works.
func TestRevokingAFolderShareEndsSessionsBeneathIt(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)

	owner := h.newTenant(t, "revoke-folder-owner")
	root := h.driveRoot(t, owner.token, owner.workspaceID)
	folder := h.createFolder(t, owner.token, owner.workspaceID, root.ID,
		fmt.Sprintf("team-%d", time.Now().UnixNano()))

	resp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/workspaces/"+owner.workspaceID+"/drive/files", owner.token,
		map[string]string{"folder_id": folder.ID, "name": "spec.doc", "file_type": "document"})
	var doc driveDescriptor
	decodeInto(t, resp.Data, &doc)

	editor := h.newUser(t, admin, ws, "revoke-folder-editor")
	h.joinWorkspace(t, owner.token, owner.workspaceID, editor)
	h.req(t, http.StatusOK, http.MethodPut,
		"/api/v1/drive/folder/"+folder.ID+"/shares", owner.token,
		map[string]string{"subject_id": editor.id, "capability": "write"})

	conn := h.dialWS(t, editor.token)
	conn.send(map[string]any{
		"type": "collab.join",
		"data": map[string]string{"document_id": *doc.CollabDocumentID},
	})
	conn.waitFor(10*time.Second, "collab.joined")

	h.req(t, http.StatusNoContent, http.MethodDelete,
		"/api/v1/drive/folder/"+folder.ID+"/shares/user/"+editor.id, owner.token, nil)

	left := conn.waitFor(5*time.Second, "collab.left")
	var leftData struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(left.data, &leftData); err != nil {
		t.Fatal(err)
	}
	if leftData.Reason != "revoked" {
		t.Errorf("reason = %q, want revoked", leftData.Reason)
	}
}

// The revocation is TARGETED: revoking one person must not disconnect everybody
// else in the room. A fan-out that dropped the whole room would look correct in
// the test above and would kick the document's owner out of their own document.
func TestRevokingOnePersonLeavesTheOthersEditing(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)

	owner := h.newTenant(t, "revoke-bystander-owner")
	doc := h.newDocument(t, owner.token, owner.workspaceID,
		fmt.Sprintf("shared-%d", time.Now().UnixNano()))

	leaving := h.newUser(t, admin, ws, "revoke-leaving")
	staying := h.newUser(t, admin, ws, "revoke-staying")
	for _, a := range []*actor{leaving, staying} {
		h.joinWorkspace(t, owner.token, owner.workspaceID, a)
		h.req(t, http.StatusOK, http.MethodPut,
			"/api/v1/drive/file/"+doc.ID+"/shares", owner.token,
			map[string]string{"subject_id": a.id, "capability": "write"})
	}

	connLeaving := h.dialWS(t, leaving.token)
	connStaying := h.dialWS(t, staying.token)
	for _, c := range []*wsClient{connLeaving, connStaying} {
		c.send(map[string]any{
			"type": "collab.join",
			"data": map[string]string{"document_id": *doc.CollabDocumentID},
		})
		c.waitFor(10*time.Second, "collab.joined")
	}

	h.req(t, http.StatusNoContent, http.MethodDelete,
		"/api/v1/drive/file/"+doc.ID+"/shares/user/"+leaving.id, owner.token, nil)

	connLeaving.waitFor(5*time.Second, "collab.left")
	// And the bystander is still in. Two seconds is enough: the revocation
	// above already arrived, so a broadcast one would have arrived with it.
	connStaying.expectNone(2*time.Second, "collab.left")
}
