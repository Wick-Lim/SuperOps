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

// Orphan is a file row whose object should be removed.
type Orphan struct {
	ID         string `json:"id"`
	StorageKey string `json:"storage_key"`
}

// ListOrphans returns unattached files older than cutoff. The cutoff exists so
// an upload that is mid-flight towards its message is never collected.
func (r *Repository) ListOrphans(ctx context.Context, cutoff time.Time, limit int) ([]Orphan, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := r.pool.Query(ctx,
		`SELECT id, storage_key FROM files
		  WHERE message_id IS NULL AND created_at < $1
		  ORDER BY created_at
		  LIMIT $2`, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("list orphan files: %w", err)
	}
	defer rows.Close()

	orphans := []Orphan{}
	for rows.Next() {
		var o Orphan
		if err := rows.Scan(&o.ID, &o.StorageKey); err != nil {
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

// StorageKeysPresent returns the subset of keys that still have a files row.
// Anything the sweeper listed from the bucket and that is absent from the
// result has no owner left in Postgres and can be removed.
func (r *Repository) StorageKeysPresent(ctx context.Context, keys []string) (map[string]bool, error) {
	present := map[string]bool{}
	if len(keys) == 0 {
		return present, nil
	}
	rows, err := r.pool.Query(ctx, `SELECT storage_key FROM files WHERE storage_key = ANY($1)`, keys)
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
