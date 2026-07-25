//go:build integration

package integration

import (
	"bytes"
	"net/http"
	"testing"
)

type comment struct {
	ID         string  `json:"id"`
	ObjectType string  `json:"object_type"`
	ObjectID   string  `json:"object_id"`
	AuthorID   *string `json:"author_id"`
	ParentID   *string `json:"parent_id"`
	Body       string  `json:"body"`
	Deleted    bool    `json:"deleted"`
	ReplyCount int     `json:"reply_count"`
}

func (h *harness) comments(t *testing.T, token, objectType, objectID string) []comment {
	t.Helper()
	resp := h.req(t, http.StatusOK, http.MethodGet,
		"/api/v1/comments/"+objectType+"/"+objectID, token, nil)
	var out []comment
	decodeInto(t, resp.Data, &out)
	return out
}

func (h *harness) comment(t *testing.T, token, objectType, objectID, body, parentID string) comment {
	t.Helper()
	payload := map[string]string{"body": body}
	if parentID != "" {
		payload["parent_id"] = parentID
	}
	resp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/comments/"+objectType+"/"+objectID, token, payload)
	var c comment
	decodeInto(t, resp.Data, &c)
	return c
}

// THE CONTRACT: a comment is authorized by its TARGET and has no rule of its
// own. That is what lets four pillars share one comment system — plan 02 §13 cut
// per-file comments on exactly this promise.
//
// The target here is a Drive file, which is a type the comment package has never
// heard of.
func TestCommentsInheritTheTargetsPermissions(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	root := h.driveRoot(t, admin, ws)

	code, resp := h.uploadToDrive(t, admin, ws, root.ID, "spec.txt", bytes.Repeat([]byte("s"), 100))
	if code != http.StatusCreated {
		t.Fatalf("upload = %d (%+v)", code, resp.Error)
	}
	var file driveDescriptor
	decodeInto(t, resp.Data, &file)

	first := h.comment(t, admin, "file", file.ID, "does this cover the migration?", "")
	if first.ObjectType != "file" || first.ObjectID != file.ID {
		t.Fatalf("the comment names %s:%s, want file:%s", first.ObjectType, first.ObjectID, file.ID)
	}

	// A workspace member reads it, because they can read the file. Drive is
	// workspace-shared, so this is the target's ACL doing the work.
	member := h.newUser(t, admin, ws, "commenter")
	if got := h.comments(t, member.token, "file", file.ID); len(got) != 1 || got[0].ID != first.ID {
		t.Fatalf("a member cannot see a comment on a file they can read: %+v", got)
	}

	// ...and can add one, because a member holds write on the shared Drive and
	// write implies comment on the ladder.
	h.comment(t, member.token, "file", file.ID, "yes, 026 covers it", "")

	// ANOTHER TENANT SEES NOTHING, and gets 404 rather than 403 — a 403 on a
	// comment about something you cannot see confirms the thing exists.
	other := h.newTenant(t, "comment-outsider")
	h.denied(t, http.StatusNotFound, http.MethodGet,
		"/api/v1/comments/file/"+file.ID, other.token, nil)
	h.denied(t, http.StatusNotFound, http.MethodPost,
		"/api/v1/comments/file/"+file.ID, other.token, map[string]string{"body": "hello"})
}

// Threading is ONE level. Arbitrary nesting is a rendering problem with no good
// answer on a phone and a recursive query on every read; a flat list loses the
// "replying to" relation that makes a long thread readable.
func TestCommentThreadingIsOneLevel(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	root := h.driveRoot(t, admin, ws)
	folder := h.createFolder(t, admin, ws, root.ID, "threaded")

	top := h.comment(t, admin, "folder", folder.ID, "top level", "")
	reply := h.comment(t, admin, "folder", folder.ID, "a reply", top.ID)

	// A reply to a reply is refused, and as a CONFLICT rather than a 500: the
	// request is well-formed and the rule is a product decision.
	h.denied(t, http.StatusConflict, http.MethodPost,
		"/api/v1/comments/folder/"+folder.ID, admin,
		map[string]string{"body": "nested too far", "parent_id": reply.ID})

	// The thread lists only the top level, with the reply count — so a long
	// argument under one comment does not make every other reader page through
	// it to reach the next topic.
	thread := h.comments(t, admin, "folder", folder.ID)
	if len(thread) != 1 {
		t.Fatalf("thread = %d entries, want only the top level: %+v", len(thread), thread)
	}
	if thread[0].ReplyCount != 1 {
		t.Errorf("reply_count = %d, want 1", thread[0].ReplyCount)
	}

	resp := h.req(t, http.StatusOK, http.MethodGet,
		"/api/v1/comments/"+top.ID+"/replies", admin, nil)
	var replies []comment
	decodeInto(t, resp.Data, &replies)
	if len(replies) != 1 || replies[0].ID != reply.ID {
		t.Errorf("replies = %+v, want the one reply", replies)
	}
}

