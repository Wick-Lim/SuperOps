//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

type resolvedRef struct {
	RefType string  `json:"ref_type"`
	RefID   string  `json:"ref_id"`
	Access  string  `json:"access"`
	Title   *string `json:"title,omitempty"`
}

// renameFileRow renames a file directly, for a fixture that needs a
// distinctive name on an object the API has no rename route for (a chat
// attachment is not a Drive file).
func (h *harness) renameFileRow(t *testing.T, fileID, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := h.app.DB.Exec(ctx, `UPDATE files SET name = $2 WHERE id = $1`, fileID, name); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) resolveRefs(t *testing.T, token, fileID string, refs []map[string]any) []resolvedRef {
	t.Helper()
	resp := h.req(t, http.StatusOK, http.MethodPost,
		"/api/v1/drive/files/"+fileID+"/refs/resolve", token,
		map[string]any{"refs": refs})
	var out []resolvedRef
	decodeInto(t, resp.Data, &out)
	return out
}

// THE LEAK TEST. Run it on every commit.
//
// A document embeds a file. The document is shared with somebody who cannot
// read that file. The body is an opaque CRDT blob the server cannot filter, so
// the ONLY defence is that the body never contains anything worth leaking: an
// embed node carries {ref_type, ref_id} and nothing else, and the preview is
// resolved per-caller here.
//
// The assertion is deliberately stronger than "access == denied". It checks the
// RAW JSON for the title, because a `title: ""` field would still be a leak of
// shape — it would confirm the object exists and has a name the caller is not
// being shown — and a struct comparison would not notice the difference.
func TestEmbedResolveDeniesUnreadableTargetWithNoTitleInTheJSON(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)

	// THE SECRET IS A PRIVATE-CHANNEL ATTACHMENT, not a file in a "private"
	// folder — there is no such thing. Every Drive object under the root
	// inherits the workspace grant, so a subfolder is readable by every member
	// and guest; restricted folders are a named cut (README ruling 5) because
	// inheritance is computed twice and stopping it in one place and not the
	// other is a leak. A file attached to a private channel derives its path
	// from the channel instead, which is the one in-workspace object a guest
	// genuinely cannot read.
	//
	// It is also the realistic case: somebody embeds a file that was shared in
	// a channel not everyone is in.
	owner := h.newTenant(t, "embed-owner")
	secretID := h.requireFiles(t, owner.token, owner.workspaceID)
	// A distinctive name, set on the row: the upload fixture names every file
	// "fixture.txt", and a leak assertion against a string that appears in a
	// dozen other rows would be worthless.
	secretName := fmt.Sprintf("acquisition-terms-%d.txt", time.Now().UnixNano())
	h.renameFileRow(t, secretID, secretName)
	priv := h.createTypedChannel(t, owner.token, owner.workspaceID, uniqueSlug("embedpriv"), "private")
	h.req(t, http.StatusCreated, http.MethodPost, "/api/v1/channels/"+priv+"/messages", owner.token,
		map[string]any{"content": "the terms", "file_ids": []string{secretID}})

	// A member of the same workspace who is NOT in that channel.
	guest := h.newUser(t, admin, ws, "embed-guest")
	h.joinWorkspace(t, owner.token, owner.workspaceID, guest)

	// The document that embeds it lives in the owner's Drive, shared with the
	// guest, who can therefore read the DOCUMENT and not its embed.
	doc := h.newDocument(t, owner.token, owner.workspaceID, fmt.Sprintf("memo-%d", time.Now().UnixNano()))
	h.req(t, http.StatusOK, http.MethodPut,
		"/api/v1/drive/file/"+doc.ID+"/shares", owner.token,
		map[string]string{"subject_id": guest.id, "capability": "read"})

	refs := []map[string]any{{"ref_type": "file", "ref_id": secretID}}

	// The owner sees the name.
	admin = owner.token
	mine := h.resolveRefs(t, admin, doc.ID, refs)
	if len(mine) != 1 || mine[0].Access != "granted" || mine[0].Title == nil {
		t.Fatalf("the owner cannot resolve their own embed: %+v", mine)
	}
	if *mine[0].Title != secretName {
		t.Fatalf("title = %q, want %q", *mine[0].Title, secretName)
	}

	// The guest does not — and the response contains no `title` key at all.
	raw := h.rawJSON(t, guest.token, http.MethodPost,
		"/api/v1/drive/files/"+doc.ID+"/refs/resolve",
		map[string]any{"refs": refs})
	if !bytes.Contains(raw, []byte(`"denied"`)) {
		t.Fatalf("the guest was not denied: %s", raw)
	}
	if bytes.Contains(raw, []byte(`"title"`)) {
		t.Fatalf("the denial carries a title field: %s", raw)
	}
	if bytes.Contains(raw, []byte(secretName)) {
		t.Fatalf("THE FILE NAME LEAKED THROUGH A DENIED EMBED: %s", raw)
	}

	// Admitted to the channel, the same caller resolves the same ref — which
	// proves the refusal was the capability and not the endpoint being broken
	// for anyone but the owner.
	h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/workspaces/"+owner.workspaceID+"/channels/"+priv+"/members", owner.token,
		map[string]string{"user_id": guest.id})
	after := h.resolveRefs(t, guest.token, doc.ID, refs)
	if len(after) != 1 || after[0].Access != "granted" || after[0].Title == nil {
		t.Fatalf("after joining the channel, the caller still cannot resolve the embed: %+v", after)
	}
}

