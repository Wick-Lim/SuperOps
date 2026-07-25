// Package audit records who did what, to which object, from where.
//
// # The two forces, and how they are resolved
//
// Audit has to be cheap enough to leave on and trustworthy enough to be worth
// leaving on, and those pull in opposite directions. Cheap says: buffer the
// writes, coalesce aggressively, skip reads, drop under pressure. Trustworthy
// says: write synchronously, never coalesce, record everything, never drop.
// Every audit system that fails, fails by picking one side silently.
//
// The resolution here is to make the trade EXPLICIT AND PER-CATEGORY:
//
//   - Tier 1, synchronous, never coalesced, chained. Authentication and session,
//     authorization change, sharing, configuration. These are the things that
//     must not be lost, and they are exactly the things that are low volume —
//     hundreds a day, not millions — so they can afford it. Record / Try.
//   - Tier 2, buffered, optionally coalesced. Egress and sensitive reads. These
//     are the things that are high volume, and exactly the things where an
//     hourly count is as good as individual rows. Buffer.
//
// Nothing is left in the middle undecided.
//
// # The volume rule
//
// ROUTINE READS ARE NOT AUDITED. Auditing every GET /api/v1/messages writes
// 10-100x the row count of `messages` itself and nobody has ever read those
// rows. What is audited is a read that CROSSES A SENSITIVITY BOUNDARY: a file
// download or export, a read reached through an explicit grant or share link
// rather than through container membership, any read an admin performs against
// another user's data, and a read of the audit log itself. That single decision
// is the whole difference between this table being 30x smaller than `messages`
// and 30x larger.
//
// # What protects the log from the administrators it audits
//
// Four layers, honestly priced:
//
//  1. No API surface that mutates audit. There is none and there must never be
//     one; disabling auditing is startup configuration, not a runtime endpoint,
//     so turning it off lands in the deploy trail instead of in the product.
//  2. The hash chain (chain.go). Detects in-place edits and deletions.
//  3. Off-box anchoring (sink.go). The layer that actually matters, because it
//     is the only one the local administrator does not control.
//  4. Append-only at the database role — the application connecting as a role
//     with INSERT/SELECT and no UPDATE/DELETE, with migrations and retention on
//     a separate role. THIS IS NOT IMPLEMENTED. See docs/DEPLOYMENT.md
//     "Audit log trust boundary" for what that costs and why it was deferred.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
)

// Common actions. Keep the "<resource>.<verb>" shape when adding more.
const (
	ActionLogin           = "user.login"
	ActionLoginFailed     = "user.login_failed"
	ActionLogout          = "user.logout"
	ActionPasswordChanged = "user.password_changed"
	ActionTOTPEnabled     = "user.totp_enabled"
	ActionTOTPDisabled    = "user.totp_disabled"
	ActionInviteAccepted  = "invitation.accepted"

	// Egress — the category an auditor actually asks about, and the one that had
	// no coverage at all.
	ActionFileDownloaded = "file.downloaded"
	ActionAuditExported  = "audit.exported"

	// Configuration, including reads of the audit log itself. That last one is
	// the row that catches an administrator going looking.
	ActionAuditRead     = "audit.read"
	ActionAuditSinkTest = "audit.sink_test"
	ActionMailTestSent  = "mail.test_sent"
)

// maxMetadataBytes caps the serialized metadata.
//
// It is written by call sites across nine pillars and will otherwise become an
// accidental content store inside an append-only table with a 365-day retention
// — at which point a GDPR deletion request arrives for data whose only removal
// path is a redaction through an append-only log, which is a contradiction.
// Metadata never carries content: no message text, no file contents, no tokens.
const maxMetadataBytes = 4 << 10

