//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestHealthAndMetrics(t *testing.T) {
	h := getHarness(t)
	if code, _ := h.do(t, "GET", "/health", "", nil); code != 200 {
		t.Errorf("/health = %d, want 200", code)
	}
	// /metrics is plain text, not the JSON envelope.
	res, err := http.Get(h.base + "/metrics")
	if err != nil {
		t.Fatalf("/metrics: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Errorf("/metrics = %d, want 200", res.StatusCode)
	}
}

func TestAuthAndMe(t *testing.T) {
	h := getHarness(t)
	token := h.adminToken(t)

	r := h.req(t, http.StatusOK, "GET", "/api/v1/users/me", token, nil)
	var u struct {
		Email    string `json:"email"`
		Username string `json:"username"`
	}
	decodeInto(t, r.Data, &u)
	if u.Email != adminEmail {
		t.Errorf("me.email = %q, want %q", u.Email, adminEmail)
	}

	// Wrong password is rejected.
	if code, _ := h.do(t, "POST", "/api/v1/auth/login", "", map[string]string{"email": adminEmail, "password": "nope"}); code != 401 {
		t.Errorf("bad login = %d, want 401", code)
	}
	// Missing token is rejected.
	if code, _ := h.do(t, "GET", "/api/v1/users/me", "", nil); code != 401 {
		t.Errorf("no-auth /users/me = %d, want 401", code)
	}
}

// TestMessagingFlow walks the message lifecycle. Every mutation is verified by
// re-reading the row afterwards: the previous version asserted PATCH→200 and
// DELETE→200 and nothing else, so an endpoint that answered 200 and did
// nothing at all passed it.
func TestMessagingFlow(t *testing.T) {
	h := getHarness(t)
	token := h.adminToken(t)
	wsID := h.firstWorkspace(t, token)
	chID := h.createChannel(t, token, wsID, uniqueSlug("msg"))

	msg := h.postMessage(t, token, chID, "hello integration")

	// List returns it (and a pagination meta envelope).
	r := h.req(t, http.StatusOK, "GET", "/api/v1/channels/"+chID+"/messages?limit=10", token, nil)
	if r.Meta == nil {
		t.Fatal("message list missing pagination meta")
	}
	var msgs []postedMessage
	decodeInto(t, r.Data, &msgs)
	if !containsMessage(msgs, msg.ID) {
		t.Fatalf("list did not return the message just sent (%d rows)", len(msgs))
	}

	// React, then confirm the reaction is hydrated on read.
	h.req(t, http.StatusCreated, "POST", "/api/v1/channels/"+chID+"/messages/"+msg.ID+"/reactions", token,
		map[string]string{"emoji": "👍"})
	got := h.getMessage(t, token, chID, msg.ID)
	if len(got.Reactions) != 1 || got.Reactions[0].Emoji != "👍" {
		t.Errorf("reactions not hydrated: %+v", got.Reactions)
	}

	// Removing it again really removes it.
	h.req(t, http.StatusOK, "DELETE",
		"/api/v1/channels/"+chID+"/messages/"+msg.ID+"/reactions/"+url.PathEscape("👍"), token, nil)
	if got = h.getMessage(t, token, chID, msg.ID); len(got.Reactions) != 0 {
		t.Errorf("reaction survived deletion: %+v", got.Reactions)
	}

	// Pin, and confirm the flag is persisted rather than just acknowledged.
	h.req(t, http.StatusOK, "POST", "/api/v1/channels/"+chID+"/messages/"+msg.ID+"/pin", token, nil)
	if got = h.getMessage(t, token, chID, msg.ID); !got.IsPinned {
		t.Error("pin returned 200 but the message is not pinned")
	}
	pins := h.req(t, http.StatusOK, "GET", "/api/v1/channels/"+chID+"/pins", token, nil)
	if pins.Meta == nil {
		t.Error("pin list missing pagination meta")
	}
	var pinned []postedMessage
	decodeInto(t, pins.Data, &pinned)
	if !containsMessage(pinned, msg.ID) {
		t.Error("pinned message missing from the pin list")
	}
	h.req(t, http.StatusOK, "DELETE", "/api/v1/channels/"+chID+"/messages/"+msg.ID+"/pin", token, nil)
	if got = h.getMessage(t, token, chID, msg.ID); got.IsPinned {
		t.Error("unpin returned 200 but the message is still pinned")
	}

	// Edit: the stored content must actually change.
	h.req(t, http.StatusOK, "PATCH", "/api/v1/channels/"+chID+"/messages/"+msg.ID, token,
		map[string]string{"content": "edited"})
	got = h.getMessage(t, token, chID, msg.ID)
	if got.Content != "edited" {
		t.Errorf("after edit content = %q, want %q", got.Content, "edited")
	}
	if !got.IsEdited {
		t.Error("after edit is_edited = false")
	}

	// Delete: the message must stop being readable and stop being listed.
	h.req(t, http.StatusOK, "DELETE", "/api/v1/channels/"+chID+"/messages/"+msg.ID, token, nil)
	h.denied(t, http.StatusNotFound, "GET", "/api/v1/channels/"+chID+"/messages/"+msg.ID, token, nil)
	r = h.req(t, http.StatusOK, "GET", "/api/v1/channels/"+chID+"/messages?limit=50", token, nil)
	decodeInto(t, r.Data, &msgs)
	if containsMessage(msgs, msg.ID) {
		t.Error("deleted message is still listed")
	}
}

