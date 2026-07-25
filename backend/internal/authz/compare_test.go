package authz

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
)

// The dual-run comparison is the mechanism that makes step 4 of
// docs/plans/00-permissions.md safe, so it has to be tested in both directions:
// silent when the two models agree, and loud — with enough context to debug —
// when they do not. A comparison that cannot report is worse than none, because
// it produces confidence rather than evidence.

// comparingChecker returns a checker with dual-run enabled and a collecting
// sink. The logger discards: the assertions are on the sink, and the test
// output should not be buried under the mismatches a test deliberately causes.
func comparingChecker(t *testing.T) (*Checker, func() []Mismatch) {
	t.Helper()

	var mu sync.Mutex
	var seen []Mismatch

	c, _ := seed(t)
	cc := New(c.pool, WithComparisonSink(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		func(m Mismatch) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, m)
		},
	))
	return cc, func() []Mismatch {
		mu.Lock()
		defer mu.Unlock()
		return append([]Mismatch(nil), seen...)
	}
}

// TestDualRunIsSilentOnTheFixtureWorld drives every compared method over the
// whole user x channel matrix. Any disagreement here is a difference between
// what the product enforces today and what the object-level model would enforce
// after a cutover — which is precisely the thing that must be zero before a
// cutover is allowed to start.
func TestDualRunIsSilentOnTheFixtureWorld(t *testing.T) {
	_, w := rebuilt(t)
	cc, mismatches := comparingChecker(t)
	ctx := context.Background()

	users := []string{w.owner, w.admin, w.member, w.guest, w.stranger, w.multiAdmin, missingID}
	channels := []string{
		w.chPublic, w.chPublicEmpty, w.chPrivate, w.chDM, w.chArchived, w.chOtherTenant, missingID,
	}
	workspaces := []string{w.wsA, w.wsB, missingID}

	for _, user := range users {
		for _, ws := range workspaces {
			if _, err := cc.IsWorkspaceMember(ctx, ws, user); err != nil {
				t.Fatalf("IsWorkspaceMember: %v", err)
			}
			if _, err := cc.IsWorkspaceAdmin(ctx, ws, user); err != nil {
				t.Fatalf("IsWorkspaceAdmin: %v", err)
			}
			if _, err := cc.ReadableChannelIDs(ctx, ws, user); err != nil {
				t.Fatalf("ReadableChannelIDs: %v", err)
			}
		}
		for _, ch := range channels {
			info, err := cc.Channel(ctx, ch)
			if err != nil {
				t.Fatalf("Channel: %v", err)
			}
			if _, err := cc.CanReadChannel(ctx, info, user); err != nil {
				t.Fatalf("CanReadChannel: %v", err)
			}
			if _, err := cc.IsChannelMember(ctx, ch, user); err != nil {
				t.Fatalf("IsChannelMember: %v", err)
			}
			if _, err := cc.IsChannelAdmin(ctx, ch, user); err != nil {
				t.Fatalf("IsChannelAdmin: %v", err)
			}
		}
	}

	checks, mismatched, evalErrors := cc.ComparisonStats()
	if checks == 0 {
		t.Fatal("no comparisons ran; the dual-run mode is not actually wired to the legacy methods")
	}
	if evalErrors != 0 {
		t.Errorf("%d comparisons could not be evaluated", evalErrors)
	}
	if mismatched != 0 {
		for _, m := range mismatches() {
			t.Errorf("dual-run mismatch: %s", m)
		}
		t.Fatalf("%d of %d comparisons disagreed", mismatched, checks)
	}
}

// TestDualRunReportsADisagreementWithContext injects the disagreement the model
// is designed to have — an explicit grant that the legacy predicate knows
// nothing about — and asserts the report names everything needed to reproduce
// it from a log line.
func TestDualRunReportsADisagreementWithContext(t *testing.T) {
	_, w := rebuilt(t)
	cc, mismatches := comparingChecker(t)
	ctx := context.Background()
	actor := UserSubject(w.owner)

	// The guest is a workspace member and not a member of the private channel,
	// so CanReadChannel says no. An explicit grant says yes. Until the cutover
	// the product acts on the first answer, and this is exactly the class of
	// divergence the comparison has to surface before it acts on the second.
	if err := cc.Grant(ctx, actor, UserSubject(w.guest), ChannelObject(w.chPrivate), CapRead); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	t.Cleanup(func() {
		if err := cc.Revoke(context.Background(), actor, UserSubject(w.guest), ChannelObject(w.chPrivate)); err != nil {
			t.Errorf("cleanup Revoke: %v", err)
		}
	})

	info, err := cc.Channel(ctx, w.chPrivate)
	if err != nil {
		t.Fatalf("Channel: %v", err)
	}
	ok, err := cc.CanReadChannel(ctx, info, w.guest)
	if err != nil {
		t.Fatalf("CanReadChannel: %v", err)
	}

	// The whole point: the comparison observes, it does not decide. The answer
	// the caller acts on is the legacy one, unchanged.
	if ok {
		t.Fatal("CanReadChannel changed its answer; the comparison must never alter a decision")
	}

	found := mismatches()
	if len(found) == 0 {
		t.Fatal("the injected disagreement was not reported")
	}
	m := found[0]
	switch {
	case m.Method != "CanReadChannel":
		t.Errorf("Method = %q, want CanReadChannel", m.Method)
	case m.Subject.ID != w.guest:
		t.Errorf("Subject = %s, want the guest", m.Subject)
	case m.Object != ChannelObject(w.chPrivate):
		t.Errorf("Object = %s, want the private channel", m.Object)
	case m.Legacy:
		t.Error("Legacy answer recorded as true, want false")
	case m.Got != CapRead || m.Want != CapRead:
		t.Errorf("capabilities = got %s / want %s, expected read / read", m.Got, m.Want)
	}

	// And the structured log fields carry the same thing, because that is what
	// anybody debugging this will actually be reading.
	attrs := m.logAttrs()
	if len(attrs)%2 != 0 {
		t.Fatalf("logAttrs is not a key/value sequence: %v", attrs)
	}
	for _, want := range []string{"method", "subject_id", "object_type", "object_id", "legacy_answer", "new_capability", "required_capability"} {
		if !containsAttr(attrs, want) {
			t.Errorf("logAttrs is missing %q: %v", want, attrs)
		}
	}
}

// TestComparisonIsOffByDefault: it costs two to three extra queries per
// authorization check, which is fine for a test run and is not fine for a
// request path.
func TestComparisonIsOffByDefault(t *testing.T) {
	c, _ := seed(t)
	if c.ComparisonEnabled() {
		t.Error("a plain New(pool) enabled dual-run comparison")
	}
	if checks, mismatches, evalErrors := c.ComparisonStats(); checks|mismatches|evalErrors != 0 {
		t.Errorf("a disabled comparison reported stats: %d/%d/%d", checks, mismatches, evalErrors)
	}
}

func containsAttr(attrs []any, key string) bool {
	for i := 0; i < len(attrs); i += 2 {
		if s, ok := attrs[i].(string); ok && s == key {
			return true
		}
	}
	return false
}