// A `user` ref is an @mention and is NOT an acl_object. Feeding it to the
// capability checker returns ErrNotRegistered, so a resolver without a
// per-type dispatch answers 500 for the single most common ref in a document.
func TestUserRefResolvesByWorkspaceMembership(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)

	doc := h.newDocument(t, admin, ws, fmt.Sprintf("mentions-%d", time.Now().UnixNano()))
	colleague := h.newUser(t, admin, ws, "mentioned")

	got := h.resolveRefs(t, admin, doc.ID, []map[string]any{
		{"ref_type": "user", "ref_id": colleague.id},
	})
	if len(got) != 1 {
		t.Fatalf("got %d answers, want 1", len(got))
	}
	if got[0].Access != "granted" || got[0].Title == nil || *got[0].Title == "" {
		t.Fatalf("a mention of a colleague did not resolve: %+v", got[0])
	}

	// Somebody in another tenant does not resolve — a mention must not be a
	// directory of every user on the deployment.
	outsider := h.newTenant(t, "mention-outsider")
	raw := h.rawJSON(t, admin, http.MethodPost,
		"/api/v1/drive/files/"+doc.ID+"/refs/resolve",
		map[string]any{"refs": []map[string]any{
			{"ref_type": "user", "ref_id": outsider.id},
		}})
	if bytes.Contains(raw, []byte(`"title"`)) {
		t.Fatalf("a cross-tenant mention resolved to a name: %s", raw)
	}
}

// An unknown ref type is a placeholder, not a 500. A client is free to put a
// node in a document that this deployment has never heard of.
func TestUnknownRefTypeIsDeniedNotAnError(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	doc := h.newDocument(t, admin, ws, fmt.Sprintf("unknown-%d", time.Now().UnixNano()))

	got := h.resolveRefs(t, admin, doc.ID, []map[string]any{
		{"ref_type": "hologram", "ref_id": "11111111-1111-1111-1111-111111111111"},
	})
	if len(got) != 1 || got[0].Access != "denied" || got[0].Title != nil {
		t.Fatalf("an unknown ref type did not degrade to a placeholder: %+v", got)
	}
}

// Refs are rebuilt WHOLESALE inside the projection transaction, so a backlink
// appears when an embed is added and disappears when it is removed. A ref set
// that only ever grew would make "where is this file used" permanently wrong in
// the direction that matters — claiming a document references something it does
// not.
func TestBacklinksAppearAndDisappear(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	root := h.driveRoot(t, admin, ws)

	resp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/workspaces/"+ws+"/drive/files", admin,
		map[string]string{"folder_id": root.ID, "name": "target.doc", "file_type": "document"})
	var target driveDescriptor
	decodeInto(t, resp.Data, &target)

	doc := h.newDocument(t, admin, ws, fmt.Sprintf("citing-%d", time.Now().UnixNano()))
	h.appendUpdate(t, *doc.CollabDocumentID, me, []byte{1})

	// Project WITH the embed.
	if code, r := h.project(t, admin, doc.ID, map[string]any{
		"seq": 1, "schema_version": 1, "body_text": "see the other doc",
		"refs": []map[string]any{{"ref_type": "file", "ref_id": target.ID, "block_id": "b1"}},
	}); code != http.StatusOK {
		t.Fatalf("project = %d (%+v)", code, r.Error)
	}

	if !backlinkContains(h.backlinks(t, admin, "file", target.ID), doc.ID) {
		t.Fatal("the embedding document does not appear in its target's backlinks")
	}

	// Re-project WITHOUT it. The seq must advance or rule 1 refuses the write.
	h.appendUpdate(t, *doc.CollabDocumentID, me, []byte{2})
	if code, r := h.project(t, admin, doc.ID, map[string]any{
		"seq": 2, "schema_version": 1, "body_text": "the embed is gone",
	}); code != http.StatusOK {
		t.Fatalf("re-project = %d (%+v)", code, r.Error)
	}

	if backlinkContains(h.backlinks(t, admin, "file", target.ID), doc.ID) {
		t.Fatal("the backlink survived the embed being removed; refs are not rebuilt wholesale")
	}
}

