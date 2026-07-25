package audit

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type world struct {
	pool        *pgxpool.Pool
	svc         *Service
	workspaceID string
	actorID     string
}

var worldSeq int

func newWorld(t *testing.T) *world {
	t.Helper()
	pool := testDB(t)
	ctx := t.Context()
	worldSeq++

	w := &world{pool: pool, svc: NewService(pool, quietLogger()),
		workspaceID: uuid.NewString(), actorID: uuid.NewString()}

	name := fmt.Sprintf("audit-%d", worldSeq)
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, username, full_name) VALUES ($1, $2, $3, $3)`,
		w.actorID, name+"@test.local", name); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO workspaces (id, name, slug, owner_id) VALUES ($1, $2, $2, $3)`,
		w.workspaceID, name, w.actorID); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	return w
}

// The regression for the live bug migration 021 exists to fix: resource_id was
// UUID, internal/admin/mail.go passed "smtp" into it, the INSERT failed with
// 22P02 and the error was discarded — so mail.test_sent had never once been
// recorded. The Go side now puts the transport in metadata, but the column has
// to be able to hold a non-uuid identifier either way, because several
// categories legitimately have one.
func TestNonUUIDResourceIDIsRecorded(t *testing.T) {
	w := newWorld(t)

	if err := w.svc.Record(t.Context(), Entry{
		WorkspaceID:  w.workspaceID,
		ActorID:      w.actorID,
		Action:       ActionMailTestSent,
		ResourceType: "mail",
		ResourceID:   "smtp",
		Metadata:     map[string]interface{}{"transport": "smtp"},
	}); err != nil {
		t.Fatalf("Record with a non-uuid resource_id: %v", err)
	}

	var n int
	if err := w.pool.QueryRow(t.Context(),
		`SELECT COUNT(*) FROM audit_logs WHERE workspace_id = $1 AND action = $2 AND resource_id = 'smtp'`,
		w.workspaceID, ActionMailTestSent).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("%d rows for %s, want 1", n, ActionMailTestSent)
	}
}