func containsMessage(msgs []postedMessage, id string) bool {
	for _, m := range msgs {
		if m.ID == id {
			return true
		}
	}
	return false
}

// TestRBACAndMembership exercises the invite flow, then verifies a plain member
// is denied admin endpoints and non-member channel access.
func TestRBACAndMembership(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	wsID := h.firstWorkspace(t, admin)

	// Admin endpoint works for the admin.
	h.req(t, http.StatusOK, "GET", "/api/v1/admin/stats", admin, nil)

	// workspace_id is mandatory and must be a workspace the caller administers.
	h.denied(t, http.StatusBadRequest, "POST", "/api/v1/admin/invitations", admin,
		map[string]string{"email": uniqueSlug("nows") + "@demo.local", "role": "member"})

	member := h.newUser(t, admin, wsID, "member")

	// Member is denied the admin surface: they administer no workspace.
	h.denied(t, http.StatusForbidden, "GET", "/api/v1/admin/stats", member.token, nil)
	h.denied(t, http.StatusForbidden, "GET", "/api/v1/admin/users", member.token, nil)

	// Member is not in a channel the admin creates → membership-gated reads 403.
	chID := h.createTypedChannel(t, admin, wsID, uniqueSlug("priv"), "private")
	h.denied(t, http.StatusForbidden, "GET",
		"/api/v1/workspaces/"+wsID+"/channels/"+chID+"/members", member.token, nil)
	h.denied(t, http.StatusForbidden, "POST",
		"/api/v1/channels/"+chID+"/messages", member.token, map[string]string{"content": "x"})
	// A private channel is not joinable at all, even from inside the workspace.
	h.denied(t, http.StatusForbidden, "POST",
		"/api/v1/workspaces/"+wsID+"/channels/"+chID+"/join", member.token, nil)
}

