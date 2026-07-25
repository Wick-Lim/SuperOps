// Package quota owns workspace_storage and workspaces.storage_quota_bytes.
//
// It exists as its own package because four call sites move blob bytes — the
// Drive upload, the chat upload, a new file version, and every delete path
// including the object collector — and a quota that any one of them can bypass
// is not enforcement. Concentrating the arithmetic here is what makes "is this
// path charged?" a question with an answer.
//
// INVARIANT I1, established by migration 026:
//
//	workspace_storage.bytes_used
//	  = SUM(file_versions.size_bytes) over every version row whose file belongs
//	    to the workspace.
//
// Every function here preserves it. Recompute re-derives it from scratch and is
// what proves the incremental arithmetic has not drifted.
//
// Trashed files and old versions COUNT. That is the point of a quota (plan 02
// §3): a trashed file is still an object in a bucket somebody is paying for,
// and it is cheaper to say so in the usage endpoint than to explain it later.
package quota

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrExceeded is the refusal. It maps to 507 Insufficient Storage, which is the
// one status that says "the request was fine, this account is full" — 403 reads
// as a permission problem and 413 as a request that was too big for the server.
var ErrExceeded = errors.New("quota: workspace storage quota exceeded")

// Querier is satisfied by *pgxpool.Pool and by pgx.Tx alike.
//
// The read helpers take it so a caller can read inside its own transaction
// without a second connection; the write helpers take pgx.Tx specifically,
// because a charge that is not in the same transaction as the row it is charging
// for is a counter that drifts on every failure.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Usage is what an operator and a client are shown.
//
// BytesUsed and CollabBytes are separate fields with separate timestamps and
// there is deliberately no Total: one is exact at every instant an upload can
// observe it and the other is recomputed by a job, and adding them produces a
// number whose accuracy nobody can state.
type Usage struct {
	WorkspaceID string `json:"workspace_id"`

	// QuotaBytes is 0 for unlimited, which is what every workspace has until an
	// admin sets one.
	QuotaBytes int64 `json:"quota_bytes"`

	// BytesUsed is exact and transactional: no upload can commit without it
	// having been updated in the same transaction.
	BytesUsed int64      `json:"bytes_used"`
	UpdatedAt *time.Time `json:"bytes_used_at"`

	// CollabBytes is the CRDT logs and snapshots, recomputed by a job. Nil
	// CollabBytesAt means it has never been computed — which is different from
	// zero, and a client that could not tell them apart would report an empty
	// Drive as accurate.
	CollabBytes   int64      `json:"collab_bytes"`
	CollabBytesAt *time.Time `json:"collab_bytes_at"`
}

// Available reports how many more bytes may be stored, or -1 for unlimited.
func (u Usage) Available() int64 {
	if u.QuotaBytes == 0 {
		return -1
	}
	if free := u.QuotaBytes - u.BytesUsed; free > 0 {
		return free
	}
	return 0
}

// Breakdown answers the support question a usage number always provokes: "we
// deleted everything, why is it still full?"
type Breakdown struct {
	LiveBytes    int64 `json:"live_bytes"`
	TrashedBytes int64 `json:"trashed_bytes"`
	// VersionBytes is the non-head versions — the half nobody expects.
	VersionBytes int64 `json:"version_bytes"`
	// RecomputedBytes is I1 evaluated right now. It should equal BytesUsed;
	// DriftBytes is the difference, and a non-zero value is a bug in the
	// incremental arithmetic, reported rather than silently corrected.
	RecomputedBytes int64 `json:"recomputed_bytes"`
	DriftBytes      int64 `json:"drift_bytes"`
}

