package audit

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// The canonical field order IS the chain. Changing it invalidates every hash
// ever computed, which a verifier reports as tampering — so this test exists to
// make that a deliberate act rather than a side effect of tidying a struct.
//
// If this fails and the change was intentional: bump canonicalFieldVersion,
// update the golden below, and understand that every existing chain now reports
// a break from its next entry onward.
func TestCanonicalIsStable(t *testing.T) {
	e := storedEntry{
		ID:           "11111111-1111-1111-1111-111111111111",
		WorkspaceID:  "22222222-2222-2222-2222-222222222222",
		ActorID:      "33333333-3333-3333-3333-333333333333",
		Action:       "user.login",
		ResourceType: "user",
		ResourceID:   "smtp",
		Metadata:     `{"a": 1}`, // as `metadata::text` renders it
		IPAddress:    "203.0.113.7",
		CreatedAt:    time.Date(2026, 7, 25, 12, 0, 0, 123456789, time.UTC),
		ChainSeq:     7,
	}

	const golden = "2:v1|36:11111111-1111-1111-1111-111111111111|" +
		"36:22222222-2222-2222-2222-222222222222|36:33333333-3333-3333-3333-333333333333|" +
		"10:user.login|4:user|4:smtp|7:{\"a\":1}|11:203.0.113.7|30:2026-07-25T12:00:00.123456789Z|1:7"

	if got := string(canonical(e)); got != golden {
		t.Fatalf("canonical form changed.\n got: %s\nwant: %s\n\n"+
			"Changing this invalidates every existing chain. If that is intended, "+
			"bump canonicalFieldVersion and update this golden.", got, golden)
	}
}

// A timestamp is hashed in UTC, so a server whose TimeZone setting changes does
// not retroactively break every chain it wrote.
func TestCanonicalNormalisesTheTimezone(t *testing.T) {
	utc := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	seoul := utc.In(time.FixedZone("KST", 9*3600))

	a := storedEntry{Action: "x", CreatedAt: utc}
	b := storedEntry{Action: "x", CreatedAt: seoul}
	if string(canonical(a)) != string(canonical(b)) {
		t.Fatal("the same instant in two zones must hash identically")
	}
}

// The length prefix is what stops two different entries rendering to the same
// bytes. Without it, an action of "a|4:user" would be indistinguishable from an
// action of "a" followed by a resource type of "user".
// The regression that made every chained row fail verification: metadata is a
// jsonb column, so Postgres re-renders it. The writer hashes {"a":1} and the
// verifier reads back {"a": 1} — different bytes, identical document. Both sides
// have to normalise, or the chain reports tampering on every row it ever wrote.
func TestCanonicalNormalisesJSONRendering(t *testing.T) {
	compact := storedEntry{Action: "x", Metadata: `{"a":1,"b":"z"}`}
	// Exactly what `metadata::text` returns for the same document.
	rendered := storedEntry{Action: "x", Metadata: `{"a": 1, "b": "z"}`}
	if string(canonical(compact)) != string(canonical(rendered)) {
		t.Fatalf("the same jsonb document hashed differently:\n %s\n %s",
			canonical(compact), canonical(rendered))
	}

	// Key ORDER is normalised too: jsonb orders keys by its own rule, not by
	// insertion.
	reordered := storedEntry{Action: "x", Metadata: `{"b": "z", "a": 1}`}
	if string(canonical(compact)) != string(canonical(reordered)) {
		t.Fatal("key order changed the hash; jsonb does not preserve insertion order")
	}

	// A different DOCUMENT must still hash differently.
	other := storedEntry{Action: "x", Metadata: `{"a":2,"b":"z"}`}
	if string(canonical(compact)) == string(canonical(other)) {
		t.Fatal("normalising the rendering also erased the contents")
	}

	// A large integer must not go through float64 and come back as 1.23e+18.
	big := `{"n":1234567890123456789}`
	if got := canonicalJSON(big); got != big {
		t.Fatalf("canonicalJSON(%s) = %s; a large integer lost precision", big, got)
	}
}

func TestCanonicalIsUnambiguous(t *testing.T) {
	a := storedEntry{Action: "a", ResourceType: "b", ResourceID: "c"}
	b := storedEntry{Action: "a|1:b", ResourceType: "", ResourceID: "c"}
	if string(canonical(a)) == string(canonical(b)) {
		t.Fatal("the canonical encoding is ambiguous across field boundaries")
	}
}

