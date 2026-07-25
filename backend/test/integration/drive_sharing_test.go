//go:build integration

package integration

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"
)

type shareLink struct {
	ID          string `json:"id"`
	Capability  string `json:"capability"`
	HasPassword bool   `json:"has_password"`
	UseCount    int    `json:"use_count"`
}

type linkCreated struct {
	Link  shareLink `json:"link"`
	Token string    `json:"token"`
}

// A grant reaches the whole subtree, and it cannot hand over more than the
// granter holds.
func TestDriveShareGrantsAndRefusesEscalation(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	root := h.driveRoot(t, admin, ws)
	folder := h.createFolder(t, admin, ws, root.ID, "shared-folder")

	guest := h.newUser(t, admin, ws, "share-guest")

	// A guest holds read on Drive through the workspace grant, capped by role.
	// Sharing raises it for this one object.
	h.req(t, http.StatusOK, http.MethodPut,
		"/api/v1/drive/folder/"+folder.ID+"/shares", admin,
		map[string]string{"subject_id": guest.id, "capability": "write"})

	// The panel shows it.
	resp := h.req(t, http.StatusOK, http.MethodGet,
		"/api/v1/drive/folder/"+folder.ID+"/shares", admin, nil)
	var shares []struct {
		SubjectID  string `json:"subject_id"`
		Capability string `json:"capability"`
	}
	decodeInto(t, resp.Data, &shares)
	found := false
	for _, s := range shares {
		if s.SubjectID == guest.id && s.Capability == "write" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the grant is not in the sharing panel: %+v", shares)
	}

	// The guest can now write into it.
	h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/workspaces/"+ws+"/drive/folders", guest.token,
		map[string]string{"parent_id": folder.ID, "name": "guest made this"})

	// But they cannot grant ABOVE what they hold. Nothing in authz compares the
	// requested capability to the granter's, so without this guard write is a
	// path to admin.
	other := h.newUser(t, admin, ws, "share-target")
	h.denied(t, http.StatusForbidden, http.MethodPut,
		"/api/v1/drive/folder/"+folder.ID+"/shares", guest.token,
		map[string]string{"subject_id": other.id, "capability": "admin"})

	// And a grant to somebody outside the workspace is refused: that is what
	// links are for.
	outsider := h.newTenant(t, "share-outsider")
	h.denied(t, http.StatusBadRequest, http.MethodPut,
		"/api/v1/drive/folder/"+folder.ID+"/shares", admin,
		map[string]string{"subject_id": outsider.id, "capability": "read"})

	// Revoking takes it away.
	h.req(t, http.StatusNoContent, http.MethodDelete,
		"/api/v1/drive/folder/"+folder.ID+"/shares/user/"+guest.id, admin, nil)
	h.denied(t, http.StatusForbidden, http.MethodPut,
		"/api/v1/drive/folder/"+folder.ID+"/shares", guest.token,
		map[string]string{"subject_id": other.id, "capability": "read"})
}

// A link is a bearer credential for content. The token is returned once, the
// hash is what is stored, and resolving yields EXACTLY one access key.
func TestDriveShareLinkResolvesToOneKeyOnly(t *testing.T) {
	h := getHarness(t)
	tenant := h.newTenant(t, "linkowner")
	root := h.driveRoot(t, tenant.token, tenant.workspaceID)
	folder := h.createFolder(t, tenant.token, tenant.workspaceID, root.ID, "public-ish")

	resp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/drive/folder/"+folder.ID+"/links", tenant.token,
		map[string]any{"capability": "read"})
	var created linkCreated
	decodeInto(t, resp.Data, &created)
	if created.Token == "" {
		t.Fatal("no token returned")
	}

	// The token is NOT stored. A table an operator can read is a table an
	// operator can read every share link out of.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stored []byte
	if err := h.app.DB.QueryRow(ctx,
		`SELECT token_hash FROM drive_share_links WHERE id = $1`, created.Link.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte(created.Token)) {
		t.Fatal("the plaintext token is in the database")
	}

	// It does NOT come back in the listing.
	resp = h.req(t, http.StatusOK, http.MethodGet,
		"/api/v1/drive/folder/"+folder.ID+"/links", tenant.token, nil)
	var links []map[string]any
	decodeInto(t, resp.Data, &links)
	if len(links) != 1 {
		t.Fatalf("links = %d, want 1", len(links))
	}
	if _, leaked := links[0]["token"]; leaked {
		t.Error("the listing returns the token; it is shown once and never again")
	}

	// Resolving needs no authentication and yields exactly one key.
	code, resolved := h.resolveLink(t, created.Token, "")
	if code != http.StatusOK {
		t.Fatalf("resolve = %d (%+v)", code, resolved.Error)
	}
	var session struct {
		ObjectID   string   `json:"object_id"`
		Capability string   `json:"capability"`
		AccessKeys []string `json:"access_keys"`
	}
	decodeInto(t, resolved.Data, &session)
	if session.ObjectID != folder.ID {
		t.Errorf("resolved to %s, want %s", session.ObjectID, folder.ID)
	}
	if len(session.AccessKeys) != 1 {
		t.Fatalf("a link session holds %d keys, want exactly 1: %v — anything more is a "+
			"link that widens beyond the object it was made for", len(session.AccessKeys),
			session.AccessKeys)
	}
	// NEVER the workspace key. acl_object.workspace_id stays authoritative for
	// tenancy, and a link carrying 'w-' would read the whole workspace.
	for _, k := range session.AccessKeys {
		if len(k) > 2 && k[:2] == "w-" {
			t.Fatalf("a link session holds the workspace key %q; it would read the whole tenant", k)
		}
	}

	// Revoking kills it, and the answer is the same one an unknown token gets.
	h.req(t, http.StatusNoContent, http.MethodDelete,
		"/api/v1/drive/links/"+created.Link.ID, tenant.token, nil)
	if code, _ := h.resolveLink(t, created.Token, ""); code != http.StatusNotFound {
		t.Errorf("a revoked link resolves with %d, want 404", code)
	}

	// The grant went with it. Leaving it would mean a "revoked" link still
	// materialized its key on every object in the subtree.
	var grants int
	if err := h.app.DB.QueryRow(ctx,
		`SELECT count(*) FROM acl_grant WHERE subject_type = 'link' AND subject_id = $1`,
		created.Link.ID).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if grants != 0 {
		t.Error("the revoked link's grant survived")
	}
}

