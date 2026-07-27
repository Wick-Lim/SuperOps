//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// A FILE ID IN A REQUEST BODY IS NOT PERMISSION TO USE THAT FILE.
//
// POST /workspaces/{workspace_id}/collab/documents takes the workspace from the
// path and the resource from the body, and authorized only the first. The
// second is what decides whose object gets attached to whose workspace, so a
// caller in any tenant could name a file in another one.
//
// It is a denial rather than a disclosure: reads still go through CanRead on
// the object, so the claimant sees nothing of the content. What they take is
// the (resource_type, resource_id) row itself — created in THEIR workspace —
// and the owner opening their own file afterwards gets 409 for as long as the
// row exists, with no route that releases it. resource_type is caller-supplied
// and validated only as a character class, so one file has as many claimable
// pairs as the claimant cares to spend requests on.
func TestAnotherTenantCannotClaimYourFilesCollabDocument(t *testing.T) {
	h := getHarness(t)
	victim := h.newTenant(t, "claim-victim")
	attacker := h.newTenant(t, "claim-attacker")

	file := h.newDocument(t, victim.token, victim.workspaceID,
		fmt.Sprintf("victim-%d", time.Now().UnixNano()))

	open := func(a *tenant, rtype string) (int, apiResp) {
		code, resp := h.do(t, http.MethodPost,
			"/api/v1/workspaces/"+a.workspaceID+"/collab/documents", a.token,
			map[string]string{"resource_type": rtype, "resource_id": file.ID})
		return code, resp
	}

	// "spreadsheet" rather than "document" on purpose: creating the Drive file
	// already took (document, file), so a test that used it would be refused by
	// the conflict rule and pass without the authorization check existing.
	if code, resp := open(attacker, "spreadsheet"); code != http.StatusForbidden {
		got, _ := json.Marshal(resp.Data)
		t.Fatalf("the attacker's claim = %d, want 403 (%s): a caller reached "+
			"another tenant's file with an id alone", code, got)
	}

	// The claim must not have landed: the owner opens the same pair and gets a
	// document, not the 409 that a taken row produces. This is the assertion
	// that would still fail if the check ran AFTER the insert.
	code, resp := open(victim, "spreadsheet")
	if code != http.StatusOK {
		t.Fatalf("the owner's own open = %d, want 200 (%+v): the refused claim "+
			"still created the row, so the owner is locked out of their file",
			code, resp.Error)
	}
	var doc struct {
		WorkspaceID string `json:"workspace_id"`
	}
	decodeInto(t, resp.Data, &doc)
	if doc.WorkspaceID != victim.workspaceID {
		t.Errorf("the document belongs to workspace %s, want the owner's %s",
			doc.WorkspaceID, victim.workspaceID)
	}

	// The control: the owner was never the one being refused.
	if code, _ := open(victim, "document"); code != http.StatusOK {
		t.Errorf("the owner opening their own document = %d, want 200: the new "+
			"check refuses the caller it was meant to allow", code)
	}
}

// A MEMBER OF THE SAME WORKSPACE IS STILL ALLOWED.
//
// The fix adds a second authorization to a route that had one, which is the
// shape that quietly locks out the people it was not aimed at. In this model a
// member holds workspace-level write, so they reach every file in the tenant;
// the object check has to agree with that rather than narrow it.
func TestAWorkspaceMemberCanStillOpenACollaborationDocument(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)

	owner := h.newTenant(t, "open-owner")
	file := h.newDocument(t, owner.token, owner.workspaceID,
		fmt.Sprintf("shared-%d", time.Now().UnixNano()))

	member := h.newUser(t, admin, ws, "open-member")
	h.joinWorkspace(t, owner.token, owner.workspaceID, member)

	code, resp := h.do(t, http.MethodPost,
		"/api/v1/workspaces/"+owner.workspaceID+"/collab/documents", member.token,
		map[string]string{"resource_type": "spreadsheet", "resource_id": file.ID})
	if code != http.StatusOK {
		t.Fatalf("a workspace member opening a document = %d, want 200 (%+v)",
			code, resp.Error)
	}
}