// Entry is a single audit record. Action and ResourceType are required; every
// other field is optional and empty IDs are persisted as NULL.
type Entry struct {
	WorkspaceID  string
	ActorID      string
	Action       string
	ResourceType string
	// ResourceID is TEXT, not UUID. It was a UUID column until migration 021,
	// which is why internal/admin/mail.go passing "smtp" into it failed with
	// 22P02 on every single mail test — an error that was discarded, so the
	// action was never once recorded.
	ResourceID string
	// IPAddress is the caller's address. Anything that is not a parsable IP
	// literal (including "") is stored as NULL rather than failing the insert.
	IPAddress string
	Metadata  map[string]interface{}

	// Coalesce folds repeats of the same (actor, action, resource, hour) into
	// one row carrying a count. Set it for sensitive reads and egress; NEVER for
	// a mutation or an authentication event, where "it happened twice" is the
	// whole point.
	//
	// A coalesced row is mutated on every repeat, so it is deliberately NOT
	// chained — a hash over it would go stale on the second event. Migration
	// 021 enforces that with a CHECK.
	Coalesce bool

	// At overrides the timestamp. Zero means now. Only the buffered tier sets
	// it, so a record queued during an incident carries the time it happened
	// rather than the time the queue drained.
	At time.Time
}

type Service struct {
	pool   *pgxpool.Pool
	logger *slog.Logger

	// buffer is the Tier 2 queue. nil disables buffering, in which case Buffer
	// falls back to a synchronous Try — correct, just slower.
	buffer *buffer

	// failures counts audit writes that were attempted and lost. Try swallows
	// errors by design (a failed audit write must not turn a successful login
	// into a 500), which means a broken audit path is otherwise invisible except
	// in logs. /metrics reads this.
	failures atomic.Int64
}

func NewService(pool *pgxpool.Pool, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{pool: pool, logger: logger}
}

// Failures is the count of audit writes lost since boot, for /metrics. It must
// be alertable: a silently broken audit path is indistinguishable from a quiet
// system.
func (s *Service) Failures() int64 { return s.failures.Load() }

// Record writes one audit entry synchronously and returns the failure.
//
// Use it when the audited operation has NOT yet committed and losing the trail
// entry should fail the operation. Everything in Tier 1 goes through here or
// through Try.
func (s *Service) Record(ctx context.Context, e Entry) error {
	return s.writeBatch(ctx, []Entry{e})
}

// Try records an entry, logging rather than returning a failure. Use it once the
// audited operation has already committed: losing the trail entry must be
// visible in the logs, but it must not turn a successful login into a 500.
//
// The write is detached from the caller's cancellation so that a client which
// hangs up immediately after (say) a failed login still leaves a record. That is
// a contract, not an implementation detail — see TestAuditSurvivesRequestCancellation.
func (s *Service) Try(ctx context.Context, e Entry) {
	if err := s.Record(context.WithoutCancel(ctx), e); err != nil {
		s.failures.Add(1)
		s.logger.Error("audit log write failed",
			"error", err, "action", e.Action, "actor_id", e.ActorID, "resource_id", e.ResourceID)
	}
}