// Every reason a link does not work gives the SAME answer. Distinguishing
// "expired" from "unknown" tells somebody walking the token space which guesses
// landed on a real row.
func TestDriveShareLinkFailuresAreIndistinguishable(t *testing.T) {
	h := getHarness(t)
	tenant := h.newTenant(t, "linkfail")
	root := h.driveRoot(t, tenant.token, tenant.workspaceID)

	past := time.Now().Add(-time.Hour)
	resp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/drive/folder/"+root.ID+"/links", tenant.token,
		map[string]any{"capability": "read", "expires_at": past})
	var expired linkCreated
	decodeInto(t, resp.Data, &expired)

	one := 1
	resp = h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/drive/folder/"+root.ID+"/links", tenant.token,
		map[string]any{"capability": "read", "max_uses": one})
	var limited linkCreated
	decodeInto(t, resp.Data, &limited)
	if code, _ := h.resolveLink(t, limited.Token, ""); code != http.StatusOK {
		t.Fatal("the first use of a single-use link was refused")
	}

	for _, tt := range []struct {
		name  string
		token string
	}{
		{"never existed", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		{"expired", expired.Token},
		{"used up", limited.Token},
	} {
		code, resp := h.resolveLink(t, tt.token, "")
		if code != http.StatusNotFound {
			t.Errorf("%s: resolve = %d, want 404", tt.name, code)
		}
		if resp.Error == nil || resp.Error.Code != "LINK_INVALID" {
			t.Errorf("%s: error = %+v, want a uniform LINK_INVALID", tt.name, resp.Error)
		}
	}
}

// A password is a second factor on a URL that may be forwarded further than
// intended, and a wrong one must not burn a use.
func TestDriveShareLinkPassword(t *testing.T) {
	h := getHarness(t)
	tenant := h.newTenant(t, "linkpw")
	root := h.driveRoot(t, tenant.token, tenant.workspaceID)

	resp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/drive/folder/"+root.ID+"/links", tenant.token,
		map[string]any{"capability": "read", "password": "correct horse", "max_uses": 2})
	var created linkCreated
	decodeInto(t, resp.Data, &created)
	if !created.Link.HasPassword {
		t.Error("the link does not report that it has a password")
	}

	if code, r := h.resolveLink(t, created.Token, ""); code != http.StatusUnauthorized {
		t.Errorf("resolving without the password = %d (%+v), want 401 — the holder of a valid "+
			"link has to be told to enter one", code, r.Error)
	}
	if code, _ := h.resolveLink(t, created.Token, "wrong"); code != http.StatusUnauthorized {
		t.Error("a wrong password did not produce 401")
	}
	if code, r := h.resolveLink(t, created.Token, "correct horse"); code != http.StatusOK {
		t.Fatalf("the correct password = %d (%+v)", code, r.Error)
	}

	// The two failures did not consume the two uses.
	resp = h.req(t, http.StatusOK, http.MethodGet,
		"/api/v1/drive/folder/"+root.ID+"/links", tenant.token, nil)
	var links []shareLink
	decodeInto(t, resp.Data, &links)
	if len(links) != 1 || links[0].UseCount != 1 {
		t.Errorf("use_count = %+v, want exactly 1 — a wrong password must not burn a use", links)
	}
}
