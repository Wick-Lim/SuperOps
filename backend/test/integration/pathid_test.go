//go:build integration

package integration

import (
	"net/http"
	"testing"
)

// A MISTYPED ID IS THE CALLER'S MISTAKE AND MUST READ AS ONE.
//
// Every `{*_id}` in the route table is a UUID column, so a value that is not
// one fails the cast with SQLSTATE 22P02 and the handler wraps that as an
// internal error. Measured across the table before ValidateIDPathParams
// existed: 38 of the 107 routes carrying an id parameter answered 5xx for
// `not-a-uuid`.
//
// It was NOT a coverage problem, which is the part worth recording — 30 of
// those 38 already had an integration test driving them. A test of the happy
// path says nothing about what a malformed id does, so counting covered routes
// would have reported this area as done.
//
// The routes below are a sample across the subsystems that were failing, plus
// two that were already correct, so a regression that removes the middleware
// and a regression that removes a handler's own check both show up here.
func TestAMistypedIDIsRefusedRatherThanA500(t *testing.T) {
	h := getHarness(t)
	owner := h.newTenant(t, "badid")
	ws := owner.workspaceID

	for _, c := range []struct {
		name, method, path string
		body               any
	}{
		{"webhook delete", http.MethodDelete, "/api/v1/webhooks/not-a-uuid", nil},
		{"webhook token rotation", http.MethodPut, "/api/v1/webhooks/not-a-uuid/token", map[string]any{}},
		{"emoji delete", http.MethodDelete, "/api/v1/workspaces/" + ws + "/emojis/not-a-uuid", nil},
		{"channel archive", http.MethodPost, "/api/v1/workspaces/" + ws + "/channels/not-a-uuid/archive", map[string]any{}},
		{"channel leave", http.MethodPost, "/api/v1/workspaces/" + ws + "/channels/not-a-uuid/leave", map[string]any{}},
		{"inbox undone", http.MethodPut, "/api/v1/inbox/not-a-uuid/undone", map[string]any{}},
		{"inbox unread", http.MethodPut, "/api/v1/inbox/not-a-uuid/unread", map[string]any{}},
		{"drive file delete", http.MethodDelete, "/api/v1/drive/files/not-a-uuid", nil},
		{"comment delete", http.MethodDelete, "/api/v1/comments/not-a-uuid", nil},
		{"workflow archive", http.MethodDelete, "/api/v1/workflows/not-a-uuid", nil},
		{"workflow run", http.MethodGet, "/api/v1/runs/not-a-uuid", nil},
		{"member removal", http.MethodDelete, "/api/v1/workspaces/" + ws + "/members/not-a-uuid", nil},

		// Already answered 400 from a check written out in the handler. Kept so
		// removing that check is not silently covered by the middleware, and so
		// the middleware is not the only thing being asserted.
		{"channel typing", http.MethodGet, "/api/v1/channels/not-a-uuid/typing", nil},
		{"collab snapshot", http.MethodPost, "/api/v1/collab/documents/not-a-uuid/snapshot",
			map[string]any{"through_seq": 1, "snapshot": "AAA="}},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, resp := h.do(t, c.method, c.path, owner.token, c.body)
			if code >= 500 {
				t.Fatalf("= %d %+v: a mistyped id reached Postgres and came back "+
					"as an internal error", code, resp.Error)
			}
			if code != http.StatusBadRequest {
				t.Errorf("= %d, want 400 (%+v)", code, resp.Error)
			}
		})
	}
}

// AND A WELL-FORMED ID STILL REACHES THE HANDLER.
//
// A guard that refuses everything passes the test above. These ids are valid
// and belong to nothing, so the answer has to come from the handler — 404 or
// 403, never the 400 the middleware writes.
func TestAWellFormedIDIsNotRefusedByTheGuard(t *testing.T) {
	h := getHarness(t)
	owner := h.newTenant(t, "goodid")
	const nobody = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"

	for _, c := range []struct {
		name, method, path string
	}{
		{"a webhook that does not exist", http.MethodDelete, "/api/v1/webhooks/" + nobody},
		{"an inbox item that does not exist", http.MethodPut, "/api/v1/inbox/" + nobody + "/unread"},
		{"a comment that does not exist", http.MethodDelete, "/api/v1/comments/" + nobody},
		{"a run that does not exist", http.MethodGet, "/api/v1/runs/" + nobody},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, resp := h.do(t, c.method, c.path, owner.token, map[string]any{})
			if code == http.StatusBadRequest {
				t.Errorf("= 400 (%+v): a valid id was refused as malformed", resp.Error)
			}
			if code >= 500 {
				t.Errorf("= %d (%+v)", code, resp.Error)
			}
		})
	}
}