// Buffer queues a Tier 2 entry: egress and sensitive reads, off the request's
// critical path.
//
// A synchronous pool.Exec on a file download is a latency regression on a hot
// path, which is the entire reason this tier exists. It is also exactly the
// design that invites the worst failure an audit system can have — silently
// dropping records under the load that made them interesting — so the drop is
// COUNTED and LOGGED and appears in /metrics, and Tier 1 never touches this
// queue.
func (s *Service) Buffer(ctx context.Context, e Entry) {
	if s.buffer == nil {
		s.Try(ctx, e)
		return
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	s.buffer.enqueue(e)
}

// Log is the positional form of Record, kept for existing call sites.
func (s *Service) Log(ctx context.Context, workspaceID, actorID, action, resourceType, resourceID string, metadata map[string]interface{}) error {
	return s.Record(ctx, Entry{
		WorkspaceID:  workspaceID,
		ActorID:      actorID,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Metadata:     metadata,
	})
}

// writeBatch persists a batch of entries.
//
// Batching is by WORKSPACE, and that is required rather than an optimisation:
// the chain head is a per-workspace row lock, so a batch spanning three
// workspaces has to be three lock acquisitions. Entries with no workspace cannot
// be chained (there is no chain to put them in — a failed login names no tenant),
// and coalescable entries must not be, so both take the unchained path.
func (s *Service) writeBatch(ctx context.Context, entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}

	chained := map[string][]Entry{}
	var loose []Entry
	for _, e := range entries {
		e.WorkspaceID = strings.ToLower(e.WorkspaceID)
		if e.WorkspaceID != "" && !e.Coalesce {
			chained[e.WorkspaceID] = append(chained[e.WorkspaceID], e)
			continue
		}
		loose = append(loose, e)
	}

	var firstErr error
	for ws, batch := range chained {
		if err := s.writeChained(ctx, ws, batch); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, e := range loose {
		if err := s.writeLoose(ctx, e); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// writeChained inserts a workspace's entries under its chain head lock, in one
// transaction. One lock acquisition per batch, not per row.
func (s *Service) writeChained(ctx context.Context, workspaceID string, entries []Entry) error {
	return database.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		// Create-then-lock. The head row is per workspace and is created lazily,
		// because a workspace that has never been audited has no reason to carry
		// a row and the FK makes an orphan impossible.
		if _, err := tx.Exec(ctx,
			`INSERT INTO audit_chain_heads (workspace_id) VALUES ($1) ON CONFLICT DO NOTHING`,
			workspaceID); err != nil {
			return fmt.Errorf("audit: ensure chain head: %w", err)
		}

		var seq int64
		var prev []byte
		if err := tx.QueryRow(ctx,
			`SELECT head_seq, head_hash FROM audit_chain_heads WHERE workspace_id = $1 FOR UPDATE`,
			workspaceID).Scan(&seq, &prev); err != nil {
			return fmt.Errorf("audit: lock chain head: %w", err)
		}

		for _, e := range entries {
			seq++
			stored, err := prepare(e, seq)
			if err != nil {
				return err
			}
			hash := chainHash(prev, stored)

			if _, err := tx.Exec(ctx,
				`INSERT INTO audit_logs (id, workspace_id, actor_id, action, resource_type, resource_id,
				                         metadata, ip_address, created_at, chain_seq, prev_hash, hash)
				 VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::inet, $9, $10, $11, $12)`,
				stored.ID, stored.WorkspaceID, nilIfEmpty(stored.ActorID), stored.Action, stored.ResourceType,
				nilIfEmpty(stored.ResourceID), stored.Metadata, nilIfNotIP(stored.IPAddress),
				stored.CreatedAt, seq, prev, hash); err != nil {
				return fmt.Errorf("audit log: %w", err)
			}
			prev = hash
		}

		if _, err := tx.Exec(ctx,
			`UPDATE audit_chain_heads SET head_seq = $2, head_hash = $3, updated_at = NOW()
			  WHERE workspace_id = $1`,
			workspaceID, seq, prev); err != nil {
			return fmt.Errorf("audit: advance chain head: %w", err)
		}
		return nil
	})
}

// writeLoose inserts an entry that carries no chain: either it names no
// workspace, or it coalesces.
func (s *Service) writeLoose(ctx context.Context, e Entry) error {
	stored, err := prepare(e, 0)
	if err != nil {
		return err
	}

	if !e.Coalesce {
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO audit_logs (id, workspace_id, actor_id, action, resource_type, resource_id,
			                         metadata, ip_address, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::inet, $9)`,
			stored.ID, nilIfEmpty(stored.WorkspaceID), nilIfEmpty(stored.ActorID), stored.Action,
			stored.ResourceType, nilIfEmpty(stored.ResourceID), stored.Metadata,
			nilIfNotIP(stored.IPAddress), stored.CreatedAt); err != nil {
			return fmt.Errorf("audit log: %w", err)
		}
		return nil
	}

	// Coalesced. created_at is pinned to the start of the dedupe key's hour
	// bucket so the (dedupe_key, created_at) unique index — which has to include
	// the partition key — behaves like a unique index on dedupe_key alone.
	// last_at carries the real time of the most recent occurrence.
	key, bucket := dedupeKey(e.ActorID, e.Action, e.ResourceType, e.ResourceID, stored.CreatedAt)
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO audit_logs (id, workspace_id, actor_id, action, resource_type, resource_id,
		                         metadata, ip_address, created_at, dedupe_key, event_count, last_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::inet, $9, $10, 1, $11)
		 ON CONFLICT (dedupe_key, created_at) WHERE dedupe_key IS NOT NULL DO UPDATE SET
		     event_count = audit_logs.event_count + 1,
		     last_at     = EXCLUDED.last_at`,
		stored.ID, nilIfEmpty(stored.WorkspaceID), nilIfEmpty(stored.ActorID), stored.Action,
		stored.ResourceType, nilIfEmpty(stored.ResourceID), stored.Metadata,
		nilIfNotIP(stored.IPAddress), bucket, key, stored.CreatedAt); err != nil {
		return fmt.Errorf("audit log (coalesced): %w", err)
	}
	return nil
}

// prepare resolves an Entry into the row that will be STORED — and therefore
// into exactly what the chain hashes.
//
// Every normalisation below exists because the verifier reads the row back out
// of Postgres and has to reproduce the bytes that were hashed. Postgres does not
// store what it is given verbatim:
//
//   - timestamptz has MICROSECOND resolution. time.Now() has nanoseconds, so an
//     unrounded timestamp is hashed at one precision and read back at another,
//     and every chained row fails verification.
//   - uuid renders lowercase, so an uppercase id from a caller would not survive
//     the round trip.
//   - inet normalises an address (2001:0db8::1 reads back as 2001:db8::1).
//   - jsonb re-renders the whole document; see canonicalJSON.
//
// The normalised values are what go into the INSERT as well as into the hash, so
// the row and its hash agree by construction rather than by luck.
func prepare(e Entry, seq int64) (storedEntry, error) {
	at := e.At
	if at.IsZero() {
		at = time.Now()
	}
	return storedEntry{
		ID:           uuid.NewString(),
		WorkspaceID:  strings.ToLower(e.WorkspaceID),
		ActorID:      strings.ToLower(e.ActorID),
		Action:       e.Action,
		ResourceType: e.ResourceType,
		ResourceID:   e.ResourceID,
		Metadata:     encodeMetadata(e.Metadata),
		IPAddress:    normalizeIP(e.IPAddress),
		CreatedAt:    at.Truncate(time.Microsecond),
		ChainSeq:     seq,
	}, nil
}

// normalizeIP renders an address the way Postgres' host(inet) will read it back,
// or "" for anything that is not an address literal. See prepare.
func normalizeIP(s string) string {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil {
		return ""
	}
	return ip.String()
}

// encodeMetadata serialises the metadata map, capped.
//
// Over the cap the payload is REPLACED by a marker rather than truncated
// mid-object: a truncated JSON document is not a JSON document, and the column
// is jsonb. The keys are kept so the row still says what it was about.
func encodeMetadata(m map[string]interface{}) string {
	if len(m) == 0 {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return `{"_error":"metadata is not encodable"}`
	}
	if len(b) <= maxMetadataBytes {
		return string(b)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	marker, err := json.Marshal(map[string]interface{}{
		"_truncated": true,
		"_bytes":     len(b),
		"_keys":      keys,
	})
	if err != nil {
		return `{"_truncated":true}`
	}
	return string(marker)
}

func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func nilIfNotIP(s string) interface{} {
	// A forwarded header can carry "ip:port" or a list; anything that is not a
	// bare literal is stored as NULL rather than failing the insert.
	if s = normalizeIP(s); s == "" {
		return nil
	}
	return s
}