// Backlinks are an ORACLE if they are not authorized on the target: ask for a
// file id you cannot read and the answer tells you which documents mention it.
func TestBacklinksRequireReadingTheTarget(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)

	// Same fixture as the embed test and for the same reason: the only
	// in-workspace object a fellow member cannot read is a private-channel
	// attachment.
	owner := h.newTenant(t, "oracle-owner")
	targetID := h.requireFiles(t, owner.token, owner.workspaceID)
	priv := h.createTypedChannel(t, owner.token, owner.workspaceID, uniqueSlug("oraclepriv"), "private")
	h.req(t, http.StatusCreated, http.MethodPost, "/api/v1/channels/"+priv+"/messages", owner.token,
		map[string]any{"content": "attached", "file_ids": []string{targetID}})

	me := h.whoami(t, owner.token)
	doc := h.newDocument(t, owner.token, owner.workspaceID,
		fmt.Sprintf("oracle-citing-%d", time.Now().UnixNano()))
	h.appendUpdate(t, *doc.CollabDocumentID, me, []byte{1})
	if code, r := h.project(t, owner.token, doc.ID, map[string]any{
		"seq": 1, "schema_version": 1, "body_text": "x",
		"refs": []map[string]any{{"ref_type": "file", "ref_id": targetID}},
	}); code != http.StatusOK {
		t.Fatalf("project = %d (%+v)", code, r.Error)
	}

	outsider := h.newUser(t, admin, ws, "oracle-outsider")
	h.joinWorkspace(t, owner.token, owner.workspaceID, outsider)

	code, r := h.do(t, http.MethodGet, "/api/v1/drive/refs/file/"+targetID+"/files", outsider.token, nil)
	if code == http.StatusOK {
		t.Fatalf("a caller who cannot read the target listed its backlinks: %s", string(r.Data))
	}
	if code != http.StatusNotFound {
		t.Fatalf("= %d, want 404 — a 403 on an object you cannot see confirms it exists", code)
	}

	// The owner can, so the refusal above was about capability.
	if !backlinkContains(h.backlinks(t, owner.token, "file", targetID), doc.ID) {
		t.Fatal("the owner cannot list their own backlinks; the fixture proves nothing")
	}
}

// The backlink list is scoped to the TARGET's workspace, resolved from the
// target rather than taken from the request — otherwise a caller could ask which
// documents in their own workspace mention an object in somebody else's.
//
// GAP, stated rather than worked around: the per-row acl_key filter inside
// BacklinksTo cannot be exercised in-workspace today, because there is no Drive
// object a workspace member cannot read (README ruling 5 — restricted folders
// are a named cut). The filter is still written and still required: it is what
// makes the list correct the moment a boundary exists, and leaving it out now
// would mean adding it later to a query nobody remembers is unfiltered. What is
// testable today is the tenancy boundary, which is below.
func TestBacklinkListIsScopedToTheTargetsWorkspace(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	root := h.driveRoot(t, admin, ws)

	resp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/workspaces/"+ws+"/drive/files", admin,
		map[string]string{"folder_id": root.ID, "name": "cited.doc", "file_type": "document"})
	var target driveDescriptor
	decodeInto(t, resp.Data, &target)

	doc := h.newDocument(t, admin, ws, fmt.Sprintf("citer-%d", time.Now().UnixNano()))
	h.appendUpdate(t, *doc.CollabDocumentID, me, []byte{1})
	if code, r := h.project(t, admin, doc.ID, map[string]any{
		"seq": 1, "schema_version": 1, "body_text": "x",
		"refs": []map[string]any{{"ref_type": "file", "ref_id": target.ID}},
	}); code != http.StatusOK {
		t.Fatalf("project = %d (%+v)", code, r.Error)
	}
	if !backlinkContains(h.backlinks(t, admin, "file", target.ID), doc.ID) {
		t.Fatal("the backlink is not there at all; the rest of this test proves nothing")
	}

	// Another tenant asking about the same object id gets a 404, not a list and
	// not a 403 — which would confirm the id exists.
	stranger := h.newTenant(t, "backlink-stranger")
	code, _ := h.do(t, http.MethodGet, "/api/v1/drive/refs/file/"+target.ID+"/files", stranger.token, nil)
	if code != http.StatusNotFound {
		t.Fatalf("a stranger listing another tenant's backlinks = %d, want 404", code)
	}
}

