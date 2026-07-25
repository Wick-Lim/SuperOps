package file

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository serves the object-storage garbage collector.
//
// Orphaned MinIO objects accumulate from three directions: an upload that is
// never attached to a message, a message deletion (files.message_id is
// ON DELETE SET NULL, so the row survives and the object with it), and a
// workspace deletion (which cascades the rows away and leaves every object).
// The first two are visible in Postgres and answered by ListOrphans; the third
// is only visible by walking the bucket, which is what StorageKeysPresent is
// for.
type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// Orphan is a file row whose objects should be removed.
//
// A row owns up to two objects. ThumbnailKey is empty for the rows this tree
// writes today — nothing populates files.thumbnail_key yet — but it is part of
// the schema, and a collector that only knew about storage_key would leak the
// thumbnail of every row it deleted the moment something did.
type Orphan struct {
	ID           string `json:"id"`
	StorageKey   string `json:"storage_key"`
	ThumbnailKey string `json:"thumbnail_key,omitempty"`
}

// Keys returns the object keys this row owns, skipping the empty ones.
func (o Orphan) Keys() []string {
	keys := make([]string, 0, 2)
	if o.StorageKey != "" {
		keys = append(keys, o.StorageKey)
	}
	if o.ThumbnailKey != "" && o.ThumbnailKey != o.StorageKey {
		keys = append(keys, o.ThumbnailKey)
	}
	return keys
}

// ListOrphans returns files that nothing owns, older than cutoff. The cutoff
// exists so an upload that is mid-flight towards its message is never
// collected.
//
// THE PREDICATE BELOW IS THE ONE THAT DELETES CUSTOMER DATA IF IT IS WRONG, and
// it has exactly one correct form: every ownership column, named together.
//
// It used to be `message_id IS NULL`, from a time when a message attachment was
// the only kind of file there was. A Drive file has no message, so under that
// predicate every file a user uploaded to Drive was deleted 24 hours later by a
// job that logged it as success (docs/plans/02-drive.md §2).
//
// docs/plans/README.md ruling 2 makes this the single owner of the predicate:
// four separate plans each proposed rewriting one clause of it, none aware of
// the others, and a missed clause is not a bug report. A pillar that gives
// files a new owner adds ONE column here and ONE case to TestOrphansAreOnlyThe
// Unowned — it does not rewrite the query.
//
// Ownership today:
//
//	folder_id  — a Drive file. Its owner is a folder, and it may live there
//	             forever without ever being attached to anything.
//	message_id — a chat attachment.
//	trashed_at — trashed, which means the user asked for it to go away through
//	             the trash, and the trash purge job owns it from that moment.
//	             Collecting it here would race that job and skip its audit
//	             entry.
//
// The direction of the risk is asymmetric: an over-narrow predicate leaks
// objects, which costs storage; an over-broad one deletes user data. When a new
// owner is uncertain, exclude it.
func (r *Repository) ListOrphans(ctx context.Context, cutoff time.Time, limit int) ([]Orphan, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := r.pool.Query(ctx,
		`SELECT id, storage_key, COALESCE(thumbnail_key, '') FROM files
		  WHERE folder_id  IS NULL
		    AND message_id IS NULL
		    AND trashed_at IS NULL
		    AND created_at < $1
		  ORDER BY created_at
		  LIMIT $2`, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("list orphan files: %w", err)
	}
	defer rows.Close()

	orphans := []Orphan{}
	for rows.Next() {
		var o Orphan
		if err := rows.Scan(&o.ID, &o.StorageKey, &o.ThumbnailKey); err != nil {
			return nil, fmt.Errorf("scan orphan file: %w", err)
		}
		orphans = append(orphans, o)
	}
	return orphans, rows.Err()
}

// DeleteByIDs removes file rows once their objects are gone.
func (r *Repository) DeleteByIDs(ctx context.Context, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tag, err := r.pool.Exec(ctx, `DELETE FROM files WHERE id = ANY($1)`, ids)
	if err != nil {
		return 0, fmt.Errorf("delete files: %w", err)
	}
	return tag.RowsAffected(), nil
}

// StorageKeysPresent returns the subset of keys that are still referenced from
// Postgres. Anything the sweeper listed from the bucket and that is absent from
// the result has no owner left and can be removed.
//
// THE SECOND PREDICATE THAT DELETES CUSTOMER DATA IF IT IS WRONG, and it is the
// quieter of the two: it works from the bucket inwards, so a reference this
// query does not know about is not merely missed — it is proof, to the sweeper,
// that the object is garbage.
//
// One arm per table that can name an object key. Adding a table that stores a
// key and forgetting to add an arm here deletes every object that table owns:
//
//	files.storage_key    — the head object.
//	files.thumbnail_key  — a different object in the same bucket. A lookup that
//	                       only knew about storage_key would report a live
//	                       thumbnail as unreferenced. (`= ANY(...)` never
//	                       matches NULL, so rows without one contribute
//	                       nothing.)
//	file_versions        — every non-head version. Without this arm, uploading
//	                       a second version of a file marks the first one's
//	                       object garbage and the next sweep deletes the history
//	                       the versions UI is about to offer to restore.
//
// docs/plans/README.md ruling 2 flags the next omission in advance: plan 08
// stores raw RFC822 originals under a key with no files row at all. As written
// this query would sweep every archived email. That arm belongs to plan 08 and
// goes here, with a case in TestStorageKeysPresentCoversEveryReference.
func (r *Repository) StorageKeysPresent(ctx context.Context, keys []string) (map[string]bool, error) {
	present := map[string]bool{}
	if len(keys) == 0 {
		return present, nil
	}
	rows, err := r.pool.Query(ctx,
		`SELECT storage_key   FROM files         WHERE storage_key   = ANY($1)
		  UNION
		 SELECT thumbnail_key FROM files         WHERE thumbnail_key = ANY($1)
		  UNION
		 SELECT storage_key   FROM file_versions WHERE storage_key   = ANY($1)`, keys)
	if err != nil {
		return nil, fmt.Errorf("check storage keys: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("scan storage key: %w", err)
		}
		present[k] = true
	}
	return present, rows.Err()
}