// TestPaginationIsTotal is the regression test for the cursor tiebreaker.
//
// Ordering on created_at alone is not a total order: the scheduled-message
// batch promotion stamps a whole batch with one transaction timestamp. A
// strictly-less-than filter on the timestamp alone then skips every tied row
// but one at each page boundary, so messages silently disappeared from history.
// The cursor is (created_at, id) now; this forces the tie and pages through one
// row at a time, asserting every message comes back exactly once.
func TestPaginationIsTotal(t *testing.T) {
	h := getHarness(t)
	token := h.adminToken(t)
	wsID := h.firstWorkspace(t, token)
	chID := h.createChannel(t, token, wsID, uniqueSlug("page"))

	const n = 5
	want := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		want[h.postMessage(t, token, chID, fmt.Sprintf("tied-%d", i)).ID] = true
	}

	// Collapse every created_at onto one instant, which is what the batch
	// promotion produces and what no API call can produce on its own.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tied := time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)
	if _, err := h.app.DB.Exec(ctx,
		`UPDATE messages SET created_at = $1 WHERE channel_id = $2`, tied, chID); err != nil {
		t.Fatalf("collapse created_at: %v", err)
	}

	seen := map[string]int{}
	cursor := ""
	pages := 0
	for pages = 0; pages <= n+2; pages++ {
		path := "/api/v1/channels/" + chID + "/messages?limit=1"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		r := h.req(t, http.StatusOK, "GET", path, token, nil)
		if r.Meta == nil {
			t.Fatalf("page %d: missing meta envelope", pages)
		}
		var got []postedMessage
		decodeInto(t, r.Data, &got)
		if len(got) > 1 {
			t.Fatalf("page %d returned %d rows for limit=1", pages, len(got))
		}
		for _, m := range got {
			seen[m.ID]++
		}
		if !r.Meta.HasMore {
			break
		}
		if r.Meta.Cursor == "" {
			t.Fatalf("page %d: has_more with no cursor", pages)
		}
		if r.Meta.Cursor == cursor {
			t.Fatalf("page %d: cursor did not advance (%q)", pages, cursor)
		}
		cursor = r.Meta.Cursor
	}
	if pages > n {
		t.Fatalf("paging did not terminate after %d pages for %d messages", pages, n)
	}

	for id := range want {
		switch seen[id] {
		case 1: // exactly once, as required
		case 0:
			t.Errorf("message %s was skipped by the cursor", id)
		default:
			t.Errorf("message %s was returned %d times", id, seen[id])
		}
	}
	for id, count := range seen {
		if !want[id] {
			t.Errorf("unexpected message %s (x%d) in the channel", id, count)
		}
	}
}

// TestPaginationRejectsBadInput pins ParsePagination's new contract. Both a
// forged cursor and an out-of-range limit used to be swallowed — the first
// silently returned page 1, the second was silently clamped — so a client had
// no way to tell a broken request from a working one.
func TestPaginationRejectsBadInput(t *testing.T) {
	h := getHarness(t)
	token := h.adminToken(t)
	wsID := h.firstWorkspace(t, token)
	chID := h.createChannel(t, token, wsID, uniqueSlug("badpage"))

	base := "/api/v1/channels/" + chID + "/messages"
	for _, q := range []string{
		"?cursor=not-base64!!",
		"?cursor=" + url.QueryEscape("bm90LWEtdGltZXN0YW1w"), // valid base64, junk payload
		"?limit=0",
		"?limit=-1",
		"?limit=101",
		"?limit=abc",
	} {
		h.denied(t, http.StatusBadRequest, "GET", base+q, token, nil)
	}

	// A cursor issued by the server is still accepted.
	h.postMessage(t, token, chID, "one")
	h.postMessage(t, token, chID, "two")
	r := h.req(t, http.StatusOK, "GET", base+"?limit=1", token, nil)
	if r.Meta == nil || r.Meta.Cursor == "" {
		t.Fatal("first page issued no cursor")
	}
	h.req(t, http.StatusOK, "GET", base+"?limit=1&cursor="+url.QueryEscape(r.Meta.Cursor), token, nil)
}

