//go:build integration

// Multi-tenancy regression tests.
//
// Every test in this file is written as the attack that used to work, against
// the fully-wired app. The bugs they cover were not theoretical: a workspace
// was a label, not a boundary. Search filtered on nothing but the workspace id
// in the URL, /channels/browse and /join had no membership gate at all, every
// /admin/* query ran instance-wide for anyone who owned any workspace, and
// message-addressed writes authorized against the channel id in the URL rather
// than the channel the message actually lives in.
//
// The shape of each test is: build two tenants that share nothing, have one of
// them create something, then prove the other cannot see or touch it — and
// prove afterwards that the target really is unchanged, because a 403 that
// arrives after the write has already happened is not a fix.
package integration

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestCrossTenantSearch covers the worst of the search bugs: the Meilisearch
// filter was `workspace_id = <whatever the URL said>`, with no membership check
// and no channel allowlist, so any authenticated user could full-text search
// any workspace — including its private channels and its DMs.
func TestCrossTenantSearch(t *testing.T) {
	h := getHarness(t)
	h.requireSearch(t)

	admin := h.adminToken(t)
	seed := h.firstWorkspace(t, admin)
	a := h.newTenant(t, "srcha")
	b := h.newTenant(t, "srchb")

	pub := h.createChannel(t, b.token, b.workspaceID, uniqueSlug("srchpub"))
	priv := h.createTypedChannel(t, b.token, b.workspaceID, uniqueSlug("srchpriv"), "private")

	needle := fmt.Sprintf("zoltar%d", time.Now().UnixNano())
	pubMsg := h.postMessage(t, b.token, pub, "public "+needle)
	privMsg := h.postMessage(t, b.token, priv, "private "+needle)
	h.indexMessage(t, b.workspaceID, pubMsg)
	h.indexMessage(t, b.workspaceID, privMsg)

	// Control. Without it every assertion below could be passing because the
	// index is empty rather than because the filter works.
	if got := h.searchHits(t, b.token, b.workspaceID, needle); len(got) != 2 {
		t.Fatalf("owner of B found %d/2 of its own messages (%v) — index fixture is broken", len(got), got)
	}

	// 1. A member of another tenant cannot search workspace B at all.
	h.denied(t, http.StatusForbidden, "GET",
		"/api/v1/workspaces/"+b.workspaceID+"/search?q="+needle, a.token, nil)

	// ...nor reach B's content by searching their own workspace.
	if got := h.searchHits(t, a.token, a.workspaceID, needle); len(got) != 0 {
		t.Errorf("tenant A's own-workspace search leaked %v", got)
	}

	// ...nor by naming one of B's channels explicitly.
	h.denied(t, http.StatusForbidden, "GET",
		"/api/v1/workspaces/"+a.workspaceID+"/search?q="+needle+"&channel="+pub, a.token, nil)

	// 2. A member of B who is not in B's private channel sees only the public
	// hit. This is the second layer: workspace membership is not permission to
	// read every channel in the workspace.
	outsider := h.newUser(t, admin, seed, "srchc")
	h.joinWorkspace(t, b.token, b.workspaceID, outsider)

	got := h.searchHits(t, outsider.token, b.workspaceID, needle)
	if len(got) != 1 || got[0] != pubMsg.ID {
		t.Errorf("workspace member search returned %v, want only the public message %s (private was %s)",
			got, pubMsg.ID, privMsg.ID)
	}

	// Naming the private channel explicitly cannot widen the readable set.
	h.denied(t, http.StatusForbidden, "GET",
		"/api/v1/workspaces/"+b.workspaceID+"/search?q="+needle+"&channel="+priv, outsider.token, nil)
}

