package file

import (
	"context"
	"log/slog"
	"time"
)

// Object-storage garbage collection.
//
// This lived in cmd/worker as an unexported function, which meant the only
// thing that could reach it was cmd/worker's own test binary — and so the
// predicate that decides whether a user's file is garbage had no test at all.
// It is here because this package owns both halves of the question: the key
// format is written by Handler.Upload and the ownership columns are read by
// Repository.
//
// The advisory lock that keeps two replicas from collecting at once stays in
// cmd/worker, where the pool and the lock-id registry live. Collect is the part
// worth testing and it is the part with no infrastructure of its own.

// GCDefaults are the collector's tuning values. They are exported so cmd/worker
// states them at the call site rather than duplicating the numbers.
const (
	// GCGrace is how long a file may sit unowned before it is collectible. An
	// upload is unowned from the moment it lands until its message is sent or
	// its Drive row is written, and that window is a user typing.
	GCGrace = 24 * time.Hour

	// GCKeyDateSlack is how much a storage key's date can understate the age of
	// the object under it.
	//
	// The key carries a date, not an instant, so an object under .../2026/07/25/
	// may have been written at any point during that day — the youngest it can
	// possibly be is the END of it. Comparing the start of the day against the
	// cutoff collapsed the grace period to nothing: an object written at
	// 23:59:59 became eligible two seconds later.
	//
	// The rest covers the zone. Handler.Upload formats the key with time.Now()
	// in local time while objectDay parses it as UTC, so the day can sit up to
	// fourteen hours either side of where it looks. Two days makes "at least
	// GCGrace old" hold in any deployment timezone, at the price of collecting a
	// leak three days after upload instead of one. For a leak collector that is
	// the right trade.
	GCKeyDateSlack = 48 * time.Hour

	GCRowBatch = 500
	GCKeyBatch = 1000
)

// GCPrefixes shard the bucket sweep.
//
// ListKeys has no continuation token, so an unprefixed listing returns the same
// first N keys every run and never reaches anything past them. Storage keys
// start with the workspace uuid, so sweeping one hex prefix per run covers the
// whole bucket every sixteen runs.
var GCPrefixes = []string{
	"0", "1", "2", "3", "4", "5", "6", "7",
	"8", "9", "a", "b", "c", "d", "e", "f",
}

// ObjectStore is the part of Storage the collector uses. An interface so a test
// can hand Collect a bucket it controls, and so nothing here can reach for the
// MinIO client and start uploading.
type ObjectStore interface {
	Delete(ctx context.Context, key string) error
	ListKeys(ctx context.Context, prefix string, limit int) ([]string, error)
}

// CollectOptions is one run's parameters. The zero value is not usable: Now and
// SweepPrefix have no sensible default and defaulting them would hide a caller
// that forgot to rotate the prefix, which is a sweep that examines one
// sixteenth of the bucket forever.
type CollectOptions struct {
	// Now anchors the cutoff. A parameter rather than time.Now() so a test can
	// place a file on either side of the grace period without sleeping.
	Now time.Time

	// SweepPrefix is the bucket shard to walk this run; see GCPrefixes.
	// Empty means walk everything, which is only right for a small test bucket.
	SweepPrefix string

	// Grace, RowBatch and KeyBatch fall back to the GC* constants when zero.
	Grace    time.Duration
	RowBatch int
	KeyBatch int
}

func (o CollectOptions) grace() time.Duration {
	if o.Grace > 0 {
		return o.Grace
	}
	return GCGrace
}

func (o CollectOptions) rowBatch() int {
	if o.RowBatch > 0 {
		return o.RowBatch
	}
	return GCRowBatch
}

func (o CollectOptions) keyBatch() int {
	if o.KeyBatch > 0 {
		return o.KeyBatch
	}
	return GCKeyBatch
}

// CollectResult reports what one run did. Returned rather than only logged so a
// test can assert on it and so the worker can decide what is worth an Info.
type CollectResult struct {
	// RowsDeleted is files rows removed after every object they owned was gone.
	RowsDeleted int64
	// ObjectsSwept is objects removed because nothing in Postgres named them.
	ObjectsSwept int
	// DeleteFailures is objects whose removal errored. Their rows are left
	// alone, so the next run retries instead of leaking the remainder forever.
	DeleteFailures int
}

