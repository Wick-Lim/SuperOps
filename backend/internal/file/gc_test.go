package file

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// The key format
// ---------------------------------------------------------------------------

func TestObjectDay(t *testing.T) {
	tests := []struct {
		key  string
		want string // empty = unparseable
	}{
		{"7c9e6679-7425-40de-944b-e07fc1f90ae7/2026/07/25/abc.png", "2026-07-25"},
		{"7c9e6679-7425-40de-944b-e07fc1f90ae7/2026/07/25/nested/abc.png", "2026-07-25"},
		{"7c9e6679-7425-40de-944b-e07fc1f90ae7/abc.png", ""}, // too few segments
		{"ws/2026/13/40/abc.png", ""},                        // not a date
		{"", ""},
	}
	for _, tt := range tests {
		day, ok := objectDay(tt.key)
		if ok != (tt.want != "") {
			t.Errorf("objectDay(%q) ok = %v, want %v", tt.key, ok, tt.want != "")
			continue
		}
		if ok && day.Format("2006-01-02") != tt.want {
			t.Errorf("objectDay(%q) = %s, want %s", tt.key, day.Format("2006-01-02"), tt.want)
		}
	}
}

// The bucket sweep deletes objects whose row is gone. An object that was PUT
// seconds ago and whose INSERT has not committed yet looks exactly like one, so
// nothing may be swept until the grace period has certainly elapsed — and a
// storage key only carries a date, not a time.
func TestObjectSweepHonoursTheGracePeriod(t *testing.T) {
	const key = "7c9e6679-7425-40de-944b-e07fc1f90ae7/2026/07/25/abc.png"

	// Worst case for a start-of-day comparison: uploaded at 23:59:59 on the
	// 25th, swept at 00:00:01 on the 26th, two seconds old.
	freshUpload := time.Date(2026, 7, 26, 0, 0, 1, 0, time.UTC)
	if olderThan(key, freshUpload.Add(-GCGrace)) {
		t.Error("an object that may be seconds old was eligible for deletion")
	}

	// Even a full day later it is still inside the slack that covers the date's
	// granularity and the deployment's timezone.
	if olderThan(key, time.Date(2026, 7, 27, 0, 0, 1, 0, time.UTC).Add(-GCGrace)) {
		t.Error("swept before the grace period could have elapsed in every timezone")
	}

	// But it must not become immortal: a few days on, it is collectable.
	old := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if !olderThan(key, old.Add(-GCGrace)) {
		t.Error("an object a week old was never collected")
	}

	// Anything whose age cannot be established from the key is left alone.
	if olderThan("no-date-here.png", old) {
		t.Error("an unparseable key must never be swept")
	}
}

// Whatever the slack is, it has to be at least a day (the key's granularity) on
// top of the grace period, or TestObjectSweepHonoursTheGracePeriod passes by
// coincidence.
func TestObjectKeyDateSlackCoversDateGranularity(t *testing.T) {
	if GCKeyDateSlack < 24*time.Hour {
		t.Fatalf("GCKeyDateSlack %s is shorter than the one-day granularity of a storage key",
			GCKeyDateSlack)
	}
}

func TestSplitN(t *testing.T) {
	got := splitN("a/b/c/d/e/f", '/', 5)
	want := []string{"a", "b", "c", "d", "e/f"}
	if len(got) != len(want) {
		t.Fatalf("splitN = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitN = %v, want %v", got, want)
		}
	}
	if got := splitN("abc", '/', 5); len(got) != 1 || got[0] != "abc" {
		t.Fatalf("splitN = %v, want [abc]", got)
	}
}

// ---------------------------------------------------------------------------
// The two predicates that delete customer data
// ---------------------------------------------------------------------------
//
// docs/plans/02-drive.md §11 calls this "the regression that justifies the
// phase", and docs/plans/README.md ruling 2 makes internal/file the single
// owner of both predicates. Every case below is written so that reverting one
// clause fails it — see TestGCPredicatesFailIfReverted, which proves that
// claim rather than asserting it.