// TestCrossTenantChannelAccess covers Browse/Join/Create, which had no
// workspace membership gate, and the {workspace_id} path segment, which was
// decorative: every channel-scoped route resolved by channel id alone, so any
// id could be addressed through any workspace.
func TestCrossTenantChannelAccess(t *testing.T) {
	h := getHarness(t)
	a := h.newTenant(t, "xchna")
	b := h.newTenant(t, "xchnb")

	pub := h.createChannel(t, b.token, b.workspaceID, uniqueSlug("xchn"))
	h.postMessage(t, b.token, pub, "tenant b only")

	inB := "/api/v1/workspaces/" + b.workspaceID
	inA := "/api/v1/workspaces/" + a.workspaceID

	// The workspace itself is not even enumerable.
	h.denied(t, http.StatusNotFound, "GET", inB, a.token, nil)
	h.denied(t, http.StatusNotFound, "GET", inB+"/members", a.token, nil)

	// Browsing, listing, creating and joining are all refused.
	h.denied(t, http.StatusForbidden, "GET", inB+"/channels/browse", a.token, nil)
	h.denied(t, http.StatusForbidden, "GET", inB+"/channels", a.token, nil)
	h.denied(t, http.StatusForbidden, "POST", inB+"/channels/"+pub+"/join", a.token, nil)
	h.denied(t, http.StatusForbidden, "GET", inB+"/channels/"+pub, a.token, nil)
	h.denied(t, http.StatusForbidden, "GET", inB+"/channels/"+pub+"/members", a.token, nil)
	slug := uniqueSlug("xchn-intruder")
	h.denied(t, http.StatusForbidden, "POST", inB+"/channels", a.token,
		map[string]string{"name": slug, "slug": slug, "type": "public"})

	// Laundering B's channel id through a workspace A *is* a member of answers
	// 404, so the id stays unprobeable.
	h.denied(t, http.StatusNotFound, "GET", inA+"/channels/"+pub, a.token, nil)
	h.denied(t, http.StatusNotFound, "POST", inA+"/channels/"+pub+"/join", a.token, nil)
	h.denied(t, http.StatusNotFound, "GET", inA+"/channels/"+pub+"/members", a.token, nil)

	// The channel-addressed routes, which carry no workspace id at all, stay shut.
	h.denied(t, http.StatusForbidden, "GET", "/api/v1/channels/"+pub+"/messages", a.token, nil)
	h.denied(t, http.StatusForbidden, "POST", "/api/v1/channels/"+pub+"/messages", a.token,
		map[string]string{"content": "intruder was here"})
	h.denied(t, http.StatusForbidden, "GET", "/api/v1/channels/"+pub+"/unread", a.token, nil)
	h.denied(t, http.StatusForbidden, "PUT", "/api/v1/channels/"+pub+"/read", a.token, nil)
	h.denied(t, http.StatusForbidden, "GET", "/api/v1/channels/"+pub+"/pins", a.token, nil)
	h.denied(t, http.StatusForbidden, "GET", "/api/v1/channels/"+pub+"/scheduled", a.token, nil)

	// Control: the same calls work inside A's own workspace, so the refusals
	// above are the authorization and not a broken route.
	own := h.createChannel(t, a.token, a.workspaceID, uniqueSlug("xchnown"))
	h.req(t, http.StatusOK, "GET", inA+"/channels/browse", a.token, nil)
	h.req(t, http.StatusOK, "GET", "/api/v1/channels/"+own+"/messages", a.token, nil)

	// And nothing the intruder tried actually landed.
	r := h.req(t, http.StatusOK, "GET", "/api/v1/channels/"+pub+"/messages?limit=50", b.token, nil)
	var msgs []postedMessage
	decodeInto(t, r.Data, &msgs)
	if len(msgs) != 1 || msgs[0].Content != "tenant b only" {
		t.Errorf("B's channel now holds %d messages: %+v", len(msgs), msgs)
	}
	members := h.req(t, http.StatusOK, "GET", inB+"/channels/"+pub+"/members", b.token, nil)
	var roster []struct {
		UserID string `json:"user_id"`
	}
	decodeInto(t, members.Data, &roster)
	for _, m := range roster {
		if m.UserID == a.id {
			t.Error("the intruder ended up on B's channel roster")
		}
	}
}

