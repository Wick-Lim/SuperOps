//go:build integration

// The object-level permission model, enforced end to end
// (docs/plans/00-permissions.md, steps 4 and 5).
//
// This file used to assert that a dual-run comparison was silent: every legacy
// membership method also evaluated the object-level checker, and any
// disagreement was counted and failed the suite. That comparison is gone,
// because the thing it compared against is gone — the membership methods were
// deleted in step 5, and a comparison with one model left in it is not a quiet
// comparison, it is no comparison. Reporting "0 mismatches" out of an empty
// comparison would be the most misleading number this suite could produce, so
// the counter was removed rather than left to read zero.
//
// What survives is the part that was always the real assertion: the matrix of
// who may reach what, driven through the fully-wired app. It is deliberately
// the three positions where the two models could ever have differed — a
// channel member, a workspace member who is not in the channel, and another
// tenant entirely.
//
// The permanent equivalence proof — whatever the pre-ACL predicates answered,
// Capability answers the same, for every pair in the fixture graph — lives in
// internal/authz's own tests, which run on every `go test` with no flag.
package integration

import (
	"net/http"
	"testing"
)

func TestObjectPermissionsAreEnforcedEndToEnd(t *testing.T) {
	h := getHarness(t)

	admin := h.adminToken(t)
	seed := h.firstWorkspace(t, admin)

	a := h.newTenant(t, "objperma")
	b := h.newTenant(t, "objpermb")
	outsider := h.newUser(t, admin, seed, "objpermc")
	h.joinWorkspace(t, b.token, b.workspaceID, outsider)

	pub := h.createChannel(t, b.token, b.workspaceID, uniqueSlug("objpermpub"))
	priv := h.createTypedChannel(t, b.token, b.workspaceID, uniqueSlug("objpermpriv"), "private")
	h.postMessage(t, b.token, pub, "public")
	h.postMessage(t, b.token, priv, "private")

	inB := "/api/v1/workspaces/" + b.workspaceID

	// Allowed: the owner reads both. The workspace member can browse, and can
	// join the public channel and then read it — reading a channel's messages
	// requires membership here (CapWrite on the channel object), and joining a
	// public one is how membership is obtained.
	h.req(t, http.StatusOK, "GET", "/api/v1/channels/"+priv+"/messages", b.token, nil)
	h.req(t, http.StatusOK, "GET", inB+"/channels/browse", outsider.token, nil)
	h.req(t, http.StatusOK, "POST", inB+"/channels/"+pub+"/join", outsider.token, nil)
	h.req(t, http.StatusOK, "GET", "/api/v1/channels/"+pub+"/messages", outsider.token, nil)

	// Refused: the private channel to a workspace member who is not in it, and
	// everything to the other tenant.
	h.denied(t, http.StatusForbidden, "GET", "/api/v1/channels/"+priv+"/messages", outsider.token, nil)
	h.denied(t, http.StatusForbidden, "GET", "/api/v1/channels/"+pub+"/messages", a.token, nil)
	h.denied(t, http.StatusForbidden, "GET", inB+"/channels/browse", a.token, nil)
	h.denied(t, http.StatusNotFound, "GET", inB, a.token, nil)
}

// TestCapabilityLadderSeparatesReadFromWrite pins the rung the pre-ACL model
// could not express, and the one a conversion is most likely to get wrong: a
// public channel is READABLE by every member of its workspace and WRITABLE only
// by the people who joined it.
//
// Converting a membership check to CapRead instead of CapWrite would silently
// let any workspace member post in, mute and mark read every public channel
// they never joined — and no tenancy test would catch it, because nothing
// crosses a tenant boundary.
func TestCapabilityLadderSeparatesReadFromWrite(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	seed := h.firstWorkspace(t, admin)

	owner := h.newTenant(t, "laddero")
	mate := h.newUser(t, admin, seed, "ladderm")
	h.joinWorkspace(t, owner.token, owner.workspaceID, mate)

	pub := h.createChannel(t, owner.token, owner.workspaceID, uniqueSlug("ladderpub"))
	inWS := "/api/v1/workspaces/" + owner.workspaceID

	// Read: a workspace member who never joined can see the channel and its
	// roster.
	h.req(t, http.StatusOK, "GET", inWS+"/channels/"+pub, mate.token, nil)
	h.req(t, http.StatusOK, "GET", inWS+"/channels/"+pub+"/members", mate.token, nil)

	// Write: the same caller may not post, may not move their read marker, and
	// may not set per-channel preferences — all of which need a membership row.
	h.denied(t, http.StatusForbidden, "POST", "/api/v1/channels/"+pub+"/messages", mate.token,
		map[string]string{"content": "not a member"})
	h.denied(t, http.StatusForbidden, "PUT", "/api/v1/channels/"+pub+"/read", mate.token, nil)
	h.denied(t, http.StatusForbidden, "GET", "/api/v1/channels/"+pub+"/unread", mate.token, nil)
	h.denied(t, http.StatusForbidden, "PATCH", "/api/v1/channels/"+pub+"/prefs", mate.token,
		map[string]any{"muted": true})

	// Joining supplies the missing rung, and nothing else has to change.
	h.req(t, http.StatusOK, "POST", inWS+"/channels/"+pub+"/join", mate.token, nil)
	h.postMessage(t, mate.token, pub, "now a member")
	h.req(t, http.StatusOK, "PUT", "/api/v1/channels/"+pub+"/read", mate.token, nil)
}
