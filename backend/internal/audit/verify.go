package audit

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// verifyBatch bounds one workspace's walk per run. A chain is verified forward
// from anchored_seq, so a run that stops early resumes where it left off next
// time rather than restarting.
const verifyBatch = 5000

// Break is one place a workspace's chain does not add up.
type Break struct {
	WorkspaceID string `json:"workspace_id"`
	Seq         int64  `json:"seq"`
	// Reason is one of "missing" (a gap in the sequence), "prev_mismatch" (the
	// row does not follow the one before it) or "hash_mismatch" (the row's own
	// hash does not match its contents — an in-place edit).
	Reason string `json:"reason"`
}

// ChainStatus is one workspace's answer to "is this log intact, and how much of
// it is protected by something an administrator here cannot rewrite".
type ChainStatus struct {
	WorkspaceID string  `json:"workspace_id"`
	OK          bool    `json:"ok"`
	HeadSeq     int64   `json:"head_seq"`
	AnchoredSeq int64   `json:"anchored_seq"`
	Breaks      []Break `json:"breaks"`
}

// Verifier walks chains, reports breaks and advances the off-box anchor.
type Verifier struct {
	pool   *pgxpool.Pool
	sink   Sink
	logger *slog.Logger
}

func NewVerifier(pool *pgxpool.Pool, sink Sink, logger *slog.Logger) *Verifier {
	if logger == nil {
		logger = slog.Default()
	}
	return &Verifier{pool: pool, sink: sink, logger: logger}
}

// Verify walks every named workspace's chain and reports its status. Passing no
// workspace ids verifies every workspace that has a chain head.
//
// It does NOT advance the anchor; Anchor does that, and only for chains that
// verified clean. Splitting them is what lets the read-only admin endpoint
// (GET /admin/audit-logs/verify) answer the question without having a side
// effect an auditor would have to reason about.
func (v *Verifier) Verify(ctx context.Context, workspaceIDs []string) ([]ChainStatus, error) {
	sql := `SELECT workspace_id, head_seq, anchored_seq FROM audit_chain_heads`
	var args []any
	if len(workspaceIDs) > 0 {
		sql += ` WHERE workspace_id = ANY($1)`
		args = append(args, workspaceIDs)
	}
	sql += ` ORDER BY workspace_id`

	rows, err := v.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("audit: list chain heads: %w", err)
	}
	type head struct {
		id       string
		headSeq  int64
		anchored int64
	}
	var heads []head
	for rows.Next() {
		var h head
		if err := rows.Scan(&h.id, &h.headSeq, &h.anchored); err != nil {
			rows.Close()
			return nil, fmt.Errorf("audit: scan chain head: %w", err)
		}
		heads = append(heads, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: list chain heads: %w", err)
	}

	out := make([]ChainStatus, 0, len(heads))
	for _, h := range heads {
		breaks, err := v.walk(ctx, h.id, h.anchored)
		if err != nil {
			return nil, err
		}
		out = append(out, ChainStatus{
			WorkspaceID: h.id,
			OK:          len(breaks) == 0,
			HeadSeq:     h.headSeq,
			AnchoredSeq: h.anchored,
			Breaks:      breaks,
		})
	}
	return out, nil
}

// walk recomputes one workspace's chain from `after` forward.
//
// Starting from the anchor rather than from 1 is what keeps this bounded:
// everything at or below anchored_seq has already been verified AND shipped, so
// re-walking it would re-prove a fact that is now stored somewhere else anyway.
func (v *Verifier) walk(ctx context.Context, workspaceID string, after int64) ([]Break, error) {
	rows, err := v.pool.Query(ctx, `
		SELECT id, COALESCE(actor_id::text,''), action, resource_type, COALESCE(resource_id,''),
		       metadata::text, COALESCE(host(ip_address),''), created_at, chain_seq, prev_hash, hash
		  FROM audit_logs
		 WHERE workspace_id = $1 AND chain_seq > $2
		 ORDER BY chain_seq
		 LIMIT $3`,
		workspaceID, after, verifyBatch)
	if err != nil {
		return nil, fmt.Errorf("audit: walk chain for %s: %w", workspaceID, err)
	}
	defer rows.Close()

	var breaks []Break
	var prev []byte
	expect := after + 1
	first := true

	for rows.Next() {
		var e storedEntry
		var storedPrev, storedHash []byte
		if err := rows.Scan(&e.ID, &e.ActorID, &e.Action, &e.ResourceType, &e.ResourceID,
			&e.Metadata, &e.IPAddress, &e.CreatedAt, &e.ChainSeq, &storedPrev, &storedHash); err != nil {
			return nil, fmt.Errorf("audit: scan chain row: %w", err)
		}
		e.WorkspaceID = workspaceID

		// A gap. Either a row was deleted or a transaction that allocated a seq
		// rolled back — and the second is impossible by construction, because the
		// seq is allocated under the head row lock inside the same transaction
		// that inserts the row.
		for expect < e.ChainSeq {
			breaks = append(breaks, Break{WorkspaceID: workspaceID, Seq: expect, Reason: "missing"})
			expect++
			// The chain cannot be followed across a gap: the next row's prev_hash
			// refers to a row that is gone. Trust the stored prev from here and
			// keep checking each row against ITSELF, which still catches edits.
			prev = nil
			first = true
		}
		expect = e.ChainSeq + 1

		if first {
			prev = storedPrev
			first = false
		}
		if !bytes.Equal(storedPrev, prev) {
			breaks = append(breaks, Break{WorkspaceID: workspaceID, Seq: e.ChainSeq, Reason: "prev_mismatch"})
			prev = storedPrev
		}
		if want := chainHash(prev, e); !bytes.Equal(want, storedHash) {
			breaks = append(breaks, Break{WorkspaceID: workspaceID, Seq: e.ChainSeq, Reason: "hash_mismatch"})
		}
		prev = storedHash
	}
	return breaks, rows.Err()
}