// TestUnreadCounts checks that the badge counts and that marking read clears
// it. PUT .../read answers with the recomputed Unread payload
// ({channel_id, unread_count, last_read_at}), not a message string.
func TestUnreadCounts(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	owner := h.newTenant(t, "unrd")
	chID := h.createChannel(t, owner.token, owner.workspaceID, uniqueSlug("unread"))

	reader := h.newUser(t, admin, h.firstWorkspace(t, admin), "rdr")
	h.joinWorkspace(t, owner.token, owner.workspaceID, reader)
	h.req(t, http.StatusOK, "POST",
		"/api/v1/workspaces/"+owner.workspaceID+"/channels/"+chID+"/join", reader.token, nil)

	unread := func(who string) int {
		t.Helper()
		r := h.req(t, http.StatusOK, "GET", "/api/v1/channels/"+chID+"/unread", who, nil)
		var u struct {
			ChannelID string `json:"channel_id"`
			Count     int    `json:"unread_count"`
		}
		decodeInto(t, r.Data, &u)
		if u.ChannelID != chID {
			t.Errorf("unread.channel_id = %q, want %q", u.ChannelID, chID)
		}
		return u.Count
	}

	if n := unread(reader.token); n != 0 {
		t.Fatalf("fresh membership has %d unread, want 0", n)
	}

	for i := 0; i < 3; i++ {
		h.postMessage(t, owner.token, chID, fmt.Sprintf("unread-%d", i))
	}
	if n := unread(reader.token); n != 3 {
		t.Errorf("unread after 3 posts = %d, want 3", n)
	}
	// The author never accrues unread against their own messages.
	if n := unread(owner.token); n != 0 {
		t.Errorf("author unread = %d, want 0", n)
	}

	r := h.req(t, http.StatusOK, "PUT", "/api/v1/channels/"+chID+"/read", reader.token, nil)
	var marked struct {
		ChannelID  string     `json:"channel_id"`
		Count      int        `json:"unread_count"`
		LastReadAt *time.Time `json:"last_read_at"`
	}
	decodeInto(t, r.Data, &marked)
	if marked.ChannelID != chID {
		t.Errorf("mark-read channel_id = %q, want %q", marked.ChannelID, chID)
	}
	if marked.Count != 0 {
		t.Errorf("mark-read unread_count = %d, want 0", marked.Count)
	}
	if marked.LastReadAt == nil || marked.LastReadAt.IsZero() {
		t.Error("mark-read returned no last_read_at")
	}
	if n := unread(reader.token); n != 0 {
		t.Errorf("unread after mark-read = %d, want 0", n)
	}

	// A later message counts again, and marking read up to it clears it.
	latest := h.postMessage(t, owner.token, chID, "after-read")
	if n := unread(reader.token); n != 1 {
		t.Errorf("unread after one more post = %d, want 1", n)
	}
	h.req(t, http.StatusOK, "PUT", "/api/v1/channels/"+chID+"/read", reader.token,
		map[string]string{"message_id": latest.ID})
	if n := unread(reader.token); n != 0 {
		t.Errorf("unread after mark-read(message_id) = %d, want 0", n)
	}

	// A message id from another channel must not move this channel's marker.
	other := h.createChannel(t, owner.token, owner.workspaceID, uniqueSlug("unread-other"))
	foreign := h.postMessage(t, owner.token, other, "elsewhere")
	h.denied(t, http.StatusBadRequest, "PUT", "/api/v1/channels/"+chID+"/read", reader.token,
		map[string]string{"message_id": foreign.ID})

	// Non-members have no read state to speak of.
	stranger := h.newTenant(t, "unrdx")
	h.denied(t, http.StatusForbidden, "GET", "/api/v1/channels/"+chID+"/unread", stranger.token, nil)
	h.denied(t, http.StatusForbidden, "PUT", "/api/v1/channels/"+chID+"/read", stranger.token, nil)
}

// TestPaginatedEnvelopes pins the list endpoints that became JSONList
// envelopes. A client that reads `data` as the whole body breaks on all of
// them, so the shape is worth asserting even where the page is empty.
func TestPaginatedEnvelopes(t *testing.T) {
	h := getHarness(t)
	owner := h.newTenant(t, "envl")
	chID := h.createChannel(t, owner.token, owner.workspaceID, uniqueSlug("envl"))
	root := h.postMessage(t, owner.token, chID, "root")

	h.req(t, http.StatusCreated, "POST", "/api/v1/messages/"+root.ID+"/thread", owner.token,
		map[string]string{"content": "reply"})
	h.req(t, http.StatusCreated, "POST", "/api/v1/messages/"+root.ID+"/bookmark", owner.token, nil)
	h.req(t, http.StatusCreated, "POST", "/api/v1/channels/"+chID+"/messages", owner.token,
		map[string]any{"content": "later", "scheduled_at": time.Now().Add(time.Hour)})

	for _, path := range []string{
		"/api/v1/messages/" + root.ID + "/thread",
		"/api/v1/channels/" + chID + "/pins",
		"/api/v1/channels/" + chID + "/scheduled",
		"/api/v1/bookmarks",
		"/api/v1/admin/audit-logs",
	} {
		r := h.req(t, http.StatusOK, "GET", path, owner.token, nil)
		if r.Meta == nil {
			t.Errorf("GET %s: no pagination meta envelope", path)
		}
		// `data` must always be a JSON array, never null.
		var rows []json.RawMessage
		decodeInto(t, r.Data, &rows)
	}
}
