//go:build integration

package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wick-Lim/SuperOps/backend/internal/audit"
	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/pkg/logger"
)

type auditRow struct {
	ID           string  `json:"id"`
	WorkspaceID  *string `json:"workspace_id"`
	ActorID      *string `json:"actor_id"`
	Action       string  `json:"action"`
	ResourceType string  `json:"resource_type"`
	ResourceID   *string `json:"resource_id"`
	Metadata     string  `json:"metadata"`
	EventCount   int     `json:"event_count"`
	ChainSeq     *int64  `json:"chain_seq"`
}

func (h *harness) auditLogs(t *testing.T, token, query string) []auditRow {
	t.Helper()
	r := h.req(t, http.StatusOK, "GET", "/api/v1/admin/audit-logs"+query, token, nil)
	var rows []auditRow
	decodeInto(t, r.Data, &rows)
	return rows
}

// The regression for the live bug: internal/admin/mail.go passed the transport
// name into resource_id, which was a UUID column, so the INSERT failed with
// 22P02 — and the error was discarded by `_ = h.audit.Log(...)`, so mail.test_sent
// had never once been recorded. Migration 021 widens the column and the Go side
// puts the transport in metadata; this asserts the row actually exists now.
func TestAuditMailTestIsRecorded(t *testing.T) {
	h := getHarness(t)
	a := h.newTenant(t, "audmail")

	h.req(t, http.StatusOK, "POST", "/api/v1/admin/mail/test", a.token,
		map[string]string{"email": a.email})

	rows := h.auditLogs(t, a.token, "?action=mail.test_sent")
	if len(rows) == 0 {
		t.Fatal("mail.test_sent was not recorded — the bug migration 021 exists to fix is back")
	}
	if rows[0].Metadata == "" || rows[0].Metadata == "{}" {
		t.Errorf("mail.test_sent carries no metadata; the transport belongs there: %+v", rows[0])
	}
}

// `action=` is a PREFIX match when it ends in a dot, which the
// (workspace_id, action, created_at DESC) index serves as a range scan. An exact
// value is an equality match. Both matter: an auditor asks "everything this
// person did to authentication" far more often than one specific verb.
func TestAuditFiltersNarrowRatherThanWiden(t *testing.T) {
	h := getHarness(t)
	a := h.newTenant(t, "audfilt")

	// Two different actions in this tenant.
	h.invite(t, a.token, a.workspaceID, uniqueSlug("audf")+"@demo.local", "member")
	h.req(t, http.StatusOK, "POST", "/api/v1/admin/mail/test", a.token,
		map[string]string{"email": a.email})

	all := h.auditLogs(t, a.token, "")
	if len(all) < 2 {
		t.Fatalf("%d audit rows in a tenant that just did two auditable things", len(all))
	}

	exact := h.auditLogs(t, a.token, "?action=mail.test_sent")
	for _, r := range exact {
		if r.Action != "mail.test_sent" {
			t.Errorf("action=mail.test_sent returned %q", r.Action)
		}
	}
	if len(exact) >= len(all) {
		t.Errorf("an exact action filter returned %d of %d rows; it did not narrow anything",
			len(exact), len(all))
	}

	prefix := h.auditLogs(t, a.token, "?action=mail.")
	if len(prefix) == 0 {
		t.Error("a prefix filter ('mail.') matched nothing")
	}
	for _, r := range prefix {
		if len(r.Action) < 5 || r.Action[:5] != "mail." {
			t.Errorf("action=mail. returned %q", r.Action)
		}
	}

	// A bad time bound is a 400, not a silently ignored parameter.
	h.denied(t, http.StatusBadRequest, "GET", "/api/v1/admin/audit-logs?from=yesterday", a.token, nil)
	h.denied(t, http.StatusBadRequest, "GET", "/api/v1/admin/audit-logs?actor_id=not-a-uuid", a.token, nil)
}