// TestAdminEndpointsAreWorkspaceScoped covers the /admin/* surface. The gate in
// front of it only proves "administers at least one workspace" — anyone who
// creates a workspace is its owner — so each handler has to re-scope to
// authz.AdminWorkspaceIDs. It used to run every query instance-wide.
func TestAdminEndpointsAreWorkspaceScoped(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	seed := h.firstWorkspace(t, admin)

	a := h.newTenant(t, "adma")
	b := h.newTenant(t, "admb")

	// B has an extra member, a channel, a message and a pending invitation.
	bMember := h.newUser(t, admin, seed, "admbm")
	h.joinWorkspace(t, b.token, b.workspaceID, bMember)
	bChan := h.createChannel(t, b.token, b.workspaceID, uniqueSlug("admb"))
	h.postMessage(t, b.token, bChan, "b-only")
	bPending := uniqueSlug("admbpending") + "@demo.local"
	h.invite(t, b.token, b.workspaceID, bPending, "member")

	// A has exactly one channel, one message and one pending invitation.
	aChan := h.createChannel(t, a.token, a.workspaceID, uniqueSlug("adma"))
	h.postMessage(t, a.token, aChan, "a-only")
	aPending := uniqueSlug("admapending") + "@demo.local"
	h.invite(t, a.token, a.workspaceID, aPending, "member")

	// --- stats count A's workspace and nothing else ---
	var stats struct {
		Users      int `json:"users"`
		Workspaces int `json:"workspaces"`
		Channels   int `json:"channels"`
		Messages   int `json:"messages"`
	}
	decodeInto(t, h.req(t, http.StatusOK, "GET", "/api/v1/admin/stats", a.token, nil).Data, &stats)
	if want := (struct{ w, u, c, m int }{1, 1, 1, 1}); stats.Workspaces != want.w ||
		stats.Users != want.u || stats.Channels != want.c || stats.Messages != want.m {
		t.Errorf("admin stats = %+v, want exactly A's own workspace (1/1/1/1)", stats)
	}

	// --- user list is A alone ---
	var users []struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	decodeInto(t, h.req(t, http.StatusOK, "GET", "/api/v1/admin/users", a.token, nil).Data, &users)
	sawSelf := false
	for _, u := range users {
		switch u.ID {
		case a.id:
			sawSelf = true
		case b.id, bMember.id:
			t.Errorf("admin user list leaked %s from another tenant", u.Email)
		default:
			t.Errorf("admin user list leaked an unrelated account %s", u.Email)
		}
	}
	if !sawSelf {
		t.Error("admin user list did not contain the caller")
	}

	// --- invitation list is scoped, and never carries the token ---
	var invites []map[string]any
	decodeInto(t, h.req(t, http.StatusOK, "GET", "/api/v1/admin/invitations", a.token, nil).Data, &invites)
	if len(invites) != 1 {
		t.Errorf("admin invitation list returned %d rows, want only A's one pending invite", len(invites))
	}
	for _, inv := range invites {
		if _, ok := inv["token"]; ok {
			t.Error("admin invitation list returned the raw token")
		}
		if got := inv["workspace_id"]; got != a.workspaceID {
			t.Errorf("invitation belongs to workspace %v, want %s", got, a.workspaceID)
		}
		if inv["email"] == bPending {
			t.Error("admin invitation list leaked another tenant's pending invite")
		}
	}

	// --- audit logs are scoped ---
	var logs []struct {
		WorkspaceID *string `json:"workspace_id"`
	}
	decodeInto(t, h.req(t, http.StatusOK, "GET", "/api/v1/admin/audit-logs", a.token, nil).Data, &logs)
	for _, l := range logs {
		if l.WorkspaceID != nil && *l.WorkspaceID != a.workspaceID {
			t.Errorf("audit log from workspace %s leaked to an admin of %s", *l.WorkspaceID, a.workspaceID)
		}
	}

	// --- a stranger cannot be deactivated, demoted or invited over ---
	h.denied(t, http.StatusNotFound, "PATCH", "/api/v1/admin/users/"+b.id, a.token,
		map[string]any{"is_active": false})
	h.denied(t, http.StatusNotFound, "PATCH", "/api/v1/admin/users/"+bMember.id, a.token,
		map[string]any{"role": "guest", "workspace_id": b.workspaceID})
	h.denied(t, http.StatusForbidden, "POST", "/api/v1/admin/invitations", a.token,
		map[string]string{"workspace_id": b.workspaceID, "email": uniqueSlug("admx") + "@demo.local", "role": "admin"})

	// B is untouched: still active, still able to log in, still an owner.
	h.req(t, http.StatusOK, "GET", "/api/v1/users/me", b.token, nil)
	h.login(t, b.email, b.pass)
	var bUsers []struct {
		ID       string `json:"id"`
		IsActive bool   `json:"is_active"`
	}
	decodeInto(t, h.req(t, http.StatusOK, "GET", "/api/v1/admin/users", b.token, nil).Data, &bUsers)
	for _, u := range bUsers {
		if u.ID == b.id && !u.IsActive {
			t.Error("the cross-tenant deactivation went through after all")
		}
	}

	// A retains authority inside its own workspace, so the scoping is not just
	// "deny everything". (Done last: it changes the roster the assertions above
	// depend on.)
	aMember := h.newUser(t, admin, seed, "admam")
	h.joinWorkspace(t, a.token, a.workspaceID, aMember)
	h.req(t, http.StatusOK, "PATCH", "/api/v1/admin/users/"+aMember.id, a.token,
		map[string]any{"role": "guest", "workspace_id": a.workspaceID})
	h.req(t, http.StatusOK, "PATCH", "/api/v1/admin/users/"+aMember.id, a.token,
		map[string]any{"is_active": false})

	decodeInto(t, h.req(t, http.StatusOK, "GET", "/api/v1/admin/users", a.token, nil).Data, &bUsers)
	found := false
	for _, u := range bUsers {
		if u.ID == aMember.id {
			found = true
			if u.IsActive {
				t.Error("in-workspace deactivation reported 200 but did not take effect")
			}
		}
	}
	if !found {
		t.Error("A's own new member is missing from A's admin user list")
	}
}

