package search

import (
	"strings"
	"testing"
)

const (
	wsA = "11111111-1111-1111-1111-111111111111"
	chA = "22222222-2222-2222-2222-222222222222"
	chB = "33333333-3333-3333-3333-333333333333"
	usr = "44444444-4444-4444-4444-444444444444"
	fil = "55555555-5555-5555-5555-555555555555"
	// hexID has letters in it, so ToUpper actually changes it — the numeric ids
	// above are their own uppercase form and prove nothing about canonicalisation.
	hexID = "abcdefab-cdef-abcd-efab-cdefabcdefab"
)

// keysFor is the caller-side key set the handler would build for a member of
// workspace wsA who may read the given channels.
func keysFor(channels ...string) []string {
	keys := keySet(WorkspaceKey(wsA), UserKey(usr))
	for _, ch := range channels {
		keys = append(keys, ChannelKey(ch))
	}
	return keys
}

func TestBuildFilter(t *testing.T) {
	base := `workspace_id = "` + wsA + `" AND NOT is_deleted = true AND acl IN [` +
		`"w-` + wsA + `", "u-` + usr + `", "c-` + chA + `"]`

	tests := []struct {
		name   string
		query  Query
		wantOK bool
		want   string
	}{
		{
			name:   "no access keys yields no filter and no search",
			query:  Query{WorkspaceID: wsA},
			wantOK: false,
		},
		{
			name:   "key set of only malformed keys yields no search",
			query:  Query{WorkspaceID: wsA, Keys: []string{"c-not-a-uuid", "nonsense", ""}},
			wantOK: false,
		},
		{
			name:   "a bare uuid is not a key",
			query:  Query{WorkspaceID: wsA, Keys: []string{chA}},
			wantOK: false,
		},
		{
			name:   "an unknown key prefix is not a key",
			query:  Query{WorkspaceID: wsA, Keys: []string{"x-" + chA}},
			wantOK: false,
		},
		{
			name:   "missing workspace yields no search",
			query:  Query{WorkspaceID: "", Keys: keysFor(chA)},
			wantOK: false,
		},
		{
			name:   "non-uuid workspace yields no search",
			query:  Query{WorkspaceID: `x" OR channel_id = "y`, Keys: keysFor(chA)},
			wantOK: false,
		},
		{
			name:   "non-uuid from is rejected rather than dropped",
			query:  Query{WorkspaceID: wsA, Keys: keysFor(chA), FromUserID: `" OR user_id = "`},
			wantOK: false,
		},
		{
			name:   "non-uuid channel narrowing is rejected rather than dropped",
			query:  Query{WorkspaceID: wsA, Keys: keysFor(chA), ChannelID: `" OR "1" = "1`},
			wantOK: false,
		},
		{
			name:   "an unknown type is rejected rather than dropped",
			query:  Query{WorkspaceID: wsA, Keys: keysFor(chA), Types: []ObjectType{TypeMessage, "everything"}},
			wantOK: false,
		},
		{
			name:   "workspace, user and one channel key",
			query:  Query{WorkspaceID: wsA, Keys: keysFor(chA)},
			wantOK: true,
			want:   base,
		},
		{
			name:   "type narrowing",
			query:  Query{WorkspaceID: wsA, Keys: keysFor(chA), Types: []ObjectType{TypeMessage, TypeFile}},
			wantOK: true,
			want:   base + ` AND type IN ["message", "file"]`,
		},
		{
			name:   "channel and from narrowing",
			query:  Query{WorkspaceID: wsA, Keys: keysFor(chA), ChannelID: chA, FromUserID: usr},
			wantOK: true,
			want:   base + ` AND channel_id = "` + chA + `" AND user_id = "` + usr + `"`,
		},
		{
			name:   "uppercase ids are canonicalised",
			query:  Query{WorkspaceID: strings.ToUpper(wsA), Keys: keysFor(chA)},
			wantOK: true,
			want:   base,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := buildFilter(tt.query)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (filter %q)", ok, tt.wantOK, got)
			}
			if !ok {
				if got != "" {
					t.Fatalf("filter = %q, want empty when not ok", got)
				}
				return
			}
			if got != tt.want {
				t.Fatalf("filter =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

// A filter must never be constructible without both a workspace and a non-empty
// access key set — that combination is what stops cross-tenant reads.
func TestBuildFilterAlwaysConstrainsAccess(t *testing.T) {
	got, ok := buildFilter(Query{WorkspaceID: wsA, Keys: []string{ChannelKey(chA), "c-bad", ChannelKey(chB)}})
	if !ok {
		t.Fatal("expected a filter when at least one key is valid")
	}
	if !strings.Contains(got, "acl IN [") {
		t.Fatalf("filter %q does not constrain acl", got)
	}
	if strings.Contains(got, "bad") {
		t.Fatalf("filter %q leaked a malformed key", got)
	}
	if !strings.Contains(got, `workspace_id = "`+wsA+`"`) {
		t.Fatalf("filter %q does not constrain the workspace", got)
	}
}

// The empty-key case is what TestCrossTenantSearch rests on: a caller who may
// read nothing must not fall through to an unfiltered query. A nil Meilisearch
// client makes that structural — reaching the index here panics.
func TestSearchWithNoKeysNeverQueries(t *testing.T) {
	s := &Service{}
	res, err := s.Search(t.Context(), Query{WorkspaceID: wsA, Text: "secret"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.Hits == nil {
		t.Fatal("hits must be an empty slice, not null")
	}
	if len(res.Hits) != 0 {
		t.Fatalf("hits = %v, want none", res.Hits)
	}
}

func TestCanonicalUUID(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{chA, chA, true},
		{strings.ToUpper(hexID), hexID, true},
		{"", "", false},
		{"'; DROP TABLE messages; --", "", false},
		{`" OR 1 = 1 OR "`, "", false},
		{"22222222222222222222222222222222", chA, true}, // unhyphenated form is canonicalised
	}
	for _, tt := range tests {
		got, ok := canonicalUUID(tt.in)
		if ok != tt.ok || got != tt.want {
			t.Errorf("canonicalUUID(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestQuoteFilter(t *testing.T) {
	if got, want := quoteFilter(`a"b\c`), `"a\"b\\c"`; got != want {
		t.Errorf("quoteFilter = %s, want %s", got, want)
	}
}

// hitAttributes is what a search asks Meilisearch to return: every field Hit
// decodes, and nothing else — acl above all.
func TestHitAttributesMatchHit(t *testing.T) {
	want := []string{"id", "type", "workspace_id", "channel_id", "user_id", "title", "content", "created_at", "is_deleted"}
	if !sameSet(hitAttributes, want) {
		t.Errorf("hitAttributes = %v, want %v", hitAttributes, want)
	}
	if contains(hitAttributes, "acl") {
		t.Error("acl must never be returned to a client")
	}
}