// Reading the audit log is itself audited, with the filter recorded. That is the
// row that catches an administrator going looking, and it is the one audit event
// whose absence would be most convenient for the person causing it.
//
// It is buffered (Tier 2) and coalesced, so this drives the write through and
// then waits for the buffer rather than asserting synchronously.
func TestAuditReadIsItselfAudited(t *testing.T) {
	h := getHarness(t)
	a := h.newTenant(t, "audread")

	// Several reads with the same filter inside one hour coalesce into one row
	// with a count: an admin paging through a week of logs is one investigation,
	// not forty events.
	for range 5 {
		h.auditLogs(t, a.token, "?action=user.login")
	}

	deadline := time.Now().Add(10 * time.Second)
	var found *auditRow
	for time.Now().Before(deadline) {
		for _, r := range h.auditLogs(t, a.token, "?action=audit.read") {
			row := r
			found = &row
			break
		}
		if found != nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if found == nil {
		t.Fatal("audit.read was never recorded; nothing catches an administrator reading the log")
	}
	if found.Metadata == "" || found.Metadata == "{}" {
		t.Errorf("audit.read recorded no filter: %+v", found)
	}
	// A coalesced row is unchained by construction — a hash over a row that is
	// mutated on every repeat would go stale on the second event.
	if found.ChainSeq != nil {
		t.Errorf("audit.read carries chain_seq %d; a coalescable row must be unchained", *found.ChainSeq)
	}
}

// Authorization changes are audited from INSIDE authz.Grant/Revoke, not at the
// call sites, so a pillar cannot forget a hook it never had to write.
func TestAuthorizationChangesAreAudited(t *testing.T) {
	h := getHarness(t)
	a := h.newTenant(t, "audacl")
	member := h.newUser(t, a.token, a.workspaceID, "audaclm")
	channelID := h.createChannel(t, a.token, a.workspaceID, uniqueSlug("audacl"))

	az := h.app.Authz
	ctx := context.Background()
	// Channels are a DERIVED type: their acl_object row comes from the
	// reconcile pass, not from the create handler (see cmd/worker's
	// runACLDrift). A grant needs the object registered, so run the same
	// rebuild the worker runs hourly.
	if _, err := az.Rebuild(ctx); err != nil {
		t.Fatalf("rebuild acl materialization: %v", err)
	}
	if err := az.Grant(ctx, authz.UserSubject(a.id), authz.UserSubject(member.id), authz.ChannelObject(channelID), authz.CapRead); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := az.Revoke(ctx, authz.UserSubject(a.id), authz.UserSubject(member.id), authz.ChannelObject(channelID)); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	rows := h.auditLogs(t, a.token, "?action=acl.")
	var granted, revoked bool
	for _, r := range rows {
		switch r.Action {
		case "acl.granted":
			granted = true
		case "acl.revoked":
			revoked = true
		}
		if r.WorkspaceID == nil || *r.WorkspaceID != a.workspaceID {
			t.Errorf("acl audit row is attributed to workspace %v, want %s", r.WorkspaceID, a.workspaceID)
		}
		if r.ChainSeq == nil {
			t.Errorf("%s is not chained; authorization changes are Tier 1 and must be", r.Action)
		}
	}
	if !granted || !revoked {
		t.Fatalf("acl.granted=%v acl.revoked=%v — the hooks inside authz did not fire", granted, revoked)
	}
}

// The chain is only half the answer; the anchor is the half that matters,
// because it is the only layer the local administrator does not control. With
// the default 'log' sink, anchoring has to actually advance the watermark.
func TestAuditSinkAnchors(t *testing.T) {
	h := getHarness(t)
	a := h.newTenant(t, "audanch")

	// Produce a chained entry.
	h.invite(t, a.token, a.workspaceID, uniqueSlug("audanch")+"@demo.local", "member")

	sink, err := audit.NewSink(audit.SinkConfig{Transport: audit.SinkLog, Logger: logger.New("error")})
	if err != nil {
		t.Fatalf("build log sink: %v", err)
	}
	v := audit.NewVerifier(h.app.DB, sink, logger.New("error"))
	if _, err := v.Anchor(context.Background()); err != nil {
		t.Fatalf("anchor: %v", err)
	}

	var head, anchored int64
	if err := h.app.DB.QueryRow(context.Background(),
		`SELECT head_seq, anchored_seq FROM audit_chain_heads WHERE workspace_id = $1`,
		a.workspaceID).Scan(&head, &anchored); err != nil {
		t.Fatalf("read chain head: %v", err)
	}
	if head == 0 {
		t.Fatal("nothing was chained for this workspace")
	}
	if anchored != head {
		t.Fatalf("anchored_seq = %d, head_seq = %d — the anchor did not advance", anchored, head)
	}

	// And the admin surface reports both numbers, so nobody has to guess which
	// part of the log is protected by something off-box.
	var verify struct {
		OK     bool   `json:"ok"`
		Sink   string `json:"sink"`
		Chains []struct {
			WorkspaceID string `json:"workspace_id"`
			HeadSeq     int64  `json:"head_seq"`
			AnchoredSeq int64  `json:"anchored_seq"`
		} `json:"chains"`
	}
	decodeInto(t, h.req(t, http.StatusOK, "GET", "/api/v1/admin/audit-logs/verify", a.token, nil).Data, &verify)
	if !verify.OK || verify.Sink == "" {
		t.Fatalf("verify = %+v", verify)
	}
	for _, c := range verify.Chains {
		if c.WorkspaceID == a.workspaceID && c.AnchoredSeq != c.HeadSeq {
			t.Errorf("verify reports anchored %d of %d", c.AnchoredSeq, c.HeadSeq)
		}
	}
}

// The sink test ships a REAL anchor and reports the transport's REAL error —
// same shape as the mail configuration test, and for the same reason: a
// diagnostic that hides the reason is not a diagnostic.
func TestAuditSinkTestRoute(t *testing.T) {
	h := getHarness(t)
	a := h.newTenant(t, "audsink")

	r := h.req(t, http.StatusOK, "POST", "/api/v1/admin/audit-sink/test", a.token, nil)
	var out struct {
		Shipped   bool   `json:"shipped"`
		Transport string `json:"transport"`
	}
	decodeInto(t, r.Data, &out)
	if !out.Shipped || out.Transport == "" {
		t.Fatalf("sink test = %+v", out)
	}

	// It is admin-only, like everything else on /admin.
	member := h.newUser(t, a.token, a.workspaceID, "audsinkm")
	h.denied(t, http.StatusForbidden, "POST", "/api/v1/admin/audit-sink/test", member.token, nil)
}

// There is deliberately NO route that mutates audit_logs, and there must never
// be one: disabling auditing is startup configuration, so turning it off lands
// in the deploy trail instead of in the product.
func TestAuditHasNoMutationSurface(t *testing.T) {
	h := getHarness(t)
	a := h.newTenant(t, "audmut")

	rows := h.auditLogs(t, a.token, "")
	id := "00000000-0000-0000-0000-000000000000"
	if len(rows) > 0 {
		id = rows[0].ID
	}

	for _, method := range []string{http.MethodDelete, http.MethodPut, http.MethodPatch, http.MethodPost} {
		code, _ := h.do(t, method, "/api/v1/admin/audit-logs/"+id, a.token, map[string]any{"action": "x"})
		if code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/v1/admin/audit-logs/{id} = %d; there must be no mutation surface", method, code)
		}
	}
}
