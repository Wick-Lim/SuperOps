package audit

import (
	"crypto/sha256"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The per-workspace hash chain.
//
// # What it does and does not give you
//
// It makes tampering DETECTABLE, not preventable. Anyone with UPDATE on
// audit_logs can recompute the whole chain, so a chain that lives in the same
// database as the thing it protects, guarded by an administrator who has psql,
// is theatre. It becomes real at exactly one moment: when the head is anchored
// somewhere that administrator does not control. That is what Sink is for, why
// its default has to be useful rather than a placeholder, and why
// audit_chain_heads.anchored_seq is a column rather than a log line — everything
// at or below it is protected, everything above it is not, and the verify
// endpoint reports both numbers so nobody has to guess which.
//
// # Why per workspace
//
// A global chain serialises every audit insert in the deployment. Per workspace,
// the lock is contended only by writes to one tenant, and the buffered tier
// batches by workspace precisely so that one lock acquisition covers a batch
// rather than a row.
//
// # Why a locked counter and not a SEQUENCE
//
// The same argument 015_collab.up.sql makes for collab_documents.head_seq: a
// sequence hands 5 to a transaction that commits after the one holding 6, and a
// verifier walking the chain would conclude 5 does not exist. chain_seq is
// advanced under the audit_chain_heads row lock, so the chain has no gaps and no
// holes that are "allocated but not yet committed". That lock is also the only
// thing enforcing chain_seq's uniqueness: a unique index on a partitioned table
// must include the partition key, and (workspace_id, chain_seq, created_at)
// would let two rows share a seq in different months.

// canonicalFieldVersion identifies the field order below. It is folded into
// every hash, so a future change to canonical() produces visibly different
// hashes from a specific seq onward rather than silently invalidating the whole
// chain in a way that looks like tampering.
const canonicalFieldVersion = "v1"

// canonical renders one entry into the exact bytes that are hashed.
//
// Fields are LENGTH-PREFIXED, not delimited, so no value containing the
// separator can make two different entries render identically — the same
// property the derived-id encoding in internal/inbox relies on, for the same
// reason. Timestamps are UTC RFC3339 with nanosecond precision, because a
// verifier reading the row back out of Postgres has to reproduce the string it
// was hashed from and a local-zone rendering would not survive a server whose
// TimeZone setting changed.
//
// CHANGING THE FIELD ORDER INVALIDATES EVERY EXISTING CHAIN. TestCanonicalIsStable
// exists to make that a deliberate act rather than a side effect of tidying.
func canonical(e storedEntry) []byte {
	parts := []string{
		canonicalFieldVersion,
		e.ID,
		e.WorkspaceID,
		e.ActorID,
		e.Action,
		e.ResourceType,
		e.ResourceID,
		canonicalJSON(e.Metadata),
		e.IPAddress,
		e.CreatedAt.UTC().Format(time.RFC3339Nano),
		strconv.FormatInt(e.ChainSeq, 10),
	}

	size := 0
	for _, p := range parts {
		size += len(p) + 12
	}
	buf := make([]byte, 0, size)
	for i, p := range parts {
		if i > 0 {
			buf = append(buf, '|')
		}
		buf = strconv.AppendInt(buf, int64(len(p)), 10)
		buf = append(buf, ':')
		buf = append(buf, p...)
	}
	return buf
}

// canonicalJSON re-serialises a JSON document into a form both the writer and
// the verifier can reproduce.
//
// This is load-bearing and the reason is not obvious. `metadata` is a jsonb
// column, and Postgres does NOT store the bytes it was given: it parses,
// normalises and re-renders them. Go's json.Marshal emits {"a":1,"b":2};
// `metadata::text` reads back {"a": 1, "b": 2}, with spaces and with jsonb's own
// key ordering. Hashing the string the writer produced and then verifying
// against the string the reader gets back makes EVERY chained row fail
// verification — indistinguishable, from the verifier's output, from an
// administrator having edited the table.
//
// So both sides run the document through here first: decode, then re-encode with
// Go's map ordering (encoding/json sorts map keys). UseNumber keeps numeric
// literals exactly as written rather than round-tripping them through float64,
// which would turn a large integer id into 1.234e+18 on one side and not the
// other.
//
// A document that does not parse is hashed verbatim. That cannot happen for a
// value that came out of a jsonb column, and if it somehow does, hashing the raw
// bytes is the conservative answer.
func canonicalJSON(raw string) string {
	if raw == "" || raw == "{}" {
		return "{}"
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return string(out)
}

// storedEntry is one row as it exists (or is about to exist) in audit_logs. It
// is distinct from Entry because the chain hashes what was STORED — the
// generated id, the resolved timestamp, the serialized metadata — rather than
// what a caller passed in.
type storedEntry struct {
	ID           string
	WorkspaceID  string
	ActorID      string
	Action       string
	ResourceType string
	ResourceID   string
	Metadata     string
	IPAddress    string
	CreatedAt    time.Time
	ChainSeq     int64
}

// chainHash is SHA-256(prev_hash || canonical(row)). prev is nil for the first
// entry in a workspace's chain, which is what makes seq 1 verifiable without a
// genesis row.
func chainHash(prev []byte, e storedEntry) []byte {
	h := sha256.New()
	h.Write(prev)
	h.Write(canonical(e))
	return h.Sum(nil)
}

// dedupeNamespace seeds the coalescing key. Distinct from internal/inbox's
// namespace on purpose: the two hash different tuples into different tables, and
// sharing a namespace would mean a change to one had to be reasoned about
// against the other.
var dedupeNamespace = uuid.MustParse("6b3f1a72-0d4c-4b8e-9c2a-7e5f1d3b06a9")

// dedupeKey folds repeats of one read into one row per hour.
//
// (actor, action, resource_type, resource_id, hour) — the same derived-UUIDv5
// technique used everywhere else in this tree. Fifty downloads of one file in an
// afternoon become one row with event_count = 50 and last_at advanced, which is
// all an auditor ever wanted from them, and it is the difference between
// audit_logs being 30x smaller than `messages` and 30x larger.
//
// The bucket is a whole hour in UTC, so the boundary is deterministic regardless
// of the server's zone. Coalescable rows also have their created_at pinned to
// the START of the bucket, which is what lets a unique index that must include
// the partition key still behave like a unique index on dedupe_key alone.
func dedupeKey(actorID, action, resourceType, resourceID string, at time.Time) (uuid.UUID, time.Time) {
	bucket := at.UTC().Truncate(time.Hour)
	key := lengthPrefixed(actorID, action, resourceType, resourceID, bucket.Format(time.RFC3339))
	return uuid.NewSHA1(dedupeNamespace, []byte(key)), bucket
}

func lengthPrefixed(parts ...string) string {
	buf := make([]byte, 0, 64)
	for i, p := range parts {
		if i > 0 {
			buf = append(buf, '|')
		}
		buf = strconv.AppendInt(buf, int64(len(p)), 10)
		buf = append(buf, ':')
		buf = append(buf, p...)
	}
	return string(buf)
}