// Collect removes storage objects that nothing points at any more.
//
// Two independent leaks, and they need different evidence:
//
//	(i)  a files row that nothing owns — the upload was abandoned, or its
//	     message was hard-deleted (files.message_id is ON DELETE SET NULL).
//	     Visible in Postgres; answered by ListOrphans.
//	(ii) an object whose row is gone entirely — a workspace deletion cascades
//	     the rows away and leaves every object behind. Invisible in Postgres;
//	     only findable by walking the bucket.
//
// A partial failure is never fatal. Both halves run, errors on individual
// objects are counted and logged, and the run returns an error only when
// Postgres or the bucket listing itself fails — because the alternative is a
// single unlucky object wedging the collector forever.
func Collect(
	ctx context.Context,
	repo *Repository,
	store ObjectStore,
	opts CollectOptions,
	l *slog.Logger,
) (CollectResult, error) {
	var res CollectResult
	if l == nil {
		l = slog.Default()
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	cutoff := now.Add(-opts.grace())

	// (i) Rows nothing owns, past the grace period.
	orphans, err := repo.ListOrphans(ctx, cutoff, opts.rowBatch())
	if err != nil {
		return res, err
	}
	removed := make([]string, 0, len(orphans))
	for _, o := range orphans {
		// A row owns its thumbnail as well as its main object. The row is only
		// dropped once every object it owns is gone, so a partial failure is
		// retried next run rather than leaving an unreferenced object with no
		// row left to find it by.
		ok := true
		for _, key := range o.Keys() {
			if err := store.Delete(ctx, key); err != nil {
				l.Warn("object gc: delete object", "key", key, "error", err)
				res.DeleteFailures++
				ok = false
			}
		}
		if ok {
			removed = append(removed, o.ID)
		}
	}
	if n, err := repo.DeleteByIDs(ctx, removed); err != nil {
		return res, err
	} else if n > 0 {
		res.RowsDeleted = n
		l.Info("object gc: removed unowned files", "count", n)
	}

	// (ii) Objects with no reference at all.
	keys, err := store.ListKeys(ctx, opts.SweepPrefix, opts.keyBatch())
	if err != nil {
		return res, err
	}
	// Only consider objects old enough that a concurrent upload cannot be
	// mid-flight between the PUT and its INSERT. Anything whose key does not
	// carry a parseable date is left alone rather than guessed at.
	candidates := make([]string, 0, len(keys))
	for _, k := range keys {
		if olderThan(k, cutoff) {
			candidates = append(candidates, k)
		}
	}
	present, err := repo.StorageKeysPresent(ctx, candidates)
	if err != nil {
		return res, err
	}
	for _, k := range candidates {
		if present[k] {
			continue
		}
		if err := store.Delete(ctx, k); err != nil {
			l.Warn("object gc: delete unreferenced object", "key", k, "error", err)
			res.DeleteFailures++
			continue
		}
		res.ObjectsSwept++
	}
	if res.ObjectsSwept > 0 {
		l.Info("object gc: removed unreferenced objects", "count", res.ObjectsSwept)
	}
	return res, nil
}

// olderThan reports whether an object can be proven, from its key alone, to
// have been uploaded before cutoff. Unparseable means "no" — this guard exists
// to stop the sweep from deleting an upload whose row is not committed yet, and
// it has to be conservative in that direction.
func olderThan(key string, cutoff time.Time) bool {
	day, ok := objectDay(key)
	if !ok {
		return false
	}
	return day.Add(GCKeyDateSlack).Before(cutoff)
}

// objectDay parses the upload date out of a storage key of the shape
// {workspace_id}/YYYY/MM/DD/{file_id}{ext}, written by Handler.Upload. The
// returned time is midnight UTC at the start of that day.
func objectDay(key string) (time.Time, bool) {
	parts := splitN(key, '/', 5)
	if len(parts) < 5 {
		return time.Time{}, false
	}
	day, err := time.Parse("2006/01/02", parts[1]+"/"+parts[2]+"/"+parts[3])
	if err != nil {
		return time.Time{}, false
	}
	return day, true
}

// splitN splits on sep into at most n fields, the last of which keeps the rest.
func splitN(s string, sep byte, n int) []string {
	out := make([]string, 0, n)
	start := 0
	for i := 0; i < len(s) && len(out) < n-1; i++ {
		if s[i] == sep {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