// TestCrossChannelIDOR covers authorization by URL. Every message-addressed
// route used to check membership of the {channel_id} in the path while acting
// on the message named by {message_id} — so a member of any channel anywhere
// could react to, pin, edit, delete, thread, bookmark or forward a message out
// of a private channel of another tenant. Pinning in particular broadcasts the
// message body, which turned the IDOR into a content leak.
func TestCrossChannelIDOR(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	seed := h.firstWorkspace(t, admin)

	a := h.newTenant(t, "idora")
	b := h.newTenant(t, "idorb")

	x := h.createChannel(t, a.token, a.workspaceID, uniqueSlug("idorx"))
	y := h.createTypedChannel(t, b.token, b.workspaceID, uniqueSlug("idory"), "private")
	victim := h.postMessage(t, b.token, y, "not yours")

	// Every message-addressed route, addressed through a channel the caller IS
	// a member of, pointing at a message they are not allowed to read.
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/v1/channels/" + x + "/messages/" + victim.ID, nil},
		{"PATCH", "/api/v1/channels/" + x + "/messages/" + victim.ID, map[string]string{"content": "pwned"}},
		{"DELETE", "/api/v1/channels/" + x + "/messages/" + victim.ID, nil},
		{"POST", "/api/v1/channels/" + x + "/messages/" + victim.ID + "/reactions", map[string]string{"emoji": "👀"}},
		{"DELETE", "/api/v1/channels/" + x + "/messages/" + victim.ID + "/reactions/x", nil},
		{"POST", "/api/v1/channels/" + x + "/messages/" + victim.ID + "/pin", nil},
		{"DELETE", "/api/v1/channels/" + x + "/messages/" + victim.ID + "/pin", nil},
		{"POST", "/api/v1/channels/" + x + "/messages/" + victim.ID + "/forward", map[string]string{"target_channel_id": x}},
		{"GET", "/api/v1/messages/" + victim.ID + "/reactions", nil},
		{"GET", "/api/v1/messages/" + victim.ID + "/thread", nil},
		{"POST", "/api/v1/messages/" + victim.ID + "/thread", map[string]string{"content": "reply"}},
		{"POST", "/api/v1/messages/" + victim.ID + "/bookmark", nil},
	} {
		h.denied(t, http.StatusForbidden, c.method, c.path, a.token, c.body)
	}

	// The same hole inside one workspace: a member of a public channel reaching
	// a message in a private channel of the same workspace.
	mate := h.newUser(t, admin, seed, "idorm")
	h.joinWorkspace(t, a.token, a.workspaceID, mate)
	h.req(t, http.StatusOK, "POST",
		"/api/v1/workspaces/"+a.workspaceID+"/channels/"+x+"/join", mate.token, nil)
	hidden := h.createTypedChannel(t, a.token, a.workspaceID, uniqueSlug("idorh"), "private")
	secret := h.postMessage(t, a.token, hidden, "same tenant, other channel")
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/v1/channels/" + x + "/messages/" + secret.ID + "/reactions", map[string]string{"emoji": "👀"}},
		{"POST", "/api/v1/channels/" + x + "/messages/" + secret.ID + "/pin", nil},
		{"GET", "/api/v1/channels/" + x + "/messages/" + secret.ID, nil},
	} {
		h.denied(t, http.StatusForbidden, c.method, c.path, mate.token, c.body)
	}

	// Neither victim was modified.
	for _, v := range []struct {
		token, channel string
		msg            postedMessage
	}{
		{b.token, y, victim},
		{a.token, hidden, secret},
	} {
		got := h.getMessage(t, v.token, v.channel, v.msg.ID)
		if got.Content != v.msg.Content {
			t.Errorf("message %s content = %q, want %q", v.msg.ID, got.Content, v.msg.Content)
		}
		if got.IsPinned {
			t.Errorf("message %s was pinned by an outsider", v.msg.ID)
		}
		if len(got.Reactions) != 0 {
			t.Errorf("message %s picked up reactions %+v", v.msg.ID, got.Reactions)
		}
	}

	// Nothing was forwarded into the intruder's channel either.
	r := h.req(t, http.StatusOK, "GET", "/api/v1/channels/"+x+"/messages?limit=50", a.token, nil)
	var msgs []postedMessage
	decodeInto(t, r.Data, &msgs)
	for _, m := range msgs {
		if m.Content == victim.Content || m.Content == secret.Content {
			t.Errorf("a foreign message body was forwarded into channel X: %q", m.Content)
		}
	}
}