// A comment anchored to a range of THIS document cannot be an anchor on a
// comment that lives on ANOTHER one. Without the ownership check the capability
// is being asked about the wrong object.
func TestAnchorMustBelongToTheFileItIsPostedUnder(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)

	a := h.newDocument(t, admin, ws, fmt.Sprintf("anchor-a-%d", time.Now().UnixNano()))
	b := h.newDocument(t, admin, ws, fmt.Sprintf("anchor-b-%d", time.Now().UnixNano()))

	// A comment on document B.
	resp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/comments/file/"+b.ID, admin, map[string]string{"body": "a thought about B"})
	var comment struct {
		ID string `json:"id"`
	}
	decodeInto(t, resp.Data, &comment)

	anchor := map[string]any{
		"anchor_start": []byte{1, 2, 3},
		"anchor_end":   []byte{4, 5, 6},
		"quote":        "some text",
		"block_id":     "blk",
	}

	// Anchoring it under document A is a 404, not a success.
	code, r := h.do(t, http.MethodPut,
		"/api/v1/drive/files/"+a.ID+"/comments/"+comment.ID+"/anchor", admin, anchor)
	if code != http.StatusNotFound {
		t.Fatalf("anchoring B's comment under A = %d, want 404 (%+v)", code, r.Error)
	}

	// Under its own document it works, and comes back on the anchors route.
	if code, r := h.do(t, http.MethodPut,
		"/api/v1/drive/files/"+b.ID+"/comments/"+comment.ID+"/anchor", admin, anchor); code != http.StatusNoContent {
		t.Fatalf("anchoring B's comment under B = %d (%+v)", code, r.Error)
	}

	var anchors []struct {
		CommentID string `json:"comment_id"`
		Quote     string `json:"quote"`
	}
	decodeInto(t, h.req(t, http.StatusOK, http.MethodGet,
		"/api/v1/drive/files/"+b.ID+"/anchors", admin, nil).Data, &anchors)
	found := false
	for _, x := range anchors {
		if x.CommentID == comment.ID {
			found = true
			if x.Quote != "some text" {
				t.Errorf("quote = %q; it is the only thing left when the range is deleted", x.Quote)
			}
		}
	}
	if !found {
		t.Fatal("the stored anchor is not returned by the anchors route")
	}

	// And A's anchor list does not carry it.
	var otherAnchors []struct {
		CommentID string `json:"comment_id"`
	}
	decodeInto(t, h.req(t, http.StatusOK, http.MethodGet,
		"/api/v1/drive/files/"+a.ID+"/anchors", admin, nil).Data, &otherAnchors)
	for _, x := range otherAnchors {
		if x.CommentID == comment.ID {
			t.Fatal("B's anchor is served to A's readers")
		}
	}
}

// Anchoring is gated on `comment`, not on `write`. Docs is the first consumer
// that distinguishes them, and getting it wrong means either a commenter cannot
// place a comment or a reader can.
func TestAnchorRequiresCommentCapability(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)

	doc := h.newDocument(t, admin, ws, fmt.Sprintf("anchor-cap-%d", time.Now().UnixNano()))
	guest := h.newGuest(t, admin, ws, "anchor-guest")
	h.req(t, http.StatusOK, http.MethodPut,
		"/api/v1/drive/file/"+doc.ID+"/shares", admin,
		map[string]string{"subject_id": guest.id, "capability": "comment"})

	resp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/comments/file/"+doc.ID, guest.token, map[string]string{"body": "a question"})
	var comment struct {
		ID string `json:"id"`
	}
	decodeInto(t, resp.Data, &comment)

	if code, r := h.do(t, http.MethodPut,
		"/api/v1/drive/files/"+doc.ID+"/comments/"+comment.ID+"/anchor", guest.token,
		map[string]any{
			"anchor_start": []byte{9}, "anchor_end": []byte{9}, "quote": "q",
		}); code != http.StatusNoContent {
		t.Fatalf("a comment-capability caller anchoring = %d (%+v); the whole point of the "+
			"rung between read and write is that they can", code, r.Error)
	}
}

// rawJSON returns the response body verbatim, for an assertion about a field's
// ABSENCE. Decoding into a struct cannot tell "the key was missing" from "the
// key was there and empty", and that distinction is the leak.
func (h *harness) rawJSON(t *testing.T, token, method, path string, body any) []byte {
	t.Helper()
	_, r := h.do(t, method, path, token, body)
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (h *harness) backlinks(t *testing.T, token, refType, refID string) []struct {
	FileID string `json:"file_id"`
	Name   string `json:"name"`
} {
	t.Helper()
	resp := h.req(t, http.StatusOK, http.MethodGet,
		"/api/v1/drive/refs/"+refType+"/"+refID+"/files", token, nil)
	var out []struct {
		FileID string `json:"file_id"`
		Name   string `json:"name"`
	}
	decodeInto(t, resp.Data, &out)
	return out
}

func backlinkContains(links []struct {
	FileID string `json:"file_id"`
	Name   string `json:"name"`
}, id string) bool {
	for _, l := range links {
		if l.FileID == id {
			return true
		}
	}
	return false
}
