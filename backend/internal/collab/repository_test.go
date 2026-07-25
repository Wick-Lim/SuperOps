package collab

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Wick-Lim/SuperOps/backend/internal/ws"
)

func TestEnsureDocument(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	resourceID := f.newDriveFile(t)

	first, err := f.repo.EnsureDocument(ctx, f.workspaceID, "document", resourceID, f.owner)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	second, err := f.repo.EnsureDocument(ctx, f.workspaceID, "document", resourceID, f.member)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("opening the same object twice created two documents: %s and %s", first.ID, second.ID)
	}
	if first.HeadSeq != 0 || first.SnapshotSeq != 0 {
		t.Fatalf("fresh document = head %d snapshot %d, want 0/0", first.HeadSeq, first.SnapshotSeq)
	}

	// The same object claimed by a second workspace must not hand that
	// workspace the first one's document.
	other := newFixture(t)
	if _, err := f.repo.EnsureDocument(ctx, other.workspaceID, "document", resourceID, other.owner); !errors.Is(err, ErrResourceConflict) {
		t.Fatalf("cross-workspace open error = %v, want ErrResourceConflict", err)
	}
}

// TestAppendUpdateSequencing is the property the whole design rests on: the log
// has no gaps and no duplicates, even when many writers append at once.
func TestAppendUpdateSequencing(t *testing.T) {
	f := newFixture(t)
	doc := f.newDocument(t)
	ctx := context.Background()

	const writers, perWriter = 8, 12

	var wg sync.WaitGroup
	seqs := make(chan int64, writers*perWriter)
	errs := make(chan error, writers*perWriter)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				seq, _, err := f.repo.AppendUpdate(ctx, doc.ID, f.member, []byte(fmt.Sprintf("w%d-%d", w, i)))
				if err != nil {
					errs <- err
					return
				}
				seqs <- seq
			}
		}(w)
	}
	wg.Wait()
	close(seqs)
	close(errs)
	for err := range errs {
		t.Fatalf("append: %v", err)
	}

	seen := make(map[int64]bool, writers*perWriter)
	for seq := range seqs {
		if seen[seq] {
			t.Fatalf("sequence number %d handed out twice", seq)
		}
		seen[seq] = true
	}
	for i := int64(1); i <= writers*perWriter; i++ {
		if !seen[i] {
			t.Fatalf("sequence number %d was never handed out; the log has a hole", i)
		}
	}

	state, err := f.repo.Load(ctx, doc.ID, 0, maxStateUpdates)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(state.Updates) != writers*perWriter {
		t.Fatalf("loaded %d updates, want %d", len(state.Updates), writers*perWriter)
	}
	for i, u := range state.Updates {
		if u.Seq != int64(i+1) {
			t.Fatalf("update %d has seq %d; the log is not contiguous in order", i, u.Seq)
		}
	}
	if state.ThroughSeq != int64(writers*perWriter) {
		t.Fatalf("through_seq = %d, want %d", state.ThroughSeq, writers*perWriter)
	}
}

