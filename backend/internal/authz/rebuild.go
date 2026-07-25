package authz

// Backfill and drift detection for the object-permission materialization.
//
// acl_object and acl_key are derived state, and the thing they are derived from
// (workspaces, channels, channel_members, files, acl_grant) is still the state
// the product actually enforces against. Two jobs follow from that:
//
//   - Rebuild reconciles the derived tables with the source. It is the step-2
//     backfill, and it is re-runnable: running it twice changes nothing, and
//     running it after a hundred channels were created catches all hundred.
//   - Verify recomputes and REPORTS. It does not repair. A verifier that
//     silently repaired would hide the bug that caused the drift, and the bug
//     that causes drift in an authorization table is the one worth finding.
//
// Both answer from the acl_object_expected / acl_key_expected views created in
// migrations/016. That is deliberate and is the only reason the verifier can be
// trusted: a second, hand-written definition of "expected" in Go would make
// every disagreement between the two implementations look like production
// drift.

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
)

// RebuildStats reports what a reconcile changed. All-zero means the
// materialization was already correct, which is the steady state.
type RebuildStats struct {
	ObjectsUpserted int64
	ObjectsDeleted  int64
	KeysDeleted     int64
	KeysInserted    int64
}

// Clean reports whether the reconcile found nothing to change — the steady
// state once the backfill has run and nothing has been created since.
func (s RebuildStats) Clean() bool {
	return s.ObjectsUpserted == 0 && s.ObjectsDeleted == 0 && s.KeysDeleted == 0 && s.KeysInserted == 0
}

