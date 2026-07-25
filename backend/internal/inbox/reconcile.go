package inbox

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// reconcileSliceSize is how many users one tick recomputes. The job rotates
// through the whole population a slice at a time rather than recomputing
// everything on every tick, for the same reason cmd/worker's object GC sweeps one
// hex prefix per run: a job whose cost grows with the install is a job that is
// eventually turned off.
const reconcileSliceSize = 500

// reconcileSamples bounds how many concrete disagreements one report names.
// Enough to debug from a log line, not enough to fill a disk when something has
// gone systematically wrong.
const reconcileSamples = 20

// Reconciler is the body of the inbox_reconcile worker job.
//
// It exists for the same reason internal/authz has a drift job, and the plan
// says so explicitly: inbox_items is denormalized state that answers a question
// the user asked out loud. The badge is produced by an at-least-once pipeline
// with three subsystems each owning part of "unread", and a silent divergence
// here is not a stale cache — it is a wrong answer. A user who reads #alerts and
// still sees "1" on the bell will not file a bug; they will stop trusting the
// bell, after which every pillar that files into it is wasted work.
//
// Like runACLDrift it REPAIRS as well as reports, and says exactly what it
// changed, because a watchdog that is permanently red is a watchdog nobody
// reads. Unlike runACLDrift there is no expected-state view to compare against:
// the definition is "count the events", which is the query itself.
type Reconciler struct {
	repo   *Repository
	logger *slog.Logger

	mu     sync.Mutex
	cursor string // last user id of the previous slice; "" restarts the rotation
}

func NewReconciler(repo *Repository, logger *slog.Logger) *Reconciler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Reconciler{repo: repo, logger: logger}
}

// Run reconciles one slice of users and reports what it repaired.
//
// The return value is an error when drift was found, so runLoop records it in
// /health's last_error for the job — the same treatment acl_drift gets. It
// deliberately does not fail the readiness probe: restarting the worker repairs
// nothing here, and a counter that was wrong for an hour is not an outage.
func (r *Reconciler) Run(ctx context.Context) error {
	r.mu.Lock()
	from := r.cursor
	r.mu.Unlock()

	users, err := r.repo.UserSliceAfter(ctx, from, reconcileSliceSize)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		// End of the rotation (or an empty table). Start again from the top next
		// tick rather than idling forever on a cursor past the last user.
		r.mu.Lock()
		r.cursor = ""
		r.mu.Unlock()
		return nil
	}

	r.mu.Lock()
	r.cursor = users[len(users)-1]
	r.mu.Unlock()

	drifts, repaired, err := r.repo.ReconcileUsers(ctx, users, reconcileSamples)
	if err != nil {
		return err
	}
	if repaired == 0 {
		r.logger.Debug("inbox reconcile clean", "users", len(users))
		return nil
	}

	r.logger.Error("inbox counters had drifted and were repaired",
		"users_scanned", len(users), "items_repaired", repaired, "samples", len(drifts))
	for _, d := range drifts {
		r.logger.Error("inbox drift sample",
			"item_id", d.ItemID, "user_id", d.UserID,
			"stored_event_count", d.StoredCount, "actual_event_count", d.ActualCount,
			"orphan", d.Orphan)
	}
	return fmt.Errorf("inbox: repaired %d item(s) whose counters disagreed with inbox_events", repaired)
}