func TestLoadSince(t *testing.T) {
	f := newFixture(t)
	doc := f.newDocument(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, _, err := f.repo.AppendUpdate(ctx, doc.ID, f.member, []byte{byte(i)}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	tests := []struct {
		name      string
		since     int64
		wantSeqs  []int64
		wantThrgh int64
	}{
		{"from scratch", 0, []int64{1, 2, 3, 4, 5}, 5},
		{"partway", 3, []int64{4, 5}, 5},
		{"already current", 5, nil, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, err := f.repo.Load(ctx, doc.ID, tt.since, maxStateUpdates)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if len(state.Updates) != len(tt.wantSeqs) {
				t.Fatalf("got %d updates, want %d", len(state.Updates), len(tt.wantSeqs))
			}
			for i, want := range tt.wantSeqs {
				if state.Updates[i].Seq != want {
					t.Errorf("update %d seq = %d, want %d", i, state.Updates[i].Seq, want)
				}
			}
			if state.ThroughSeq != tt.wantThrgh {
				t.Errorf("through_seq = %d, want %d", state.ThroughSeq, tt.wantThrgh)
			}
			if state.Snapshot != nil {
				t.Errorf("uncompacted document returned a snapshot")
			}
		})
	}
}

func TestLoadTruncates(t *testing.T) {
	f := newFixture(t)
	doc := f.newDocument(t)
	ctx := context.Background()

	for i := 0; i < 6; i++ {
		if _, _, err := f.repo.AppendUpdate(ctx, doc.ID, f.member, []byte{byte(i)}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	state, err := f.repo.Load(ctx, doc.ID, 0, 4)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !state.HasMore {
		t.Fatal("truncated response did not set has_more; the client would stop four updates short")
	}
	if len(state.Updates) != 4 || state.ThroughSeq != 4 {
		t.Fatalf("got %d updates through %d, want 4 through 4", len(state.Updates), state.ThroughSeq)
	}

	rest, err := f.repo.Load(ctx, doc.ID, state.ThroughSeq, 4)
	if err != nil {
		t.Fatalf("load rest: %v", err)
	}
	if rest.HasMore || len(rest.Updates) != 2 || rest.ThroughSeq != 6 {
		t.Fatalf("second page: %d updates through %d has_more=%v, want 2 through 6 has_more=false",
			len(rest.Updates), rest.ThroughSeq, rest.HasMore)
	}
}

// TestSnapshotCompacts covers the ordinary compaction: the log behind the
// snapshot is gone, and a client loading from scratch still sees everything.
func TestSnapshotCompacts(t *testing.T) {
	f := newFixture(t)
	doc := f.newDocument(t)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		if _, _, err := f.repo.AppendUpdate(ctx, doc.ID, f.member, []byte{byte(i)}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	compacted, err := f.repo.SaveSnapshot(ctx, doc.ID, 8, f.member, []byte("state-through-8"))
	if err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	if compacted != 8 {
		t.Fatalf("compacted %d updates, want 8", compacted)
	}

	state, err := f.repo.Load(ctx, doc.ID, 0, maxStateUpdates)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if string(state.Snapshot) != "state-through-8" || state.SnapshotSeq != 8 {
		t.Fatalf("snapshot = %q at %d, want %q at 8", state.Snapshot, state.SnapshotSeq, "state-through-8")
	}
	if len(state.Updates) != 2 || state.Updates[0].Seq != 9 {
		t.Fatalf("tail = %d updates starting at %d, want 2 starting at 9", len(state.Updates), state.Updates[0].Seq)
	}
	if state.ThroughSeq != 10 {
		t.Fatalf("through_seq = %d, want 10", state.ThroughSeq)
	}

	// A client that is already past the snapshot must not be sent a megabyte of
	// state it does not need.
	caught, err := f.repo.Load(ctx, doc.ID, 9, maxStateUpdates)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if caught.Snapshot != nil {
		t.Fatal("a client past the snapshot was sent the snapshot anyway")
	}
	if len(caught.Updates) != 1 || caught.Updates[0].Seq != 10 {
		t.Fatalf("tail = %v, want just seq 10", caught.Updates)
	}
}

func TestSnapshotStale(t *testing.T) {
	f := newFixture(t)
	doc := f.newDocument(t)
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		if _, _, err := f.repo.AppendUpdate(ctx, doc.ID, f.member, []byte{byte(i)}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if _, err := f.repo.SaveSnapshot(ctx, doc.ID, 4, f.member, []byte("state")); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}

	tests := []struct {
		name    string
		through int64
	}{
		{"already compacted", 4},
		{"behind the snapshot", 2},
		{"ahead of the log", 99},
		{"zero", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := f.repo.SaveSnapshot(ctx, doc.ID, tt.through, f.member, []byte("x")); !errors.Is(err, ErrStaleSnapshot) {
				t.Fatalf("error = %v, want ErrStaleSnapshot", err)
			}
		})
	}
}

// TestCompactionUnderConcurrentWrites is the race the design is built around:
// compaction must never delete an update a writer has not committed yet, and
// must never leave a hole between the snapshot and the surviving log.
//
// It runs many appends against repeated compaction attempts and then asserts
// the invariant directly: every seq from snapshot_seq+1 to head_seq is still on
// disk, and nothing below snapshot_seq survived.
func TestCompactionUnderConcurrentWrites(t *testing.T) {
	f := newFixture(t)
	doc := f.newDocument(t)
	ctx := context.Background()

	const writers, perWriter = 6, 25

	var wg sync.WaitGroup
	appendErrs := make(chan error, writers)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if _, _, err := f.repo.AppendUpdate(ctx, doc.ID, f.member, []byte(fmt.Sprintf("w%d-%d", w, i))); err != nil {
					appendErrs <- err
					return
				}
			}
		}(w)
	}

	// The compactor behaves like a client: it snapshots whatever the head is at
	// the moment it looks, which is exactly the racy thing to do.
	compactorDone := make(chan struct{})
	go func() {
		defer close(compactorDone)
		for i := 0; i < 40; i++ {
			var head int64
			if err := f.pool.QueryRow(ctx, `SELECT head_seq FROM collab_documents WHERE id = $1`, doc.ID).Scan(&head); err != nil {
				return
			}
			if head == 0 {
				continue
			}
			// A stale snapshot is the expected outcome of losing the race and
			// must not be treated as a failure.
			if _, err := f.repo.SaveSnapshot(ctx, doc.ID, head, f.owner, []byte(fmt.Sprintf("state-%d", head))); err != nil && !errors.Is(err, ErrStaleSnapshot) {
				return
			}
		}
	}()

	wg.Wait()
	close(appendErrs)
	for err := range appendErrs {
		t.Fatalf("append during compaction: %v", err)
	}
	<-compactorDone

	var head, snapshotSeq int64
	if err := f.pool.QueryRow(ctx,
		`SELECT head_seq, snapshot_seq FROM collab_documents WHERE id = $1`, doc.ID,
	).Scan(&head, &snapshotSeq); err != nil {
		t.Fatalf("read document: %v", err)
	}
	if head != writers*perWriter {
		t.Fatalf("head_seq = %d, want %d — an append was lost", head, writers*perWriter)
	}
	if snapshotSeq == 0 {
		t.Fatal("no compaction ever succeeded; the test proved nothing")
	}

	rows, err := f.pool.Query(ctx,
		`SELECT seq FROM collab_updates WHERE document_id = $1 ORDER BY seq`, doc.ID)
	if err != nil {
		t.Fatalf("list surviving updates: %v", err)
	}
	defer rows.Close()

	var survived []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			t.Fatalf("scan: %v", err)
		}
		survived = append(survived, seq)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list surviving updates: %v", err)
	}

	want := head - snapshotSeq
	if int64(len(survived)) != want {
		t.Fatalf("%d updates survived past snapshot %d of head %d, want %d — "+
			"compaction deleted an update it had not snapshotted, or left one it had",
			len(survived), snapshotSeq, head, want)
	}
	for i, seq := range survived {
		if seq != snapshotSeq+int64(i)+1 {
			t.Fatalf("surviving update %d has seq %d, want %d — the log has a hole after compaction",
				i, seq, snapshotSeq+int64(i)+1)
		}
	}

	// And the load path agrees: snapshot plus tail, contiguous to the head.
	state, err := f.repo.Load(ctx, doc.ID, 0, maxStateUpdates)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if state.SnapshotSeq != snapshotSeq || state.ThroughSeq != head {
		t.Fatalf("load returned snapshot %d through %d, want %d through %d",
			state.SnapshotSeq, state.ThroughSeq, snapshotSeq, head)
	}
}

