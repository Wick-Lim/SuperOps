package quota

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func mustQ(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// workspace returns a fresh workspace with the given quota (0 = unlimited) and
// NO workspace_storage row, which is the state a workspace created after
// migration 026 is actually in.
func workspace(t *testing.T, pool *pgxpool.Pool, quotaBytes int64) (wsID, userID string) {
	t.Helper()
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	mustQ(t, pool.QueryRow(ctx,
		`INSERT INTO users (email, username, password_hash, full_name)
		 VALUES ($1, $2, 'x', 'Quota') RETURNING id::text`,
		"q-"+suffix+"@t.local", "q"+suffix).Scan(&userID))
	mustQ(t, pool.QueryRow(ctx,
		`INSERT INTO workspaces (name, slug, owner_id, storage_quota_bytes)
		 VALUES ('Q', $1, $2, $3) RETURNING id::text`,
		"q-"+suffix, userID, quotaBytes).Scan(&wsID))
	return wsID, userID
}

func charge(t *testing.T, pool *pgxpool.Pool, wsID string, delta int64) (int64, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	mustQ(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	total, err := ChargeTx(ctx, tx, wsID, delta)
	if err != nil {
		return 0, err
	}
	return total, tx.Commit(ctx)
}

func stored(t *testing.T, pool *pgxpool.Pool, wsID string) int64 {
	t.Helper()
	u, err := Read(context.Background(), pool, wsID)
	mustQ(t, err)
	return u.BytesUsed
}

// The first upload into a workspace that has no workspace_storage row must be
// charged like any other. The INSERT arm carries the quota predicate for exactly
// this: without it a brand-new workspace's first upload is unconditionally free,
// however large — and every workspace created after migration 026 starts in that
// state.
func TestFirstChargeIntoAWorkspaceWithNoRowIsStillCapped(t *testing.T) {
	pool := testDB(t)
	wsID, _ := workspace(t, pool, 1000)

	if _, err := charge(t, pool, wsID, 2000); !errors.Is(err, ErrExceeded) {
		t.Fatalf("first charge of 2000 against a 1000 quota = %v, want ErrExceeded", err)
	}
	if got := stored(t, pool, wsID); got != 0 {
		t.Errorf("a refused charge left bytes_used at %d, want 0", got)
	}

	total, err := charge(t, pool, wsID, 400)
	mustQ(t, err)
	if total != 400 {
		t.Errorf("first accepted charge returned %d, want 400", total)
	}
}

func TestChargeAccumulatesAndRefuses(t *testing.T) {
	pool := testDB(t)
	wsID, _ := workspace(t, pool, 1000)

	for _, step := range []struct {
		delta int64
		want  int64
		fail  bool
	}{
		{400, 400, false},
		{500, 900, false},
		{200, 0, true}, // 1100 > 1000
		{100, 1000, false},
	} {
		total, err := charge(t, pool, wsID, step.delta)
		switch {
		case step.fail && !errors.Is(err, ErrExceeded):
			t.Fatalf("charge(%d) = %v, want ErrExceeded", step.delta, err)
		case !step.fail && err != nil:
			t.Fatalf("charge(%d): %v", step.delta, err)
		case !step.fail && total != step.want:
			t.Fatalf("charge(%d) = %d, want %d", step.delta, total, step.want)
		}
	}
	if got := stored(t, pool, wsID); got != 1000 {
		t.Errorf("bytes_used = %d, want exactly 1000 — a refused charge must leave nothing behind", got)
	}
}

// THE PROPERTY THE WHOLE DESIGN EXISTS FOR.
//
// Two uploads that each read 900 of a 1000 quota would both decide they fit
// under a check-then-insert, and the workspace would land at 1100. Here the
// second transaction blocks on the first's row lock, re-evaluates against its
// COMMITTED value, and returns no rows.
//
// The test drives two real transactions and holds the first open across the
// second's attempt, which is the only way to observe the lock doing its job.
func TestConcurrentChargesCannotBothFit(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	wsID, _ := workspace(t, pool, 1000)

	if _, err := charge(t, pool, wsID, 600); err != nil {
		t.Fatal(err)
	}

	txA, err := pool.Begin(ctx)
	mustQ(t, err)
	defer func() { _ = txA.Rollback(ctx) }()

	// A takes the remaining 400 and does NOT commit yet.
	if _, err := ChargeTx(ctx, txA, wsID, 400); err != nil {
		t.Fatalf("transaction A: %v", err)
	}

	// B asks for 400 too. It must block until A commits, then be refused.
	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		txB, err := pool.Begin(ctx)
		if err != nil {
			done <- result{err}
			return
		}
		defer func() { _ = txB.Rollback(ctx) }()
		_, err = ChargeTx(ctx, txB, wsID, 400)
		done <- result{err}
	}()

	select {
	case r := <-done:
		t.Fatalf("transaction B returned %v while A still held the row; "+
			"it must block on the row lock, or two uploads can both fit into "+
			"the same remaining space", r.err)
	case <-time.After(300 * time.Millisecond):
		// Blocked, as required.
	}

	mustQ(t, txA.Commit(ctx))

	select {
	case r := <-done:
		if !errors.Is(r.err, ErrExceeded) {
			t.Fatalf("transaction B = %v, want ErrExceeded after A committed", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("transaction B never unblocked after A committed")
	}

	if got := stored(t, pool, wsID); got != 1000 {
		t.Errorf("bytes_used = %d, want 1000; both charges were applied and the quota was exceeded", got)
	}
}

func TestUnlimitedQuotaAcceptsAnything(t *testing.T) {
	pool := testDB(t)
	wsID, _ := workspace(t, pool, 0)

	total, err := charge(t, pool, wsID, 1<<40)
	mustQ(t, err)
	if total != 1<<40 {
		t.Errorf("unlimited workspace charged %d, want %d", total, int64(1)<<40)
	}
}

// A refund clamps at zero rather than erroring. Of the two wrong numbers, the
// one that under-charges is the one that does not lock a customer out of their
// own Drive.
func TestRefundClampsAtZero(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	wsID, _ := workspace(t, pool, 0)

	if _, err := charge(t, pool, wsID, 100); err != nil {
		t.Fatal(err)
	}

	tx, err := pool.Begin(ctx)
	mustQ(t, err)
	mustQ(t, RefundTx(ctx, tx, wsID, 500))
	mustQ(t, tx.Commit(ctx))

	if got := stored(t, pool, wsID); got != 0 {
		t.Errorf("over-refund left bytes_used at %d, want 0", got)
	}
}

// Recompute is the counterpart to the incremental arithmetic: it re-derives I1
// and reports what it changed. Drift here is a capacity bug, not an
// access-control one, so it reports rather than blocks.
func TestRecomputeRestoresTheInvariant(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	wsID, userID := workspace(t, pool, 0)

	// Two files, one with two versions: 100 + 250 + 400.
	var f1, f2 string
	mustQ(t, pool.QueryRow(ctx,
		`INSERT INTO files (workspace_id, user_id, name, content_type, size_bytes, storage_key)
		 VALUES ($1, $2, 'a', 'application/octet-stream', 100, 'k-a') RETURNING id::text`,
		wsID, userID).Scan(&f1))
	mustQ(t, pool.QueryRow(ctx,
		`INSERT INTO files (workspace_id, user_id, name, content_type, size_bytes, storage_key)
		 VALUES ($1, $2, 'b', 'application/octet-stream', 400, 'k-b2') RETURNING id::text`,
		wsID, userID).Scan(&f2))
	for _, v := range []struct {
		file string
		n    int
		size int64
		key  string
	}{{f1, 1, 100, "k-a"}, {f2, 1, 250, "k-b1"}, {f2, 2, 400, "k-b2"}} {
		_, err := pool.Exec(ctx,
			`INSERT INTO file_versions (file_id, version, storage_key, size_bytes, content_type, created_by)
			 VALUES ($1, $2, $3, $4, 'application/octet-stream', $5)`,
			v.file, v.n, v.key, v.size, userID)
		mustQ(t, err)
	}
	_, err := pool.Exec(ctx, `UPDATE files SET current_version = 2 WHERE id = $1`, f2)
	mustQ(t, err)

	// Deliberately wrong, as a drifted deployment would be.
	_, err = pool.Exec(ctx,
		`INSERT INTO workspace_storage (workspace_id, bytes_used) VALUES ($1, 999999)
		 ON CONFLICT (workspace_id) DO UPDATE SET bytes_used = 999999`, wsID)
	mustQ(t, err)

	before, after, err := Recompute(ctx, pool, wsID)
	mustQ(t, err)
	if before != 999999 {
		t.Errorf("Recompute reported before = %d, want the drifted 999999", before)
	}
	if after != 750 {
		t.Errorf("Recompute produced %d, want 750 — the sum over EVERY version row, "+
			"because an old version is still an object in the bucket", after)
	}

	b, err := ReadBreakdown(ctx, pool, wsID)
	mustQ(t, err)
	if b.DriftBytes != 0 {
		t.Errorf("drift after recompute = %d, want 0", b.DriftBytes)
	}
	if b.VersionBytes != 250 {
		t.Errorf("version_bytes = %d, want 250 — the non-head version nobody expects to be charged",
			b.VersionBytes)
	}
	if b.LiveBytes != 500 {
		t.Errorf("live_bytes = %d, want 500 (100 + the head 400)", b.LiveBytes)
	}
}

var _ = pgx.ErrNoRows
