package inbox

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Durable consumers are at-least-once, so the id has to be a function of the
// event: the second delivery must collide with the first row rather than move a
// coalesced counter a second time.
func TestEventIDIsStableAndDistinguishing(t *testing.T) {
	const (
		user  = "1c1b6e7a-2f3d-4c5b-8a9e-0d1f2a3b4c5d"
		other = "9e8d7c6b-5a4f-4e3d-2c1b-0a9f8e7d6c5b"
		msgID = "44444444-4444-4444-4444-444444444444"
	)

	base := EventID(KindMessageMention, user, msgID)
	if base != EventID(KindMessageMention, user, msgID) {
		t.Fatal("the same event must derive the same id on every delivery")
	}
	if _, err := uuid.Parse(base); err != nil {
		t.Fatalf("derived id is not a uuid: %v", err)
	}

	distinct := map[string]string{
		"other recipient": EventID(KindMessageMention, other, msgID),
		"other kind":      EventID(KindMessageThreadReply, user, msgID),
		"other key":       EventID(KindMessageMention, user, "other"),
	}
	for name, id := range distinct {
		if id == base {
			t.Errorf("%s collided with the base id", name)
		}
	}

	// The length prefix is what makes the key unambiguous: without it these two
	// tuples render to the same string.
	if EventID(KindMessageDM, "a", "b\x00c") == EventID(KindMessageDM, "a\x00b", "c") {
		t.Error("the id key is ambiguous across field boundaries")
	}
}

// The item id is what the list's keyset cursor uses as its tiebreaker, so it has
// to be a function of the item's identity rather than of when it was created.
func TestItemIDIsStableAndDistinguishing(t *testing.T) {
	const (
		user    = "1c1b6e7a-2f3d-4c5b-8a9e-0d1f2a3b4c5d"
		channel = "44444444-4444-4444-4444-444444444444"
	)
	base := ItemID(user, SubjectChannel, channel)
	if base != ItemID(user, SubjectChannel, channel) {
		t.Fatal("item ids must be stable")
	}
	if base == ItemID(user, "document", channel) {
		t.Fatal("two subject types sharing an id must not collapse into one item")
	}
	if _, err := uuid.Parse(base); err != nil {
		t.Fatalf("derived id is not a uuid: %v", err)
	}
}

// The namespace is not decorative. Migration 020's backfill re-derives event ids
// with this exact constant in SQL, so changing it silently duplicates every
// notification once on the first fan-out after the change.
func TestNamespaceMatchesTheMigration(t *testing.T) {
	const inMigration = "3d0f5e2a-6c1b-4f7e-9a1d-2b8c5f0e7a41"
	if Namespace.String() != inMigration {
		t.Fatalf("Namespace = %s, but migrations/020_inbox.up.sql derives with %s. "+
			"Changing this duplicates every inbox event exactly once", Namespace, inMigration)
	}
}

// The wire format the SQL side reproduces with format('%s:%s|%s:%s|%s:%s', ...).
func TestDeriveKeyEncoding(t *testing.T) {
	got := derive("ab", "cde", "")
	want := uuid.NewSHA1(Namespace, []byte("2:ab|3:cde|0:")).String()
	if got != want {
		t.Fatalf("derive encoding changed: %s != %s", got, want)
	}
}

func TestEventKeyFoldsTheDiscriminator(t *testing.T) {
	if eventKey("m", "") != "m" {
		t.Fatal("an empty discriminator must leave the object id alone")
	}
	if eventKey("m", "a") == eventKey("m", "b") {
		t.Fatal("two discriminators over one object must be two events")
	}
}