// TestSnapshotRetention: an old snapshot is pruned, but not the ones that make
// a bad snapshot recoverable.
func TestSnapshotRetention(t *testing.T) {
	f := newFixture(t)
	doc := f.newDocument(t)
	ctx := context.Background()

	for i := 0; i < snapshotRetention+3; i++ {
		if _, _, err := f.repo.AppendUpdate(ctx, doc.ID, f.member, []byte{byte(i)}); err != nil {
			t.Fatalf("append: %v", err)
		}
		if _, err := f.repo.SaveSnapshot(ctx, doc.ID, int64(i+1), f.member, []byte{byte(i)}); err != nil {
			t.Fatalf("save snapshot: %v", err)
		}
	}

	var kept int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM collab_snapshots WHERE document_id = $1`, doc.ID).Scan(&kept); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if kept != snapshotRetention {
		t.Fatalf("kept %d snapshots, want %d", kept, snapshotRetention)
	}
}

func TestMissingDocument(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	missing := uuid.NewString()

	if doc, err := f.repo.Get(ctx, missing); err != nil || doc != nil {
		t.Fatalf("Get(missing) = %v, %v; want nil, nil", doc, err)
	}
	if _, _, err := f.repo.AppendUpdate(ctx, missing, f.member, []byte("x")); !errors.Is(err, ws.ErrRoomNotFound) {
		t.Fatalf("append to a missing document = %v, want ErrRoomNotFound", err)
	}
	if _, err := f.repo.Load(ctx, missing, 0, maxStateUpdates); !errors.Is(err, ws.ErrRoomNotFound) {
		t.Fatalf("load a missing document = %v, want ErrRoomNotFound", err)
	}
	if _, err := f.repo.SaveSnapshot(ctx, missing, 1, f.member, []byte("x")); !errors.Is(err, ws.ErrRoomNotFound) {
		t.Fatalf("snapshot a missing document = %v, want ErrRoomNotFound", err)
	}
}

func TestPayloadSizeIsEnforcedByPostgres(t *testing.T) {
	f := newFixture(t)
	doc := f.newDocument(t)

	// The service refuses this first; the CHECK is the backstop that makes a
	// bug in that bound a failed insert rather than an unbounded row.
	_, _, err := f.repo.AppendUpdate(context.Background(), doc.ID, f.member, make([]byte, MaxUpdateBytes+1))
	if err == nil {
		t.Fatal("an oversized update was accepted by Postgres")
	}
}