// ChargeTx adds delta bytes and refuses to exceed the quota.
//
// ONE STATEMENT, and that is the entire design. A check-then-insert races: two
// uploads that each read 900 of a 1000 quota both decide they fit, and the
// workspace lands at 1100. Here the second transaction blocks on the first's row
// lock, re-evaluates against its COMMITTED value, and returns no rows.
//
// The INSERT arm carries the same predicate as the UPDATE arm, which is not
// belt and braces: a workspace with no workspace_storage row yet would otherwise
// have its FIRST upload be unconditionally free, however large.
//
// Returns the new total, or ErrExceeded.
func ChargeTx(ctx context.Context, tx pgx.Tx, workspaceID string, delta int64) (int64, error) {
	if delta < 0 {
		return 0, fmt.Errorf("quota: ChargeTx called with a negative delta (%d); use RefundTx", delta)
	}
	if delta == 0 {
		u, err := Read(ctx, tx, workspaceID)
		return u.BytesUsed, err
	}

	var total int64
	err := tx.QueryRow(ctx, `
		INSERT INTO workspace_storage AS ws (workspace_id, bytes_used, updated_at)
		SELECT w.id, $2::BIGINT, NOW()
		  FROM workspaces w
		 WHERE w.id = $1::UUID
		   AND (w.storage_quota_bytes = 0 OR $2::BIGINT <= w.storage_quota_bytes)
		    ON CONFLICT (workspace_id) DO UPDATE
		   SET bytes_used = ws.bytes_used + EXCLUDED.bytes_used,
		       updated_at = NOW()
		 WHERE (SELECT w2.storage_quota_bytes FROM workspaces w2 WHERE w2.id = ws.workspace_id) = 0
		    OR ws.bytes_used + EXCLUDED.bytes_used
		       <= (SELECT w2.storage_quota_bytes FROM workspaces w2 WHERE w2.id = ws.workspace_id)
		RETURNING ws.bytes_used`,
		workspaceID, delta).Scan(&total)
	if errors.Is(err, pgx.ErrNoRows) {
		// No row means one of: over quota, or the workspace does not exist. The
		// caller has already resolved the workspace to get here, so it is the
		// first — and answering "not found" for a full account would be a
		// misleading 404.
		return 0, ErrExceeded
	}
	if err != nil {
		return 0, fmt.Errorf("charge storage quota: %w", err)
	}
	return total, nil
}

// RefundTx returns bytes. It never fails on "too much": the CHECK on
// bytes_used clamps at zero rather than rejecting, because a refund that errors
// leaves the caller holding a deleted object and a counter that still charges
// for it — and of the two wrong numbers, the one that under-charges is the one
// that does not lock a customer out of their own Drive.
func RefundTx(ctx context.Context, tx pgx.Tx, workspaceID string, delta int64) error {
	if delta <= 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workspace_storage
		   SET bytes_used = GREATEST(0, bytes_used - $2::BIGINT), updated_at = NOW()
		 WHERE workspace_id = $1`, workspaceID, delta); err != nil {
		return fmt.Errorf("refund storage quota: %w", err)
	}
	return nil
}

// RefundForFilesTx returns every byte owned by the given files, per workspace,
// and is the shape every delete path wants: the caller knows file ids, not byte
// counts, and computing the sum outside the transaction that deletes the rows
// would charge or refund against a total that moved underneath it.
//
// It sums file_versions, so it refunds the whole history rather than just the
// head. Call it BEFORE deleting the rows — file_versions is ON DELETE CASCADE
// from files, so afterwards there is nothing left to sum.
func RefundForFilesTx(ctx context.Context, tx pgx.Tx, fileIDs []string) error {
	if len(fileIDs) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workspace_storage ws
		   SET bytes_used = GREATEST(0, ws.bytes_used - owed.bytes), updated_at = NOW()
		  FROM (SELECT f.workspace_id, SUM(v.size_bytes) AS bytes
		          FROM files f JOIN file_versions v ON v.file_id = f.id
		         WHERE f.id = ANY($1)
		         GROUP BY f.workspace_id) AS owed
		 WHERE ws.workspace_id = owed.workspace_id`, fileIDs); err != nil {
		return fmt.Errorf("refund storage for files: %w", err)
	}
	return nil
}

// Read returns the counters. A workspace with no row reads as zero rather than
// as an error: the row is created by the first charge, and "no row" and "no
// bytes" are the same fact.
func Read(ctx context.Context, q Querier, workspaceID string) (Usage, error) {
	u := Usage{WorkspaceID: workspaceID}
	err := q.QueryRow(ctx, `
		SELECT w.storage_quota_bytes,
		       COALESCE(ws.bytes_used, 0), COALESCE(ws.collab_bytes, 0),
		       ws.updated_at, ws.collab_bytes_at
		  FROM workspaces w
		  LEFT JOIN workspace_storage ws ON ws.workspace_id = w.id
		 WHERE w.id = $1`, workspaceID).
		Scan(&u.QuotaBytes, &u.BytesUsed, &u.CollabBytes, &u.UpdatedAt, &u.CollabBytesAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Usage{}, fmt.Errorf("quota: workspace %s does not exist", workspaceID)
	}
	if err != nil {
		return Usage{}, fmt.Errorf("read storage usage: %w", err)
	}
	return u, nil
}