// TestDeactivationRevokesRefreshToken is the regression test for a ban that did
// not ban: Login checked is_active but the refresh path did not, so a
// deactivated account kept minting fresh access tokens off its 30-day refresh
// token until that token expired on its own.
func TestDeactivationRevokesRefreshToken(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	seed := h.firstWorkspace(t, admin)
	u := h.newUser(t, admin, seed, "deact")

	refreshOnce := func(want int, token string) string {
		t.Helper()
		code, r := h.do(t, "POST", "/api/v1/auth/refresh", "", map[string]string{"refresh_token": token})
		if code != want {
			t.Fatalf("refresh = %d, want %d (%v)", code, want, r.Error)
		}
		if want != http.StatusOK {
			return ""
		}
		var tp struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		}
		decodeInto(t, r.Data, &tp)
		if tp.AccessToken == "" || tp.RefreshToken == "" {
			t.Fatalf("refresh returned an incomplete token pair: %s", string(r.Data))
		}
		return tp.RefreshToken
	}

	// Baseline: the refresh token works, so the 401 below is the deactivation.
	live := refreshOnce(http.StatusOK, u.refresh)

	h.req(t, http.StatusOK, "PATCH", "/api/v1/admin/users/"+u.id, admin,
		map[string]any{"is_active": false})

	refreshOnce(http.StatusUnauthorized, live)
	h.denied(t, http.StatusUnauthorized, "POST", "/api/v1/auth/login", "",
		map[string]string{"email": u.email, "password": u.pass})

	// An admin may not lock themselves out.
	h.denied(t, http.StatusBadRequest, "PATCH",
		"/api/v1/admin/users/"+h.userID(t, admin), admin, map[string]any{"is_active": false})

	// Reactivating restores access, which proves the 401s were the flag and not
	// a token the test mangled.
	h.req(t, http.StatusOK, "PATCH", "/api/v1/admin/users/"+u.id, admin,
		map[string]any{"is_active": true})
	h.login(t, u.email, u.pass)
}