// kindRank decides the icon and the kind= filter when several kinds land on one
// subject. It lives in Go so a pillar adding a kind needs no migration — which
// means an UNREGISTERED kind must behave sensibly rather than winning by
// accident.
func TestKindRankOrdering(t *testing.T) {
	order := []string{
		KindMessageDM, KindMessageMention, KindChannelInvited,
		KindMessageThreadReply, KindMessageReaction,
	}
	for i := 1; i < len(order); i++ {
		if rankOf(order[i-1]) <= rankOf(order[i]) {
			t.Fatalf("%s must outrank %s", order[i-1], order[i])
		}
	}
	if rankOf("issue.assigned") != 0 {
		t.Fatal("an unregistered kind must rank at zero")
	}

	// kindsRankedAbove is what the SQL CASE compares the stored top_kind
	// against. A DM outranks everything, so nothing survives it; a reaction
	// outranks nothing, so every registered kind survives it.
	if got := kindsRankedAbove(KindMessageDM); len(got) != 0 {
		t.Fatalf("nothing outranks a DM, got %v", got)
	}
	if got := kindsRankedAbove(KindMessageReaction); len(got) != len(kindRank)-1 {
		t.Fatalf("every other registered kind outranks a reaction, got %v", got)
	}
	// An unregistered incoming kind is outranked by every registered one, so an
	// existing icon is not replaced by one this build cannot rank.
	if got := kindsRankedAbove("issue.assigned"); len(got) != len(kindRank) {
		t.Fatalf("every registered kind outranks an unregistered one, got %v", got)
	}
}

func TestTruncateIsRuneSafe(t *testing.T) {
	korean := strings.Repeat("가", 200)    // 3 bytes per rune → 600 bytes
	mixed := strings.Repeat("한a", 100)    // 4 bytes per iteration
	emoji := strings.Repeat("👍", 100)     // 4 bytes per rune, surrogate-free in Go
	combining := strings.Repeat("é", 200) // 2 bytes per rune

	tests := []struct {
		name      string
		in        string
		max       int
		wantRunes int
		wantSuf   bool
	}{
		{"ascii under limit", "hello", 140, 5, false},
		{"ascii at limit", strings.Repeat("a", 140), 140, 140, false},
		{"ascii over limit", strings.Repeat("a", 141), 140, 140, true},
		{"korean over limit", korean, 140, 140, true},
		{"mixed over limit", mixed, 140, 140, true},
		{"emoji over limit", emoji, 140, 100, false}, // 100 runes, under the limit
		{"accented over limit", combining, 140, 140, true},
		{"empty", "", 140, 0, false},
		{"zero budget", korean, 0, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.in, tt.max)

			// The regression: a byte slice cuts mid-rune and Postgres rejects the
			// resulting invalid UTF-8, so the recipient silently receives nothing.
			if !utf8.ValidString(got) {
				t.Fatalf("truncate produced invalid UTF-8: %q", got)
			}
			body := strings.TrimSuffix(got, "...")
			if n := utf8.RuneCountInString(body); n != tt.wantRunes {
				t.Errorf("body rune count = %d, want %d", n, tt.wantRunes)
			}
			if strings.HasSuffix(got, "...") != tt.wantSuf {
				t.Errorf("suffix presence = %v, want %v", strings.HasSuffix(got, "..."), tt.wantSuf)
			}
			if !strings.HasPrefix(tt.in, body) {
				t.Errorf("truncated body is not a prefix of the input")
			}
		})
	}
}

// The shipped React Native client switches on `type`, so every kind has to map
// onto one of migration 005's five enum values — and a kind from a pillar the
// client has never heard of has to land on its default arm rather than on
// nothing.
func TestLegacyTypeMapping(t *testing.T) {
	cases := map[string]string{
		KindMessageMention:     "mention",
		KindMessageDM:          "dm",
		KindMessageThreadReply: "thread_reply",
		KindChannelInvited:     "channel_invite",
		KindMessageReaction:    "system",
		"issue.assigned":       "system",
		"":                     "system",
	}
	for kind, want := range cases {
		if got := legacyType(kind); got != want {
			t.Errorf("legacyType(%q) = %q, want %q", kind, got, want)
		}
	}
}
