//go:build integration

// Dual-run verification for object-level permissions
// (docs/plans/00-permissions.md, step 3).
//
// The suite runs with AUTHZ_DUAL_RUN on (see buildConfig), so EVERY
// authorization decision every other test in this package causes is also
// evaluated against the object-level checker, and any disagreement is counted.
// This file drives one deliberately awkward scenario of its own and then
// asserts the counter is still zero.
//
// Why this test and not a unit test: internal/authz's own tests assert the two
// models agree on a fixture graph somebody wrote to be interesting. This one
// asserts they agree on the traffic the real handlers actually generate —
// including the cross-tenant probes in tenancy_test.go, which are the exact
// requests where a disagreement would matter most. Silence here is the
// precondition for step 4 (converting handlers to the new checker); until it
// holds, no package gets cut over.
package integration

import (
	"net/http"
	"testing"
)

func TestAuthzDualRunAgreesWithTheLegacyChecker(t *testing.T) {
	h := getHarness(t)
	if !h.app.Authz.ComparisonEnabled() {
		t.Fatal("the suite is not running with dual-run comparison enabled; buildConfig sets cfg.Authz.DualRun")
	}

	admin := h.adminToken(t)
	seed := h.firstWorkspace(t, admin)

	// Two tenants that share nothing, plus a third party who belongs to one
	// workspace but not to its private channel — the three positions where the
	// legacy predicate and the object-level model could differ.
	a := h.newTenant(t, "dualrna")
	b := h.newTenant(t, "dualrnb")
	outsider := h.newUser(t, admin, seed, "dualrnc")
	h.joinWorkspace(t, b.token, b.workspaceID, outsider)

	pub := h.createChannel(t, b.token, b.workspaceID, uniqueSlug("dualrnpub"))
	priv := h.createTypedChannel(t, b.token, b.workspaceID, uniqueSlug("dualrnpriv"), "private")
	h.postMessage(t, b.token, pub, "public")
	h.postMessage(t, b.token, priv, "private")

	inB := "/api/v1/workspaces/" + b.workspaceID

	// Allowed: the owner reads both. The workspace member can browse, and can
	// join the public channel and then read it — reading a channel's messages
	// requires membership here, and joining a public one is how membership is
	// obtained.
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

	checks, mismatches, evalErrors := h.app.Authz.ComparisonStats()
	if checks == 0 {
		t.Fatal("no comparisons ran at all; the dual-run mode is not wired to the membership methods")
	}
	t.Logf("dual-run so far: %d comparisons, %d mismatches, %d evaluation errors", checks, mismatches, evalErrors)

	// The counters are process-wide, so what this test can assert is "no
	// disagreement up to this point". The whole-suite assertion — the one that
	// covers the tenancy regressions regardless of the order Go runs them in —
	// is in TestMain.
	if mismatches != 0 {
		t.Errorf("the object-level checker disagreed with the legacy checker %d times out of %d; "+
			"grep the test log for \"authz dual-run mismatch\"", mismatches, checks)
	}
	if evalErrors != 0 {
		// Not a disagreement, but not nothing: the new checker could not answer,
		// and after the cutover that would have been a 500.
		t.Errorf("%d comparisons could not be evaluated; grep for \"capability evaluation failed\"", evalErrors)
	}
}