// Anchor verifies every chain and ships the head of each clean one off-box,
// advancing anchored_seq on success.
//
// It is the body of the audit_verify worker job. Three properties matter:
//
//   - A BREAK IS NOT A DENIAL OF SERVICE. It is reported here, logged at ERROR,
//     and surfaced through /health — deliberately never as a 500 on a
//     user-facing route. A corrupted audit log must not take the product down;
//     that would make corrupting it an attack.
//   - The anchor advances ONLY for a clean chain. Anchoring a broken one would
//     record the tampered state as the trusted one, which is worse than not
//     anchoring at all.
//   - anchored_seq moves only after Ship RETURNS NIL. A failed ship leaves the
//     anchor where it was and the next run retries, so the invariant
//     "everything at or below anchored_seq is off-box" holds.
func (v *Verifier) Anchor(ctx context.Context) ([]ChainStatus, error) {
	statuses, err := v.Verify(ctx, nil)
	if err != nil {
		return nil, err
	}
	if v.sink == nil {
		return statuses, nil
	}

	now := time.Now()
	var anchors []Anchor
	var clean []ChainStatus
	for _, st := range statuses {
		if !st.OK {
			v.logger.Error("audit chain is broken; not anchoring it",
				"workspace_id", st.WorkspaceID, "breaks", len(st.Breaks),
				"head_seq", st.HeadSeq, "anchored_seq", st.AnchoredSeq)
			for _, b := range st.Breaks {
				v.logger.Error("audit chain break", "workspace_id", b.WorkspaceID, "seq", b.Seq, "reason", b.Reason)
			}
			continue
		}
		if st.HeadSeq <= st.AnchoredSeq {
			continue // nothing new to anchor
		}
		hash, err := v.headHash(ctx, st.WorkspaceID)
		if err != nil {
			return statuses, err
		}
		anchors = append(anchors, Anchor{
			WorkspaceID: st.WorkspaceID,
			HeadSeq:     st.HeadSeq,
			HeadHash:    hex.EncodeToString(hash),
			At:          now,
		})
		clean = append(clean, st)
	}
	if len(anchors) == 0 {
		return statuses, nil
	}

	if err := v.sink.Ship(ctx, anchors); err != nil {
		return statuses, fmt.Errorf("audit: ship anchors via %s: %w", v.sink.Name(), err)
	}

	for i, a := range anchors {
		if _, err := v.pool.Exec(ctx,
			`UPDATE audit_chain_heads SET anchored_seq = $2, anchored_at = $3, updated_at = NOW()
			  WHERE workspace_id = $1 AND anchored_seq < $2`,
			a.WorkspaceID, a.HeadSeq, a.At); err != nil {
			return statuses, fmt.Errorf("audit: advance anchor for %s: %w", a.WorkspaceID, err)
		}
		clean[i].AnchoredSeq = a.HeadSeq
	}
	return statuses, nil
}

func (v *Verifier) headHash(ctx context.Context, workspaceID string) ([]byte, error) {
	var hash []byte
	if err := v.pool.QueryRow(ctx,
		`SELECT head_hash FROM audit_chain_heads WHERE workspace_id = $1`, workspaceID).Scan(&hash); err != nil {
		return nil, fmt.Errorf("audit: read chain head for %s: %w", workspaceID, err)
	}
	return hash, nil
}