// A deleted comment leaves a TOMBSTONE, and its body is genuinely gone.
//
// The row survives because the replies under it still make sense; a thread that
// silently loses its first message reads as corruption. "Deleted" has to mean
// the text is gone, or the feature is a lie the first time somebody reads the
// table.
func TestCommentDeleteLeavesATombstone(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	root := h.driveRoot(t, admin, ws)
	folder := h.createFolder(t, admin, ws, root.ID, "tombstone")

	top := h.comment(t, admin, "folder", folder.ID, "the original point", "")
	h.comment(t, admin, "folder", folder.ID, "which I am answering", top.ID)

	h.req(t, http.StatusNoContent, http.MethodDelete, "/api/v1/comments/"+top.ID, admin, nil)

	thread := h.comments(t, admin, "folder", folder.ID)
	if len(thread) != 1 {
		t.Fatalf("the deleted comment vanished, taking its reply's context: %+v", thread)
	}
	if !thread[0].Deleted {
		t.Error("the comment is not marked deleted")
	}
	if thread[0].Body != "" {
		t.Errorf("the deleted body is still readable: %q", thread[0].Body)
	}
	if thread[0].ReplyCount != 1 {
		t.Errorf("the reply under a deleted comment was lost")
	}

	// Editing a deleted comment is a conflict: there is nothing to edit.
	h.denied(t, http.StatusConflict, http.MethodPatch,
		"/api/v1/comments/"+top.ID, admin, map[string]string{"body": "undelete me"})
}

// Only the author edits, unless you administer the target.
func TestCommentEditingIsAuthorOnly(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	root := h.driveRoot(t, admin, ws)
	folder := h.createFolder(t, admin, ws, root.ID, "authorship")

	member := h.newUser(t, admin, ws, "author")
	theirs := h.comment(t, member.token, "folder", folder.ID, "my comment", "")

	stranger := h.newUser(t, admin, ws, "not-author")
	// A member holds write on the shared Drive, so they may READ and COMMENT —
	// but not rewrite somebody else's words.
	h.denied(t, http.StatusForbidden, http.MethodPatch,
		"/api/v1/comments/"+theirs.ID, stranger.token, map[string]string{"body": "words I did not write"})
	h.denied(t, http.StatusForbidden, http.MethodDelete,
		"/api/v1/comments/"+theirs.ID, stranger.token, nil)

	// The author can.
	resp := h.req(t, http.StatusOK, http.MethodPatch,
		"/api/v1/comments/"+theirs.ID, member.token, map[string]string{"body": "my comment, revised"})
	var updated comment
	decodeInto(t, resp.Data, &updated)
	if updated.Body != "my comment, revised" {
		t.Errorf("body = %q after the author edited it", updated.Body)
	}

	// And so can a workspace admin, who holds admin on the target.
	h.req(t, http.StatusNoContent, http.MethodDelete, "/api/v1/comments/"+theirs.ID, admin, nil)
}

// A comment on something that does not exist must not 500, and it must not say
// which object TYPES exist either.
//
// A well-formed type with no such object and a well-formed type nothing
// registers are the same answer — 404 — because distinguishing them enumerates
// the product's object model to anybody with a text field. A MALFORMED type is
// different: it is a client bug that no amount of retrying fixes, so it says so.
func TestCommentOnAnUnknownTargetIsRefusedCleanly(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)

	for _, path := range []string{
		"/api/v1/comments/hologram/7c9e6679-7425-40de-944b-e07fc1f90ae7",
		"/api/v1/comments/file/7c9e6679-7425-40de-944b-e07fc1f90ae7",
	} {
		h.denied(t, http.StatusNotFound, http.MethodPost, path, admin,
			map[string]string{"body": "hello"})
	}

	// A type acl_object's shape rule refuses outright.
	h.denied(t, http.StatusBadRequest, http.MethodPost,
		"/api/v1/comments/NOT_A_TYPE/7c9e6679-7425-40de-944b-e07fc1f90ae7", admin,
		map[string]string{"body": "hello"})
}
