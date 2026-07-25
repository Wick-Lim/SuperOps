package audit

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Monthly range partitions on audit_logs.created_at.
//
// The whole reason they exist: retention becomes a partition DROP rather than a
// DELETE. cmd/worker's message retention job is batched, capped and
// advisory-locked precisely because an unbounded DELETE on a large table was a
// production problem; partitioning means audit never has that problem at all.
//
// Not pg_partman. That would be a new extension in the Compose and Helm images
// to replace the sixty lines below, running inside a job loop this worker
// already has.
//
// A MISSING PARTITION IS A FAILED INSERT, i.e. a lost audit record. That is why
// EnsurePartitions keeps two months of lead time and why its failure is loud —
// cmd/worker's health.fail surfaces it on /health rather than logging and
// shrugging.

// PartitionLead is how many months ahead of the current one must exist. Two
// means a worker that stops entirely still has a month of runway before an
// INSERT can fail.
const PartitionLead = 2

// partitionName is the naming convention migration 021 also uses for the
// partition it creates during the conversion, so this job finds that one by name
// and skips it rather than failing on an overlapping range.
func partitionName(month time.Time) string {
	return "audit_logs_p" + month.UTC().Format("2006_01")
}

// EnsurePartitions creates any missing monthly partition from the current month
// through PartitionLead months ahead. It reports how many it created.
//
// CREATE TABLE IF NOT EXISTS, not a catalog probe: the name is deterministic, so
// existence by name is exactly the question, and two replicas racing on the same
// tick both succeed instead of one erroring.
func EnsurePartitions(ctx context.Context, pool *pgxpool.Pool, lead int) (int, error) {
	if lead < 0 {
		lead = PartitionLead
	}
	created := 0
	start := time.Now().UTC()
	for i := 0; i <= lead; i++ {
		from := monthStart(start.AddDate(0, i, 0))
		to := from.AddDate(0, 1, 0)

		// The identifier is derived from a timestamp, not from user input.
		sql := fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %s PARTITION OF audit_logs FOR VALUES FROM ('%s') TO ('%s')`,
			partitionName(from), from.Format("2006-01-02"), to.Format("2006-01-02"))
		tag, err := pool.Exec(ctx, sql)
		if err != nil {
			return created, fmt.Errorf("audit: create partition %s: %w", partitionName(from), err)
		}
		// Postgres reports CREATE TABLE either way; the count is derived from
		// whether the relation was already there, which the IF NOT EXISTS notice
		// does not surface. Counting attempts is close enough for a log line and
		// avoids a second round trip per month.
		_ = tag
		created++
	}
	return created, nil
}

// DropExpiredPartitions removes every partition whose entire range is older than
// the retention window. It reports the names it dropped.
//
// This is the payoff. A partition whose upper bound is already past the cutoff
// contains nothing worth keeping, so retention is a DDL statement that returns
// in milliseconds and frees the disk immediately — instead of a batched DELETE
// that has to be capped so it does not hold locks over an arbitrarily large row
// set, followed by a VACUUM that returns the space some time later.
//
// Retention is DEPLOYMENT-WIDE, not per workspace. Per-workspace retention would
// mean rows with different lifetimes sharing a partition, which turns the DROP
// back into the batched DELETE this exists to avoid. A self-hosted company's
// retention policy is a property of the company.
func DropExpiredPartitions(ctx context.Context, pool *pgxpool.Pool, retentionDays int) ([]string, error) {
	if retentionDays <= 0 {
		return nil, nil // retention disabled
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays)

	rows, err := pool.Query(ctx, `
		SELECT c.relname, pg_get_expr(c.relpartbound, c.oid)
		  FROM pg_class c
		  JOIN pg_inherits i ON i.inhrelid = c.oid
		 WHERE i.inhparent = 'audit_logs'::regclass
		 ORDER BY c.relname`)
	if err != nil {
		return nil, fmt.Errorf("audit: list partitions: %w", err)
	}
	type part struct{ name, bound string }
	var parts []part
	for rows.Next() {
		var p part
		if err := rows.Scan(&p.name, &p.bound); err != nil {
			rows.Close()
			return nil, fmt.Errorf("audit: scan partition: %w", err)
		}
		parts = append(parts, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: list partitions: %w", err)
	}

	var dropped []string
	for _, p := range parts {
		// The upper bound is what decides eligibility: a partition is droppable
		// only when EVERY row it can hold is past the cutoff. Reading it from
		// the name would be wrong for the conversion partition created by
		// migration 021, whose lower bound is MINVALUE and whose name is the
		// month the migration ran in.
		upper, ok := partitionUpperBound(p.bound)
		if !ok || !upper.Before(cutoff) {
			continue
		}
		if _, err := pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, p.name)); err != nil {
			return dropped, fmt.Errorf("audit: drop partition %s: %w", p.name, err)
		}
		dropped = append(dropped, p.name)
	}
	return dropped, nil
}

// partitionUpperBound parses the TO ('…') half of a range partition bound
// expression, which Postgres renders as
// `FOR VALUES FROM ('2026-07-01 00:00:00+00') TO ('2026-08-01 00:00:00+00')`.
//
// A MAXVALUE upper bound, or anything unparseable, reports false: a partition
// this function does not understand is one it must not drop.
func partitionUpperBound(bound string) (time.Time, bool) {
	_, after, found := strings.Cut(bound, " TO (")
	if !found {
		return time.Time{}, false
	}
	raw, _, found := strings.Cut(after, ")")
	if !found {
		return time.Time{}, false
	}
	raw = strings.Trim(strings.TrimSpace(raw), "'")
	for _, layout := range []string{
		"2006-01-02 15:04:05-07", "2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05", "2006-01-02",
	} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func monthStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// RunPartitions is the body of the audit_partitions worker job: keep the window
// ahead, drop what has aged out.
func RunPartitions(ctx context.Context, pool *pgxpool.Pool, retentionDays int, logger *slog.Logger) error {
	if _, err := EnsurePartitions(ctx, pool, PartitionLead); err != nil {
		return err
	}
	dropped, err := DropExpiredPartitions(ctx, pool, retentionDays)
	if err != nil {
		return err
	}
	if len(dropped) > 0 {
		logger.Info("audit retention: dropped expired partitions",
			"count", len(dropped), "partitions", strings.Join(dropped, ","), "retention_days", retentionDays)
	}
	return nil
}