// Fifty downloads of one file in an afternoon is one fact with a count, not
// fifty rows. That single decision is most of what keeps audit_logs smaller than
// `messages` rather than 30x larger.
func TestReadCoalescing(t *testing.T) {
	w := newWorld(t)
	fileID := uuid.NewString()

	const repeats = 50
	for range repeats {
		if err := w.svc.Record(t.Context(), Entry{
			WorkspaceID:  w.workspaceID,
			ActorID:      w.actorID,
			Action:       ActionFileDownloaded,
			ResourceType: "file",
			ResourceID:   fileID,
			Coalesce:     true,
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	var rows, count int
	var lastAt, createdAt time.Time
	if err := w.pool.QueryRow(t.Context(),
		`SELECT COUNT(*), MAX(event_count), MAX(last_at), MAX(created_at)
		   FROM audit_logs WHERE workspace_id = $1 AND resource_id = $2`,
		w.workspaceID, fileID).Scan(&rows, &count, &lastAt, &createdAt); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d rows after %d downloads of one file, want 1", rows, repeats)
	}
	if count != repeats {
		t.Fatalf("event_count = %d, want %d", count, repeats)
	}
	if !lastAt.After(createdAt) && !lastAt.Equal(createdAt) {
		t.Fatalf("last_at %s is before created_at %s", lastAt, createdAt)
	}
	// created_at is pinned to the start of the hour bucket, which is what lets
	// the unique index include the partition key and still behave like a unique
	// index on dedupe_key alone.
	if !createdAt.UTC().Equal(createdAt.UTC().Truncate(time.Hour)) {
		t.Fatalf("created_at %s is not an hour boundary", createdAt.UTC())
	}

	// A coalesced row is mutated on every repeat, so it must NOT be chained — a
	// hash over it would go stale on the second event. Migration 021 enforces
	// that with a CHECK; this asserts the code respects it rather than relying
	// on the constraint to catch a mistake.
	var chainSeq *int64
	if err := w.pool.QueryRow(t.Context(),
		`SELECT chain_seq FROM audit_logs WHERE workspace_id = $1 AND resource_id = $2`,
		w.workspaceID, fileID).Scan(&chainSeq); err != nil {
		t.Fatalf("read chain_seq: %v", err)
	}
	if chainSeq != nil {
		t.Fatalf("a coalesced row carries chain_seq %d; it must be unchained", *chainSeq)
	}
}

// The chain has one job: make an in-place edit or a deletion visible. It is only
// half the answer — anyone with UPDATE can recompute the whole chain, which is
// why anchoring exists — but the half it does has to actually work.
func TestChainDetectsTamper(t *testing.T) {
	w := newWorld(t)
	for i := range 5 {
		if err := w.svc.Record(t.Context(), Entry{
			WorkspaceID:  w.workspaceID,
			ActorID:      w.actorID,
			Action:       ActionLogin,
			ResourceType: "user",
			ResourceID:   fmt.Sprintf("r%d", i),
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	v := NewVerifier(w.pool, nil, quietLogger())
	statuses, err := v.Verify(t.Context(), []string{w.workspaceID})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(statuses) != 1 || !statuses[0].OK || statuses[0].HeadSeq != 5 {
		t.Fatalf("a freshly written chain must verify clean: %+v", statuses)
	}

	// The edit an administrator with psql would make.
	if _, err := w.pool.Exec(t.Context(),
		`UPDATE audit_logs SET action = 'user.logout' WHERE workspace_id = $1 AND chain_seq = 3`,
		w.workspaceID); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	statuses, err = v.Verify(t.Context(), []string{w.workspaceID})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if statuses[0].OK {
		t.Fatal("an edited row was not detected")
	}
	found := false
	for _, b := range statuses[0].Breaks {
		if b.Seq == 3 && b.Reason == "hash_mismatch" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the break was not reported at seq 3: %+v", statuses[0].Breaks)
	}
}

// A deletion leaves a numeric gap AND breaks the link, and both are reported —
// the gap is what a DELETE looks like, the link is what makes it impossible to
// delete a row and renumber the rest without also recomputing every hash after
// it.
func TestChainDetectsDeletion(t *testing.T) {
	w := newWorld(t)
	for i := range 4 {
		if err := w.svc.Record(t.Context(), Entry{
			WorkspaceID: w.workspaceID, ActorID: w.actorID,
			Action: ActionLogin, ResourceType: "user", ResourceID: fmt.Sprintf("r%d", i),
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if _, err := w.pool.Exec(t.Context(),
		`DELETE FROM audit_logs WHERE workspace_id = $1 AND chain_seq = 2`, w.workspaceID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	statuses, err := NewVerifier(w.pool, nil, quietLogger()).Verify(t.Context(), []string{w.workspaceID})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if statuses[0].OK {
		t.Fatal("a deleted row was not detected")
	}
	missing := false
	for _, b := range statuses[0].Breaks {
		if b.Seq == 2 && b.Reason == "missing" {
			missing = true
		}
	}
	if !missing {
		t.Fatalf("the deletion was not reported as a gap at seq 2: %+v", statuses[0].Breaks)
	}
}

// The anchor is the only layer the local administrator does not control, so the
// number that says how much of the log is protected has to actually move.
func TestAnchorAdvancesOnAClaimedChain(t *testing.T) {
	w := newWorld(t)
	for i := range 3 {
		if err := w.svc.Record(t.Context(), Entry{
			WorkspaceID: w.workspaceID, ActorID: w.actorID,
			Action: ActionLogin, ResourceType: "user", ResourceID: fmt.Sprintf("r%d", i),
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	sink := &recordingSink{}
	v := NewVerifier(w.pool, sink, quietLogger())
	if _, err := v.Anchor(t.Context()); err != nil {
		t.Fatalf("Anchor: %v", err)
	}

	var anchored int64
	if err := w.pool.QueryRow(t.Context(),
		`SELECT anchored_seq FROM audit_chain_heads WHERE workspace_id = $1`,
		w.workspaceID).Scan(&anchored); err != nil {
		t.Fatalf("read anchor: %v", err)
	}
	if anchored != 3 {
		t.Fatalf("anchored_seq = %d, want 3", anchored)
	}
	if !sink.sawWorkspace(w.workspaceID) {
		t.Fatal("the sink was never handed this workspace's anchor")
	}

	// A second run with nothing new ships nothing: the anchor is a watermark,
	// not a heartbeat.
	before := sink.count()
	if _, err := v.Anchor(t.Context()); err != nil {
		t.Fatalf("second Anchor: %v", err)
	}
	if sink.count() != before {
		t.Fatal("an unchanged chain was anchored again")
	}
}

// A broken chain must NOT be anchored: recording the tampered state as the
// trusted one is worse than not anchoring at all.
func TestBrokenChainIsNotAnchored(t *testing.T) {
	w := newWorld(t)
	if err := w.svc.Record(t.Context(), Entry{
		WorkspaceID: w.workspaceID, ActorID: w.actorID,
		Action: ActionLogin, ResourceType: "user",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if _, err := w.pool.Exec(t.Context(),
		`UPDATE audit_logs SET action = 'tampered' WHERE workspace_id = $1`, w.workspaceID); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	sink := &recordingSink{}
	if _, err := NewVerifier(w.pool, sink, quietLogger()).Anchor(t.Context()); err != nil {
		t.Fatalf("Anchor: %v", err)
	}
	if sink.sawWorkspace(w.workspaceID) {
		t.Fatal("a broken chain was shipped as an anchor")
	}
	var anchored int64
	if err := w.pool.QueryRow(t.Context(),
		`SELECT anchored_seq FROM audit_chain_heads WHERE workspace_id = $1`,
		w.workspaceID).Scan(&anchored); err != nil {
		t.Fatalf("read anchor: %v", err)
	}
	if anchored != 0 {
		t.Fatalf("anchored_seq = %d for a broken chain, want 0", anchored)
	}
}

// A missing partition is a failed INSERT, i.e. a lost audit record. Two months
// of lead time is the guard; this proves the job actually creates them and that
// a row dated into the next month lands in the new partition rather than
// erroring.
func TestPartitionRollover(t *testing.T) {
	pool := testDB(t)
	ctx := t.Context()

	if _, err := EnsurePartitions(ctx, pool, PartitionLead); err != nil {
		t.Fatalf("EnsurePartitions: %v", err)
	}

	next := time.Now().UTC().AddDate(0, 1, 0)
	want := "audit_logs_p" + next.Format("2006_01")

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_class c JOIN pg_inherits i ON i.inhrelid = c.oid
		                WHERE i.inhparent = 'audit_logs'::regclass AND c.relname = $1)`,
		want).Scan(&exists); err != nil {
		t.Fatalf("probe partition: %v", err)
	}
	if !exists {
		t.Fatalf("partition %s was not created", want)
	}

	// A row dated into the next month routes there. Without the partition this
	// INSERT is `no partition of relation "audit_logs" found for row`.
	id := uuid.NewString()
	at := time.Date(next.Year(), next.Month(), 15, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx,
		`INSERT INTO audit_logs (id, action, resource_type, created_at) VALUES ($1, 'test.rollover', 'test', $2)`,
		id, at); err != nil {
		t.Fatalf("insert into the next month: %v", err)
	}
	var landed string
	if err := pool.QueryRow(ctx,
		`SELECT tableoid::regclass::text FROM audit_logs WHERE id = $1 AND created_at = $2`,
		id, at).Scan(&landed); err != nil {
		t.Fatalf("locate row: %v", err)
	}
	if landed != want {
		t.Fatalf("row landed in %s, want %s", landed, want)
	}

	// Re-running is a no-op rather than an error: two replicas hit the same tick.
	if _, err := EnsurePartitions(ctx, pool, PartitionLead); err != nil {
		t.Fatalf("second EnsurePartitions: %v", err)
	}
}

// Retention is a DROP, not a DELETE. That is the entire payoff of migration
// 021's partitioning: milliseconds, no locks held over user rows, disk returned
// immediately — instead of the batched, capped, advisory-locked DELETE the
// message retention job has to be.
//
// Migration 021's conversion partition spans MINVALUE..(next month), so it
// overlaps every historical range by construction — which is correct in
// production (it holds all pre-migration history) and means this test has to
// detach it to have anywhere to put an expired month. It is re-attached on the
// way out so the rest of the package's tests see the schema they expect.
func TestRetentionDropsWholePartitions(t *testing.T) {
	pool := testDB(t)
	ctx := t.Context()

	legacy, bound := legacyPartition(t, pool)
	if _, err := pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE audit_logs DETACH PARTITION %s`, legacy)); err != nil {
		t.Fatalf("detach the conversion partition: %v", err)
	}
	t.Cleanup(func() {
		//nolint:usetesting // t.Context is already cancelled during cleanup
		if _, err := pool.Exec(context.Background(), fmt.Sprintf(
			`ALTER TABLE audit_logs ATTACH PARTITION %s %s`, legacy, bound)); err != nil {
			t.Errorf("re-attach the conversion partition: %v", err)
		}
	})

	// Something has to hold today's rows once the catch-all is gone, or every
	// later test in this package fails on a missing partition.
	if _, err := EnsurePartitions(ctx, pool, PartitionLead); err != nil {
		t.Fatalf("EnsurePartitions: %v", err)
	}

	// A partition entirely in the past, with a row in it.
	old := time.Now().UTC().AddDate(0, -14, 0)
	from := time.Date(old.Year(), old.Month(), 1, 0, 0, 0, 0, time.UTC)
	name := "audit_logs_p" + from.Format("2006_01")
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF audit_logs FOR VALUES FROM ('%s') TO ('%s')`,
		name, from.Format("2006-01-02"), from.AddDate(0, 1, 0).Format("2006-01-02"))); err != nil {
		t.Fatalf("create old partition: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO audit_logs (id, action, resource_type, created_at) VALUES ($1, 'test.old', 'test', $2)`,
		uuid.NewString(), from.AddDate(0, 0, 5)); err != nil {
		t.Fatalf("insert into the old partition: %v", err)
	}

	dropped, err := DropExpiredPartitions(ctx, pool, 365)
	if err != nil {
		t.Fatalf("DropExpiredPartitions: %v", err)
	}
	if !containsString(dropped, name) {
		t.Fatalf("dropped %v, want it to include %s", dropped, name)
	}

	// The current month survives, which is the failure that would matter.
	current := "audit_logs_p" + time.Now().UTC().Format("2006_01")
	if containsString(dropped, current) {
		t.Fatalf("retention dropped the CURRENT month's partition %s", current)
	}

	// Retention disabled drops nothing.
	if got, err := DropExpiredPartitions(ctx, pool, 0); err != nil || len(got) != 0 {
		t.Fatalf("AUDIT_RETENTION_DAYS=0 dropped %v (err %v); it must disable retention", got, err)
	}
}

// legacyPartition finds migration 021's conversion partition — the one whose
// lower bound is MINVALUE — and returns its name and its bound expression.
func legacyPartition(t *testing.T, pool *pgxpool.Pool) (string, string) {
	t.Helper()
	var name, bound string
	err := pool.QueryRow(t.Context(), `
		SELECT c.relname, pg_get_expr(c.relpartbound, c.oid)
		  FROM pg_class c
		  JOIN pg_inherits i ON i.inhrelid = c.oid
		 WHERE i.inhparent = 'audit_logs'::regclass
		   AND pg_get_expr(c.relpartbound, c.oid) LIKE '%MINVALUE%'`).Scan(&name, &bound)
	if err != nil {
		t.Fatalf("locate the conversion partition: %v", err)
	}
	return name, bound
}

// Try's stated contract: the write is detached from the caller's cancellation,
// so a client that hangs up immediately after a failed login still leaves a
// record. Nothing asserted it before.
func TestRecordSurvivesRequestCancellation(t *testing.T) {
	w := newWorld(t)
	rid := uuid.NewString()

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // the client is already gone

	w.svc.Try(ctx, Entry{
		WorkspaceID: w.workspaceID, ActorID: w.actorID,
		Action: ActionLoginFailed, ResourceType: "user", ResourceID: rid,
	})

	var n int
	if err := w.pool.QueryRow(t.Context(),
		`SELECT COUNT(*) FROM audit_logs WHERE resource_id = $1`, rid).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("%d rows after a cancelled request, want 1 — Try must detach from the caller's context", n)
	}
	if got := w.svc.Failures(); got != 0 {
		t.Fatalf("Failures() = %d after a successful write", got)
	}
}

// The Tier 2 queue has to actually land its records, and it has to drain on
// Close rather than discarding what is in flight — those are the records for the
// last requests a replica served, which is exactly what an incident timeline
// needs.
func TestBufferDrainsOnClose(t *testing.T) {
	w := newWorld(t)
	w.svc.StartBuffer(BufferConfig{Size: 128, Workers: 1, FlushInterval: time.Hour, BatchSize: 1000})

	const n = 20
	for i := range n {
		w.svc.Buffer(t.Context(), Entry{
			WorkspaceID: w.workspaceID, ActorID: w.actorID,
			Action: ActionFileDownloaded, ResourceType: "file", ResourceID: fmt.Sprintf("f%d", i),
		})
	}
	// FlushInterval is an hour, so nothing has been written yet; Close is what
	// has to flush.
	w.svc.Close()

	var got int
	if err := w.pool.QueryRow(t.Context(),
		`SELECT COUNT(*) FROM audit_logs WHERE workspace_id = $1 AND action = $2`,
		w.workspaceID, ActionFileDownloaded).Scan(&got); err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != n {
		t.Fatalf("%d buffered entries landed, want %d — Close must drain, not discard", got, n)
	}
	if d := w.svc.Dropped(); d != 0 {
		t.Fatalf("Dropped() = %d with a queue that was never full", d)
	}
}

// A full buffer drops — counted and logged, never silently. The counter is what
// makes the drop alertable, and an unalertable drop is the failure that makes
// the whole surface worthless.
func TestBufferDropsAreCounted(t *testing.T) {
	w := newWorld(t)
	// One slot, no workers draining it: the second entry has nowhere to go.
	w.svc.buffer = &buffer{
		ch:     make(chan Entry, 1),
		cfg:    DefaultBufferConfig,
		logger: quietLogger(),
	}

	for range 10 {
		w.svc.Buffer(t.Context(), Entry{Action: ActionFileDownloaded, ResourceType: "file"})
	}
	if got := w.svc.Dropped(); got != 9 {
		t.Fatalf("Dropped() = %d, want 9 (10 entries into a queue of 1)", got)
	}
}

type recordingSink struct{ shipped []Anchor }

func (s *recordingSink) Name() string { return "recording" }

func (s *recordingSink) Ship(_ context.Context, anchors []Anchor) error {
	s.shipped = append(s.shipped, anchors...)
	return nil
}

func (s *recordingSink) count() int { return len(s.shipped) }

func (s *recordingSink) sawWorkspace(id string) bool {
	for _, a := range s.shipped {
		if a.WorkspaceID == id {
			return true
		}
	}
	return false
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