// memStore is a bucket a test controls. Not a mock of MinIO: it is the bucket,
// with exactly the two operations Collect performs.
type memStore struct {
	mu      sync.Mutex
	objects map[string]bool
	failOn  map[string]bool // keys whose Delete errors
}

func newMemStore(keys ...string) *memStore {
	s := &memStore{objects: map[string]bool{}, failOn: map[string]bool{}}
	for _, k := range keys {
		s.objects[k] = true
	}
	return s
}

func (s *memStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failOn[key] {
		return fmt.Errorf("simulated storage failure for %q", key)
	}
	delete(s.objects, key)
	return nil
}

func (s *memStore) List(_ context.Context, prefix string, limit int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []string{}
	for k := range s.objects {
		if prefix == "" || (len(k) >= len(prefix) && k[:len(prefix)] == prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *memStore) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objects[key]
}

var _ ObjectStore = (*memStore)(nil)

// fixture is one workspace with a user and a Drive root, plus helpers to place
// files in each of the states the predicates distinguish.
type fixture struct {
	pool      *pgxpool.Pool
	workspace string
	user      string
	channel   string
	root      string // drive_folders.id
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := testDB(t)
	ctx := context.Background()

	f := &fixture{pool: pool}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	// The package shares one database, and the collector is global by design:
	// ListOrphans has no workspace filter, because an unowned file is unowned
	// whoever uploaded it. So a previous test's abandoned upload lands in this
	// one's RowsDeleted. Clearing the table is what makes the counts mean what
	// they say — a fixture that only isolated the workspace would produce
	// assertions that pass or fail depending on test order.
	_, err := pool.Exec(ctx, `TRUNCATE files CASCADE`)
	must(t, err)

	must(t, pool.QueryRow(ctx,
		`INSERT INTO users (email, username, password_hash, full_name)
		 VALUES ($1, $2, 'x', 'GC Test') RETURNING id`,
		"gc-"+suffix+"@test.local", "gc"+suffix).Scan(&f.user))

	must(t, pool.QueryRow(ctx,
		`INSERT INTO workspaces (name, slug, owner_id) VALUES ('GC', $1, $2) RETURNING id`,
		"gc-"+suffix, f.user).Scan(&f.workspace))

	must(t, pool.QueryRow(ctx,
		`INSERT INTO channels (workspace_id, name, slug, type, creator_id)
		 VALUES ($1, 'general', $2, 'public', $3) RETURNING id`,
		f.workspace, "general-"+suffix, f.user).Scan(&f.channel))

	// The workspace was created after 025 ran, so its root folder does not
	// exist yet — the migration backfilled the workspaces that existed then.
	// Creating it here is what workspace.Handler.Create will do.
	must(t, pool.QueryRow(ctx,
		`INSERT INTO drive_folders (workspace_id, name, is_root, created_by)
		 VALUES ($1, 'Drive', TRUE, $2) RETURNING id`,
		f.workspace, f.user).Scan(&f.root))

	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// insertFile writes a files row `age` old. folderID and messageID are optional;
// trashed marks it for the trash purge job rather than for the collector.
func (f *fixture) insertFile(t *testing.T, key string, age time.Duration, folderID, messageID *string, trashed bool) string {
	t.Helper()
	var id string
	must(t, f.pool.QueryRow(context.Background(),
		`INSERT INTO files (workspace_id, user_id, folder_id, message_id, name, content_type,
		                    size_bytes, storage_key, created_at, trashed_at)
		 VALUES ($1, $2, $3, $4, 'f.bin', 'application/octet-stream', 10, $5,
		         NOW() - $6::interval, CASE WHEN $7 THEN NOW() ELSE NULL END)
		 RETURNING id`,
		f.workspace, f.user, folderID, messageID, key,
		fmt.Sprintf("%d seconds", int(age.Seconds())), trashed,
	).Scan(&id))
	return id
}

func (f *fixture) insertMessage(t *testing.T) string {
	t.Helper()
	var id string
	must(t, f.pool.QueryRow(context.Background(),
		`INSERT INTO messages (channel_id, user_id, content)
		 VALUES ($1, $2, 'hi') RETURNING id`,
		f.channel, f.user).Scan(&id))
	return id
}

func (f *fixture) insertVersion(t *testing.T, fileID string, version int, key string) {
	t.Helper()
	_, err := f.pool.Exec(context.Background(),
		`INSERT INTO file_versions (file_id, version, storage_key, size_bytes, content_type, created_by)
		 VALUES ($1, $2, $3, 10, 'application/octet-stream', $4)`,
		fileID, version, key, f.user)
	must(t, err)
}

func (f *fixture) rowExists(t *testing.T, id string) bool {
	t.Helper()
	var n int
	must(t, f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM files WHERE id = $1`, id).Scan(&n))
	return n > 0
}

// sweepKey builds a storage key whose encoded day is old enough that the sweep
// will consider it. Collect derives age from the key, not from created_at.
func (f *fixture) sweepKey(name string) string {
	day := time.Now().UTC().Add(-30 * 24 * time.Hour)
	return fmt.Sprintf("%s/%s/%s", f.workspace, day.Format("2006/01/02"), name)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestOrphansAreOnlyTheUnowned is the ListOrphans half.
//
// A pillar that gives files a new owner adds a case here and a column to the
// predicate. It does not rewrite the query.
func TestOrphansAreOnlyTheUnowned(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	msg := f.insertMessage(t)
	old := 48 * time.Hour

	driveFile := f.insertFile(t, "k-drive", old, &f.root, nil, false)
	chatFile := f.insertFile(t, "k-chat", old, nil, &msg, false)
	bothFile := f.insertFile(t, "k-both", old, &f.root, &msg, false)
	trashedFile := f.insertFile(t, "k-trashed", old, &f.root, nil, true)
	abandoned := f.insertFile(t, "k-abandoned", old, nil, nil, false)
	inFlight := f.insertFile(t, "k-inflight", time.Minute, nil, nil, false)

	orphans, err := NewRepository(f.pool).ListOrphans(ctx, time.Now().Add(-GCGrace), 100)
	must(t, err)

	got := map[string]bool{}
	for _, o := range orphans {
		got[o.ID] = true
	}

	for _, tc := range []struct {
		name    string
		id      string
		orphan  bool
		because string
	}{
		{"drive file", driveFile, false,
			"a Drive file has no message and lives in its folder forever; collecting it is the data loss this predicate exists to prevent"},
		{"chat attachment", chatFile, false, "owned by its message"},
		{"drive file shared into a channel", bothFile, false, "owned twice over"},
		{"trashed file", trashedFile, false,
			"the trash purge job owns it from the moment the user trashed it; collecting it here races that job and skips its audit entry"},
		{"abandoned upload", abandoned, true,
			"owned by nothing and past the grace period — this is what the collector is for, and it must not regress in the other direction"},
		{"upload in flight", inFlight, false, "inside the grace period"},
	} {
		if got[tc.id] != tc.orphan {
			t.Errorf("%s: orphan = %v, want %v — %s", tc.name, got[tc.id], tc.orphan, tc.because)
		}
	}
}

// TestStorageKeysPresentCoversEveryReference is the bucket-sweep half.
//
// This is the quieter of the two predicates: it works from the bucket inwards,
// so a reference it does not know about is not merely missed — it is proof, to
// the sweeper, that the object is garbage.
func TestStorageKeysPresentCoversEveryReference(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	head := "ref-head"
	thumb := "ref-thumb"
	v1, v2 := "ref-v1", "ref-v2"

	id := f.insertFile(t, head, time.Hour, &f.root, nil, false)
	_, err := f.pool.Exec(ctx, `UPDATE files SET thumbnail_key = $1, current_version = 3 WHERE id = $2`, thumb, id)
	must(t, err)
	f.insertVersion(t, id, 1, v1)
	f.insertVersion(t, id, 2, v2)
	f.insertVersion(t, id, 3, head)

	present, err := NewRepository(f.pool).StorageKeysPresent(ctx,
		[]string{head, thumb, v1, v2, "ref-nobody"})
	must(t, err)

	for _, tc := range []struct {
		key     string
		want    bool
		because string
	}{
		{head, true, "files.storage_key"},
		{thumb, true, "files.thumbnail_key — a different object in the same bucket"},
		{v1, true, "file_versions: without this arm, uploading a second version marks the first one's object garbage and the next sweep deletes the history the versions UI is about to offer to restore"},
		{v2, true, "file_versions"},
		{"ref-nobody", false, "genuinely unreferenced — the sweep must still work"},
	} {
		if present[tc.key] != tc.want {
			t.Errorf("StorageKeysPresent[%q] = %v, want %v — %s", tc.key, present[tc.key], tc.want, tc.because)
		}
	}
}

// TestCollectLeavesDriveAlone drives the whole collector end to end, because
// the predicates being right is necessary and not sufficient: Collect is what
// turns a row into a DELETE.
func TestCollectLeavesDriveAlone(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	driveKey := f.sweepKey("drive.bin")
	versionKey := f.sweepKey("drive-v1.bin")
	abandonedKey := f.sweepKey("abandoned.bin")
	looseKey := f.sweepKey("no-row-at-all.bin")

	driveFile := f.insertFile(t, driveKey, 48*time.Hour, &f.root, nil, false)
	f.insertVersion(t, driveFile, 1, versionKey)
	f.insertVersion(t, driveFile, 2, driveKey)
	abandoned := f.insertFile(t, abandonedKey, 48*time.Hour, nil, nil, false)

	store := newMemStore(driveKey, versionKey, abandonedKey, looseKey)

	res, err := Collect(ctx, NewRepository(f.pool), store, CollectOptions{
		Now:         time.Now(),
		SweepPrefix: f.workspace[:1],
	}, discardLogger())
	must(t, err)

	if !store.has(driveKey) {
		t.Error("the Drive file's object was deleted 48 hours after upload — this is the bug the phase exists to prevent")
	}
	if !f.rowExists(t, driveFile) {
		t.Error("the Drive file's row was deleted")
	}
	if !store.has(versionKey) {
		t.Error("a non-head version object was swept; version history is gone")
	}
	if store.has(abandonedKey) {
		t.Error("an abandoned upload was not collected — the leak the collector exists for")
	}
	if f.rowExists(t, abandoned) {
		t.Error("an abandoned upload's row survived")
	}
	if store.has(looseKey) {
		t.Error("an object with no row at all was not swept")
	}
	if res.RowsDeleted != 1 {
		t.Errorf("RowsDeleted = %d, want exactly the one abandoned upload", res.RowsDeleted)
	}
	if res.ObjectsSwept != 1 {
		t.Errorf("ObjectsSwept = %d, want exactly the one object with no row", res.ObjectsSwept)
	}
}

// TestCollectKeepsTheRowWhenAnObjectFailsToDelete: a row is only dropped once
// every object it owns is gone. Dropping it early leaves an unreferenced object
// with no row left to find it by — and the bucket sweep will not help, because
// by then the key is not in any list this package produces.
func TestCollectKeepsTheRowWhenAnObjectFailsToDelete(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	key := f.sweepKey("wedged.bin")
	id := f.insertFile(t, key, 48*time.Hour, nil, nil, false)

	store := newMemStore(key)
	store.failOn[key] = true

	res, err := Collect(ctx, NewRepository(f.pool), store, CollectOptions{
		Now:         time.Now(),
		SweepPrefix: f.workspace[:1],
	}, discardLogger())
	if err != nil {
		t.Fatalf("one unlucky object must not wedge the whole run: %v", err)
	}
	if !f.rowExists(t, id) {
		t.Error("the row was deleted while its object survived; the object is now unfindable")
	}
	if res.DeleteFailures == 0 {
		t.Error("the failure was not counted, so nothing would ever notice a permanently wedged object")
	}
}

// TestGCPredicatesFailIfReverted proves the tests above are load-bearing.
//
// The plan asks for "a regression test that fails if the predicate is
// reverted". A test that merely exercises the current SQL would pass against
// the old predicate too if the fixtures happened to miss the case, so this runs
// the OLD queries against the SAME fixtures and asserts they get it wrong. If
// someone reverts internal/file to the pre-Drive predicates, the tests above go
// red — and this is the evidence for that claim rather than a hope.
func TestGCPredicatesFailIfReverted(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	driveFile := f.insertFile(t, "revert-drive", 48*time.Hour, &f.root, nil, false)
	id := f.insertFile(t, "revert-head", time.Hour, &f.root, nil, false)
	f.insertVersion(t, id, 1, "revert-v1")

	// The pre-Drive ListOrphans.
	var collected bool
	must(t, f.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM files
		                 WHERE message_id IS NULL AND created_at < $1 AND id = $2)`,
		time.Now().Add(-GCGrace), driveFile).Scan(&collected))
	if !collected {
		t.Error("the old ListOrphans predicate did NOT select the Drive file, so " +
			"TestOrphansAreOnlyTheUnowned would pass against a revert and proves nothing")
	}

	// The pre-Drive StorageKeysPresent.
	var referenced bool
	must(t, f.pool.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT storage_key   FROM files WHERE storage_key   = $1
		      UNION
		     SELECT thumbnail_key FROM files WHERE thumbnail_key = $1)`,
		"revert-v1").Scan(&referenced))
	if referenced {
		t.Error("the old StorageKeysPresent predicate DID find the version object, so " +
			"TestStorageKeysPresentCoversEveryReference would pass against a revert and proves nothing")
	}

	// THE MAIL BLOCK, and it is the one with the worst consequence.
	//
	// A raw RFC822 original lives in the same bucket with NO files row, so the
	// pre-mail StorageKeysPresent reports it unreferenced — and the sweep works
	// from the bucket inwards, so "unreferenced" is proof to the sweeper that
	// the object is garbage. Reverting that arm deletes every archived email an
	// hour after it arrives.
	rawKey := "revert-raw-" + uniqueSuffix()
	must(t, f.pool.QueryRow(ctx, `
		WITH d AS (
			INSERT INTO mail_domains (workspace_id, domain, verify_token)
			VALUES ($1, 'revert-' || substr(md5(random()::text), 1, 8) || '.test', 'tok')
			RETURNING id
		), mb AS (
			INSERT INTO mailboxes (workspace_id, domain_id, address, prefix)
			SELECT $1, d.id, 'revert-' || substr(md5(random()::text), 1, 8) || '@revert.test', 'REV'
			  FROM d RETURNING id
		), c AS (
			INSERT INTO mail_conversations (mailbox_id, workspace_id, number)
			SELECT mb.id, $1, 1 FROM mb RETURNING id
		)
		INSERT INTO mail_messages (conversation_id, direction, message_id, raw_key, sent_at)
		SELECT c.id, 'inbound', 'mid-' || $2, $2, NOW() FROM c
		RETURNING raw_key`, f.workspace, rawKey).Scan(&rawKey))

	must(t, f.pool.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT storage_key   FROM files         WHERE storage_key   = $1
		      UNION
		     SELECT thumbnail_key FROM files         WHERE thumbnail_key = $1
		      UNION
		     SELECT storage_key   FROM file_versions WHERE storage_key   = $1)`,
		rawKey).Scan(&referenced))
	if referenced {
		t.Error("the pre-mail StorageKeysPresent predicate found the raw email object, so " +
			"the arms added for it prove nothing")
	}

	// And the live query DOES find it — which is the half that matters.
	repo := NewRepository(f.pool)
	present, err := repo.StorageKeysPresent(ctx, []string{rawKey})
	must(t, err)
	if !present[rawKey] {
		t.Fatal("StorageKeysPresent does NOT name the raw email object; the bucket sweep " +
			"will delete every archived email an hour after it arrives")
	}

	// The mail-owner clause in ListOrphans, same shape. An attachment owned by a
	// mail message is not an orphan, and the pre-mail predicate says it is.
	// The UPDATE and the check are separate statements on purpose: a subquery
	// inside RETURNING is evaluated against the snapshot from the START of the
	// statement, so it would report the file's OLD ownership and this assertion
	// would pass for the wrong reason.
	_, err = f.pool.Exec(ctx, `
		UPDATE files
		   SET mail_message_id = (SELECT id FROM mail_messages WHERE raw_key = $2),
		       folder_id = NULL
		 WHERE id = $1`, driveFile, rawKey)
	must(t, err)

	var attachmentCollected bool
	must(t, f.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM files
		                 WHERE folder_id IS NULL AND message_id IS NULL
		                   AND trashed_at IS NULL AND created_at < NOW() AND id = $1)`,
		driveFile).Scan(&attachmentCollected))
	if !attachmentCollected {
		t.Error("the pre-mail ListOrphans predicate did NOT select the mail attachment, so " +
			"the mail_message_id clause proves nothing")
	}
	orphans, err := repo.ListOrphans(ctx, time.Now().Add(time.Hour), 100)
	must(t, err)
	for _, o := range orphans {
		if o.ID == driveFile {
			t.Fatal("ListOrphans selected a file owned by a mail message; every attachment " +
				"would be deleted an hour after it arrived")
		}
	}
}

// uniqueSuffix keeps fixtures from colliding across runs of the same suite
// against a database that persists between them.
func uniqueSuffix() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// EVERY COLUMN THAT HOLDS AN OBJECT KEY MUST BE NAMED IN StorageKeysPresent.
//
// This is the structural version of docs/plans/README.md ruling 2, and it exists
// because the human version has already failed twice: four separate plans each
// proposed rewriting one clause of that query, none aware of the others, and
// plan 08 stores raw RFC822 originals under a `raw_key` with no files row at all
// — which as things stand means the bucket sweep deletes every archived email.
//
// The sweep works from the bucket INWARDS: a reference this query does not know
// about is not merely missed, it is proof to the sweeper that the object is
// garbage. So "somebody remembers to add an arm" is not a control. This test is.
//
// It reads the live schema, finds every column whose name says it holds a key,
// and fails if the query does not mention its table. A pillar that adds one gets
// a red test naming the table rather than a support ticket about missing files.
func TestEveryStorageKeyColumnIsNamedInThePredicate(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	rows, err := pool.Query(ctx, `
		SELECT c.table_name, c.column_name
		  FROM information_schema.columns c
		  JOIN information_schema.tables t
		    ON t.table_schema = c.table_schema AND t.table_name = c.table_name
		 WHERE c.table_schema = 'public'
		   AND t.table_type = 'BASE TABLE'
		   AND c.data_type = 'text'
		   AND (c.column_name LIKE '%storage_key%'
		     OR c.column_name LIKE '%thumbnail_key%'
		     OR c.column_name LIKE 'raw_key'
		     OR c.column_name LIKE '%object_key%')
		 ORDER BY c.table_name, c.column_name`)
	must(t, err)
	defer rows.Close()

	type column struct{ table, name string }
	var found []column
	for rows.Next() {
		var c column
		must(t, rows.Scan(&c.table, &c.name))
		found = append(found, c)
	}
	must(t, rows.Err())

	if len(found) == 0 {
		t.Fatal("no storage-key columns found at all; this test is not testing anything")
	}

	// The predicate's own text, so the check tracks the query rather than a copy
	// of it. Reading the source is deliberate: asserting against a hand-written
	// list here would be the same second-opinion problem one level up.
	source, err := os.ReadFile("repository.go")
	must(t, err)
	predicate := string(source)
	start := strings.Index(predicate, "func (r *Repository) StorageKeysPresent")
	if start < 0 {
		t.Fatal("StorageKeysPresent not found in repository.go")
	}
	predicate = predicate[start:]

	for _, c := range found {
		if !strings.Contains(predicate, "FROM "+c.table) &&
			!strings.Contains(predicate, "FROM  "+c.table) {
			t.Errorf("%s.%s holds an object key and StorageKeysPresent never reads %s.\n\n"+
				"The bucket sweep deletes any object no row in that query names, so every "+
				"object this column points at would be deleted on the next run — silently, "+
				"by a job that logs success.\n\n"+
				"Add an arm to StorageKeysPresent and a case to "+
				"TestStorageKeysPresentCoversEveryReference in the same change.",
				c.table, c.name, c.table)
		}
	}
}