// TestWorkspaceRemovalRevokesAccess checks that eviction is real: the channel
// memberships go with the workspace membership, so a removed member loses
// every channel of that workspace rather than keeping the ones they had
// already joined.
func TestWorkspaceRemovalRevokesAccess(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	seed := h.firstWorkspace(t, admin)

	owner := h.newTenant(t, "rmvo")
	member := h.newUser(t, admin, seed, "rmvm")
	h.joinWorkspace(t, owner.token, owner.workspaceID, member)

	ch := h.createChannel(t, owner.token, owner.workspaceID, uniqueSlug("rmv"))
	h.req(t, http.StatusOK, "POST",
		"/api/v1/workspaces/"+owner.workspaceID+"/channels/"+ch+"/join", member.token, nil)

	// Baseline: the member really does have access before the removal.
	h.postMessage(t, owner.token, ch, "before removal")
	h.req(t, http.StatusOK, "GET", "/api/v1/channels/"+ch+"/messages", member.token, nil)
	theirs := h.postMessage(t, member.token, ch, "member can post")
	h.req(t, http.StatusOK, "GET", "/api/v1/workspaces/"+owner.workspaceID+"/channels/browse", member.token, nil)

	r := h.req(t, http.StatusOK, "DELETE",
		"/api/v1/workspaces/"+owner.workspaceID+"/members/"+member.id, owner.token, nil)
	var removal struct {
		ChannelsRemoved int      `json:"channels_removed"`
		ChannelIDs      []string `json:"removed_channel_ids"`
	}
	decodeInto(t, r.Data, &removal)
	if removal.ChannelsRemoved < 1 {
		t.Errorf("removal reported %d channels, want at least the one they joined", removal.ChannelsRemoved)
	}

	// Everything they could reach a moment ago is now refused.
	h.denied(t, http.StatusForbidden, "GET", "/api/v1/channels/"+ch+"/messages", member.token, nil)
	h.denied(t, http.StatusForbidden, "POST", "/api/v1/channels/"+ch+"/messages", member.token,
		map[string]string{"content": "still here?"})
	h.denied(t, http.StatusForbidden, "GET", "/api/v1/channels/"+ch+"/unread", member.token, nil)
	h.denied(t, http.StatusForbidden, "GET",
		"/api/v1/workspaces/"+owner.workspaceID+"/channels/browse", member.token, nil)
	h.denied(t, http.StatusNotFound, "GET", "/api/v1/workspaces/"+owner.workspaceID, member.token, nil)
	h.denied(t, http.StatusForbidden, "POST",
		"/api/v1/workspaces/"+owner.workspaceID+"/channels/"+ch+"/join", member.token, nil)

	// Editing their own message is refused too: authorship is not membership.
	h.denied(t, http.StatusForbidden, "PATCH", "/api/v1/channels/"+ch+"/messages/"+theirs.ID,
		member.token, map[string]string{"content": "sneaky edit"})

	// The account itself still works; only this workspace is gone.
	h.req(t, http.StatusOK, "GET", "/api/v1/users/me", member.token, nil)

	// And the history they wrote is still there for whoever remains.
	got := h.getMessage(t, owner.token, ch, theirs.ID)
	if got.Content != "member can post" {
		t.Errorf("removed member's message = %q, want it intact", got.Content)
	}
}