// ReadBreakdown splits the number and reports drift against I1.
func ReadBreakdown(ctx context.Context, q Querier, workspaceID string) (Breakdown, error) {
	var b Breakdown
	err := q.QueryRow(ctx, `
		SELECT
		  COALESCE(SUM(v.size_bytes) FILTER (WHERE f.trashed_at IS NULL
		                                       AND v.version = f.current_version), 0) AS live,
		  COALESCE(SUM(v.size_bytes) FILTER (WHERE f.trashed_at IS NOT NULL), 0)       AS trashed,
		  COALESCE(SUM(v.size_bytes) FILTER (WHERE f.trashed_at IS NULL
		                                       AND v.version <> f.current_version), 0) AS versions,
		  COALESCE(SUM(v.size_bytes), 0)                                               AS total
		  FROM files f JOIN file_versions v ON v.file_id = f.id
		 WHERE f.workspace_id = $1`, workspaceID).
		Scan(&b.LiveBytes, &b.TrashedBytes, &b.VersionBytes, &b.RecomputedBytes)
	if err != nil {
		return Breakdown{}, fmt.Errorf("read storage breakdown: %w", err)
	}

	stored, err := Read(ctx, q, workspaceID)
	if err != nil {
		return Breakdown{}, err
	}
	b.DriftBytes = stored.BytesUsed - b.RecomputedBytes
	return b, nil
}

// SetQuota sets the cap. 0 is unlimited.
//
// It does NOT refuse a quota below current usage. An operator lowering the cap
// on an over-full workspace is doing exactly what they meant to: new uploads
// stop, nothing existing is deleted, and the usage endpoint shows why. Refusing
// would mean the only way to shrink a workspace's allowance is to delete its
// data first.
func SetQuota(ctx context.Context, q Querier, workspaceID string, bytes int64) error {
	if bytes < 0 {
		return fmt.Errorf("quota: a negative quota (%d) is not meaningful; 0 means unlimited", bytes)
	}
	tag, err := q.Exec(ctx,
		`UPDATE workspaces SET storage_quota_bytes = $2 WHERE id = $1`, workspaceID, bytes)
	if err != nil {
		return fmt.Errorf("set storage quota: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("quota: workspace %s does not exist", workspaceID)
	}
	return nil
}

// Recompute re-derives bytes_used from I1 for one workspace and reports what it
// changed.
//
// This is the counterpart to the incremental arithmetic, and it reports rather
// than blocks: drift here is a capacity and billing bug, not an access-control
// one, so the right response is a number in a log an operator can act on — the
// same posture internal/authz takes for acl_key drift.
func Recompute(ctx context.Context, q Querier, workspaceID string) (before, after int64, err error) {
	err = q.QueryRow(ctx, `
		WITH truth AS (
		    SELECT COALESCE(SUM(v.size_bytes), 0) AS bytes
		      FROM files f JOIN file_versions v ON v.file_id = f.id
		     WHERE f.workspace_id = $1
		), prev AS (
		    SELECT COALESCE(bytes_used, 0) AS bytes FROM workspace_storage WHERE workspace_id = $1
		)
		INSERT INTO workspace_storage (workspace_id, bytes_used, updated_at)
		SELECT $1, truth.bytes, NOW() FROM truth
		    ON CONFLICT (workspace_id) DO UPDATE
		   SET bytes_used = EXCLUDED.bytes_used, updated_at = NOW()
		RETURNING COALESCE((SELECT bytes FROM prev), 0), workspace_storage.bytes_used`,
		workspaceID).Scan(&before, &after)
	if err != nil {
		return 0, 0, fmt.Errorf("recompute storage usage: %w", err)
	}
	return before, after, nil
}