func TestChainHashLinks(t *testing.T) {
	e := storedEntry{ID: "x", Action: "a", ResourceType: "r", ChainSeq: 1}

	first := chainHash(nil, e)
	if len(first) != 32 {
		t.Fatalf("hash is %d bytes, want 32", len(first))
	}
	if hex.EncodeToString(chainHash(nil, e)) != hex.EncodeToString(first) {
		t.Fatal("hashing is not deterministic")
	}

	// The predecessor is part of the hash: that is what makes a deletion in the
	// middle of the chain detectable rather than merely leaving a numeric gap.
	if hex.EncodeToString(chainHash([]byte("prev"), e)) == hex.EncodeToString(first) {
		t.Fatal("prev_hash does not affect the hash")
	}
	// So is the row's own content: that is what makes an in-place edit
	// detectable.
	edited := e
	edited.Action = "a2"
	if hex.EncodeToString(chainHash(nil, edited)) == hex.EncodeToString(first) {
		t.Fatal("editing a field does not change the hash")
	}
}

// The bucket boundary is the whole coalescing contract: two events in one hour
// are one row, two events either side of the boundary are two. Getting this
// wrong in one direction loses events, in the other it stops coalescing at all.
func TestDedupeKeyHourBoundaries(t *testing.T) {
	const (
		actor  = "a"
		action = "file.downloaded"
		rtype  = "file"
		rid    = "f1"
	)
	base := time.Date(2026, 7, 25, 14, 0, 0, 0, time.UTC)

	k1, b1 := dedupeKey(actor, action, rtype, rid, base)
	k2, b2 := dedupeKey(actor, action, rtype, rid, base.Add(59*time.Minute+59*time.Second))
	if k1 != k2 {
		t.Fatal("two events inside one hour must share a dedupe key")
	}
	if !b1.Equal(b2) || !b1.Equal(base) {
		t.Fatalf("bucket = %s / %s, want the start of the hour %s", b1, b2, base)
	}

	k3, b3 := dedupeKey(actor, action, rtype, rid, base.Add(time.Hour))
	if k3 == k1 {
		t.Fatal("the next hour must be a different dedupe key")
	}
	if !b3.Equal(base.Add(time.Hour)) {
		t.Fatalf("bucket = %s, want %s", b3, base.Add(time.Hour))
	}

	// The bucket is UTC, so a server in another zone buckets identically.
	kSeoul, bSeoul := dedupeKey(actor, action, rtype, rid, base.In(time.FixedZone("KST", 9*3600)))
	if kSeoul != k1 || !bSeoul.Equal(b1) {
		t.Fatal("the hour bucket must be timezone independent")
	}

	// Every component distinguishes.
	for name, k := range map[string]string{
		"actor":         "a2",
		"action":        "file.deleted",
		"resource type": "message",
		"resource id":   "f2",
	} {
		got := [4]string{actor, action, rtype, rid}
		switch name {
		case "actor":
			got[0] = k
		case "action":
			got[1] = k
		case "resource type":
			got[2] = k
		case "resource id":
			got[3] = k
		}
		if other, _ := dedupeKey(got[0], got[1], got[2], got[3], base); other == k1 {
			t.Errorf("a different %s collided with the base dedupe key", name)
		}
	}
}

// metadata is written by call sites across nine pillars into an append-only
// table with a 365-day retention and no redaction path, so the cap has to hold
// and it has to leave valid JSON behind.
func TestMetadataCap(t *testing.T) {
	if got := encodeMetadata(nil); got != "{}" {
		t.Fatalf("nil metadata = %q, want {}", got)
	}
	small := encodeMetadata(map[string]interface{}{"a": 1})
	if small != `{"a":1}` {
		t.Fatalf("small metadata = %q", small)
	}

	big := encodeMetadata(map[string]interface{}{"blob": strings.Repeat("x", maxMetadataBytes*2)})
	if len(big) > maxMetadataBytes {
		t.Fatalf("over-cap metadata rendered %d bytes, over the %d cap", len(big), maxMetadataBytes)
	}
	// Replaced, not truncated: a truncated JSON document is not a JSON document
	// and the column is jsonb.
	if !strings.Contains(big, `"_truncated":true`) {
		t.Fatalf("over-cap metadata = %q, want a truncation marker", big)
	}
	if !strings.Contains(big, `"blob"`) {
		t.Fatalf("over-cap metadata dropped the key names, which is all that was left worth keeping: %q", big)
	}
}