// Rebuild reconciles acl_object and acl_key with the legacy schema, in one
// transaction.
//
// Scope is deliberately asymmetric. Objects are reconciled only for the three
// DERIVED types — deleting an ACL-native folder because it is absent from a
// view that never described folders would be catastrophic — while keys are
// reconciled in full, because acl_key_expected is the complete definition of
// acl_key for every object type including ACL-native ones.
func (c *Checker) Rebuild(ctx context.Context) (RebuildStats, error) {
	var stats RebuildStats

	err := database.WithTx(ctx, c.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO acl_object (object_type, object_id, workspace_id, path)
			SELECT object_type, object_id, workspace_id, path FROM acl_object_expected
			    ON CONFLICT (object_type, object_id) DO UPDATE
			   SET workspace_id = EXCLUDED.workspace_id,
			       path         = EXCLUDED.path,
			       updated_at   = NOW()
			 WHERE acl_object.workspace_id IS DISTINCT FROM EXCLUDED.workspace_id
			    OR acl_object.path         IS DISTINCT FROM EXCLUDED.path`)
		if err != nil {
			return fmt.Errorf("backfill acl_object: %w", err)
		}
		stats.ObjectsUpserted = tag.RowsAffected()

		// A deleted channel leaves its acl_object row behind: object_id is
		// polymorphic, so there is no foreign key to cascade from. Dropping the
		// row here cascades acl_grant and acl_key with it.
		tag, err = tx.Exec(ctx, `
			DELETE FROM acl_object o
			 WHERE o.object_type = ANY($1)
			   AND NOT EXISTS (
			       SELECT 1 FROM acl_object_expected e
			        WHERE e.object_type = o.object_type AND e.object_id = o.object_id)`,
			derivedTypeList())
		if err != nil {
			return fmt.Errorf("prune stale acl_object rows: %w", err)
		}
		stats.ObjectsDeleted = tag.RowsAffected()

		tag, err = tx.Exec(ctx, `
			DELETE FROM acl_key k
			 WHERE NOT EXISTS (
			     SELECT 1 FROM acl_key_expected e
			      WHERE e.object_type = k.object_type AND e.object_id = k.object_id AND e.key = k.key)`)
		if err != nil {
			return fmt.Errorf("prune stale acl_key rows: %w", err)
		}
		stats.KeysDeleted = tag.RowsAffected()

		tag, err = tx.Exec(ctx, `
			INSERT INTO acl_key (object_type, object_id, key)
			SELECT object_type, object_id, key FROM acl_key_expected
			    ON CONFLICT DO NOTHING`)
		if err != nil {
			return fmt.Errorf("materialize acl_key rows: %w", err)
		}
		stats.KeysInserted = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return RebuildStats{}, err
	}
	return stats, nil
}

func derivedTypeList() []string {
	out := make([]string, 0, len(derivedTypes))
	for t := range derivedTypes {
		out = append(out, t)
	}
	return out
}

// ---------------------------------------------------------------------------
// Drift
// ---------------------------------------------------------------------------

// DriftSample is one concrete disagreement, named precisely enough to go
// looking for it. "3 keys missing" is a metric; "key u-<uuid> missing from
// file:<uuid>" is something somebody can debug.
type DriftSample struct {
	Kind       string // missing_key | extra_key | missing_object | misplaced_object | extra_object
	ObjectType string
	ObjectID   string
	Detail     string
}

func (s DriftSample) String() string {
	return fmt.Sprintf("%s %s:%s %s", s.Kind, s.ObjectType, s.ObjectID, s.Detail)
}

// DriftReport is the verifier's answer.
type DriftReport struct {
	MissingKeys      int64
	ExtraKeys        int64
	MissingObjects   int64
	MisplacedObjects int64
	ExtraObjects     int64

	// Samples is bounded; the counts above are not.
	Samples []DriftSample
}

// Clean reports whether the materialization matches its definition exactly.
func (r DriftReport) Clean() bool {
	return r.MissingKeys == 0 && r.ExtraKeys == 0 &&
		r.MissingObjects == 0 && r.MisplacedObjects == 0 && r.ExtraObjects == 0
}

func (r DriftReport) String() string {
	return fmt.Sprintf(
		"acl drift: keys missing=%d extra=%d; objects missing=%d misplaced=%d extra=%d",
		r.MissingKeys, r.ExtraKeys, r.MissingObjects, r.MisplacedObjects, r.ExtraObjects)
}

// Verify recomputes the materialization and reports every way it disagrees with
// its definition. It never writes.
//
// The five categories are not the same failure and must not be collapsed:
//
//	missing_key       somebody cannot see something they should — an outage
//	extra_key         somebody CAN see something they should not — a leak
//	missing_object    an object nothing has registered; its keys are absent
//	                  entirely, so it is invisible rather than exposed
//	misplaced_object  a stored path disagrees with the derived one; its whole
//	                  subtree inherits the wrong keys
//	extra_object      a row whose source is gone
//
// sampleLimit bounds how many concrete examples are collected per category.
func (c *Checker) Verify(ctx context.Context, sampleLimit int) (DriftReport, error) {
	if sampleLimit < 0 {
		sampleLimit = 0
	}
	var r DriftReport

	counts := []struct {
		into *int64
		sql  string
		args []any
	}{
		{&r.MissingKeys, `
			SELECT COUNT(*) FROM acl_key_expected e
			 WHERE NOT EXISTS (SELECT 1 FROM acl_key k
			        WHERE k.object_type = e.object_type AND k.object_id = e.object_id AND k.key = e.key)`, nil},
		{&r.ExtraKeys, `
			SELECT COUNT(*) FROM acl_key k
			 WHERE NOT EXISTS (SELECT 1 FROM acl_key_expected e
			        WHERE e.object_type = k.object_type AND e.object_id = k.object_id AND e.key = k.key)`, nil},
		{&r.MissingObjects, `
			SELECT COUNT(*) FROM acl_object_expected e
			 WHERE NOT EXISTS (SELECT 1 FROM acl_object o
			        WHERE o.object_type = e.object_type AND o.object_id = e.object_id)`, nil},
		{&r.MisplacedObjects, `
			SELECT COUNT(*) FROM acl_object_expected e
			  JOIN acl_object o ON o.object_type = e.object_type AND o.object_id = e.object_id
			 WHERE o.path IS DISTINCT FROM e.path OR o.workspace_id IS DISTINCT FROM e.workspace_id`, nil},
		{&r.ExtraObjects, `
			SELECT COUNT(*) FROM acl_object o
			 WHERE o.object_type = ANY($1)
			   AND NOT EXISTS (SELECT 1 FROM acl_object_expected e
			        WHERE e.object_type = o.object_type AND e.object_id = o.object_id)`,
			[]any{derivedTypeList()}},
	}
	for _, q := range counts {
		if err := c.pool.QueryRow(ctx, q.sql, q.args...).Scan(q.into); err != nil {
			return DriftReport{}, fmt.Errorf("count acl drift: %w", err)
		}
	}

	if r.Clean() || sampleLimit == 0 {
		return r, nil
	}

	samples := []struct {
		kind string
		sql  string
		args []any
	}{
		{"missing_key", `
			SELECT e.object_type, e.object_id::text, e.key FROM acl_key_expected e
			 WHERE NOT EXISTS (SELECT 1 FROM acl_key k
			        WHERE k.object_type = e.object_type AND k.object_id = e.object_id AND k.key = e.key)
			 LIMIT $1`, []any{sampleLimit}},
		{"extra_key", `
			SELECT k.object_type, k.object_id::text, k.key FROM acl_key k
			 WHERE NOT EXISTS (SELECT 1 FROM acl_key_expected e
			        WHERE e.object_type = k.object_type AND e.object_id = k.object_id AND e.key = k.key)
			 LIMIT $1`, []any{sampleLimit}},
		{"missing_object", `
			SELECT e.object_type, e.object_id::text, e.path FROM acl_object_expected e
			 WHERE NOT EXISTS (SELECT 1 FROM acl_object o
			        WHERE o.object_type = e.object_type AND o.object_id = e.object_id)
			 LIMIT $1`, []any{sampleLimit}},
		{"misplaced_object", `
			SELECT e.object_type, e.object_id::text, 'stored ' || o.path || ' expected ' || e.path
			  FROM acl_object_expected e
			  JOIN acl_object o ON o.object_type = e.object_type AND o.object_id = e.object_id
			 WHERE o.path IS DISTINCT FROM e.path OR o.workspace_id IS DISTINCT FROM e.workspace_id
			 LIMIT $1`, []any{sampleLimit}},
		{"extra_object", `
			SELECT o.object_type, o.object_id::text, o.path FROM acl_object o
			 WHERE o.object_type = ANY($2)
			   AND NOT EXISTS (SELECT 1 FROM acl_object_expected e
			        WHERE e.object_type = o.object_type AND e.object_id = o.object_id)
			 LIMIT $1`, []any{sampleLimit, derivedTypeList()}},
	}
	for _, s := range samples {
		rows, err := c.pool.Query(ctx, s.sql, s.args...)
		if err != nil {
			return DriftReport{}, fmt.Errorf("sample acl drift: %w", err)
		}
		for rows.Next() {
			sample := DriftSample{Kind: s.kind}
			if err := rows.Scan(&sample.ObjectType, &sample.ObjectID, &sample.Detail); err != nil {
				rows.Close()
				return DriftReport{}, fmt.Errorf("scan acl drift sample: %w", err)
			}
			r.Samples = append(r.Samples, sample)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return DriftReport{}, fmt.Errorf("sample acl drift: %w", err)
		}
	}
	return r, nil
}
