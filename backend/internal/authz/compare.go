package authz

// Dual-run comparison — step 3 of docs/plans/00-permissions.md, and the reason
// step 4 is allowed to happen at all.
//
// The fifteen membership methods keep answering every authorization question in
// the product. When comparison is enabled, each of the ones that has an
// object-level equivalent ALSO evaluates Capability and reports a disagreement.
// Nothing about the answer the caller receives changes: compare cannot alter a
// return value, cannot fail a request, and cannot turn a database hiccup into a
// denial. If it could, the safety mechanism would itself be a way to break
// authorization, which is the opposite of the point.
//
// A comparison that logs "mismatch" and nothing else is worthless — by the time
// anybody reads it the request is gone. So every report carries the subject,
// the object, both answers, the capability that was required, and the call site
// that asked. That is the minimum needed to reproduce one from a log line.
//
// Off by default (AUTHZ_DUAL_RUN), on in the integration suite. It costs two to
// three extra queries per authorization check, which is fine for a test run and
// is not fine for a request path — hence the default.

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
)

// Mismatch is one disagreement between a legacy method and the object-level
// checker.
type Mismatch struct {
	// Method is the legacy method that was called, e.g. "IsChannelMember".
	Method string
	// Subject and Object are what was asked about.
	Subject SubjectRef
	Object  ObjectRef
	// Legacy is the answer the product actually acted on.
	Legacy bool
	// Want is the capability the object-level equivalent of Method requires;
	// Got is the capability the checker computed.
	Want Capability
	Got  Capability
	// Detail carries set differences for the list comparisons, where "true vs
	// false" is not a useful description of what went wrong.
	Detail string
}

func (m Mismatch) String() string {
	s := fmt.Sprintf("%s(%s, %s): legacy=%t new=%s (needs %s)",
		m.Method, m.Subject, m.Object, m.Legacy, m.Got, m.Want)
	if m.Detail != "" {
		s += ": " + m.Detail
	}
	return s
}

// logAttrs renders a mismatch as structured fields. Flat and greppable on
// purpose: the first thing anybody does with one of these is grep for the
// object id.
func (m Mismatch) logAttrs() []any {
	attrs := []any{
		"method", m.Method,
		"subject_type", m.Subject.Type,
		"subject_id", m.Subject.ID,
		"object_type", m.Object.Type,
		"object_id", m.Object.ID,
		"legacy_answer", m.Legacy,
		"new_capability", m.Got.String(),
		"required_capability", m.Want.String(),
	}
	if m.Detail != "" {
		attrs = append(attrs, "detail", m.Detail)
	}
	return attrs
}

// comparison holds the dual-run state. It is a pointer field on Checker so the
// zero Checker (comparison disabled) allocates nothing.
type comparison struct {
	logger *slog.Logger
	// sink receives every mismatch in addition to the log. Tests use it to
	// assert on disagreements instead of parsing log output; a deployment can
	// use it to push a metric.
	sink func(Mismatch)

	checks    atomic.Int64
	mismatch  atomic.Int64
	evalError atomic.Int64
}

// ComparisonEnabled reports whether dual-run comparison is active.
func (c *Checker) ComparisonEnabled() bool { return c.cmp != nil }

// ComparisonStats returns (checks, mismatches, evaluation errors) since start.
// Zero mismatches over a representative workload is the precondition for
// cutting a package over to the object-level checker.
func (c *Checker) ComparisonStats() (checks, mismatches, evalErrors int64) {
	if c.cmp == nil {
		return 0, 0, 0
	}
	return c.cmp.checks.Load(), c.cmp.mismatch.Load(), c.cmp.evalError.Load()
}

// compare evaluates the object-level equivalent of a legacy answer and reports
// a disagreement. It is a no-op when comparison is disabled, and it swallows
// its own errors: this path exists to observe the checker, not to let the
// checker break the product.
func (c *Checker) compare(ctx context.Context, method string, subject SubjectRef, obj ObjectRef, legacy bool, want Capability) {
	if c.cmp == nil {
		return
	}
	c.cmp.checks.Add(1)

	got, err := c.Capability(ctx, subject, obj)
	if err != nil {
		// A failed evaluation is not a mismatch — reporting it as one would
		// bury the real disagreements under every context cancellation.
		c.cmp.evalError.Add(1)
		c.cmp.logger.Warn("authz dual-run: capability evaluation failed",
			"method", method, "subject_id", subject.ID,
			"object_type", obj.Type, "object_id", obj.ID, "error", err)
		return
	}
	if got.Implies(want) == legacy {
		return
	}
	c.report(Mismatch{
		Method: method, Subject: subject, Object: obj,
		Legacy: legacy, Want: want, Got: got,
	})
}

// compareSets reports a disagreement between two id lists — ReadableChannelIDs
// against the channel keys KeysFor derives. A boolean comparison cannot express
// this one, and "the sets differ" without naming the ids is not debuggable.
func (c *Checker) compareSets(ctx context.Context, method string, subject SubjectRef, obj ObjectRef, legacy []string) {
	if c.cmp == nil {
		return
	}
	c.cmp.checks.Add(1)

	keys, err := c.KeysFor(ctx, subject, obj.ID)
	if err != nil {
		c.cmp.evalError.Add(1)
		c.cmp.logger.Warn("authz dual-run: key resolution failed",
			"method", method, "subject_id", subject.ID, "workspace_id", obj.ID, "error", err)
		return
	}

	// Compare on keys, not on ids: the key is what a search filter is actually
	// built from, so a difference here is a difference in what the caller can
	// find.
	want := map[string]bool{}
	for _, id := range legacy {
		if k := ContainerKey(ChannelObject(id)); k != "" {
			want[k] = true
		}
	}
	got := map[string]bool{}
	for _, k := range keys {
		if len(k) > 2 && k[:2] == "c-" {
			got[k] = true
		}
	}

	var onlyLegacy, onlyNew []string
	for k := range want {
		if !got[k] {
			onlyLegacy = append(onlyLegacy, k)
		}
	}
	for k := range got {
		if !want[k] {
			onlyNew = append(onlyNew, k)
		}
	}
	if len(onlyLegacy) == 0 && len(onlyNew) == 0 {
		return
	}

	c.report(Mismatch{
		Method: method, Subject: subject, Object: obj,
		Legacy: true, Want: CapRead, Got: CapNone,
		Detail: fmt.Sprintf("legacy-only=[%s] new-only=[%s]",
			summarize(dedupeSorted(onlyLegacy), 10), summarize(dedupeSorted(onlyNew), 10)),
	})
}

// report records and logs one mismatch. Error level, not warn: a disagreement
// here means the object-level model and the model the product enforces have
// diverged, and cutting a package over on top of that would be the second time
// multi-tenancy broke in this repository.
func (c *Checker) report(m Mismatch) {
	c.cmp.mismatch.Add(1)
	c.cmp.logger.Error("authz dual-run mismatch", m.logAttrs()...)
	if c.cmp.sink != nil {
		c.cmp.sink(m)
	}
}
