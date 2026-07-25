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
)

func TestBuildFilter(t *testing.T) {
	tests := []struct {
		name    string
		query   Query
		wantOK  bool
		want    string
		wantSub []string
	}{
		{
			name:   "no readable channels yields no filter and no search",
			query:  Query{WorkspaceID: wsA, ChannelIDs: nil},
			wantOK: false,
		},
		{
			name:   "readable set of only invalid ids yields no search",
			query:  Query{WorkspaceID: wsA, ChannelIDs: []string{"not-a-uuid"}},
			wantOK: false,
		},
		{
			name:   "missing workspace yields no search",
			query:  Query{WorkspaceID: "", ChannelIDs: []string{chA}},
			wantOK: false,
		},
		{
			name:   "non-uuid workspace yields no search",
			query:  Query{WorkspaceID: `x" OR channel_id = "y`, ChannelIDs: []string{chA}},
			wantOK: false,
		},
		{
			name:   "non-uuid from is rejected rather than dropped",
			query:  Query{WorkspaceID: wsA, ChannelIDs: []string{chA}, FromUserID: `" OR user_id = "`},
			wantOK: false,
		},
		{
			name:   "single channel",
			query:  Query{WorkspaceID: wsA, ChannelIDs: []string{chA}},
			wantOK: true,
			want:   `workspace_id = "` + wsA + `" AND NOT is_deleted = true AND channel_id IN ["` + chA + `"]`,
		},
		{
			name:   "multiple channels and a from filter",
			query:  Query{WorkspaceID: wsA, ChannelIDs: []string{chA, chB}, FromUserID: usr},
			wantOK: true,
			want: `workspace_id = "` + wsA + `" AND NOT is_deleted = true AND channel_id IN ["` + chA + `", "` + chB + `"]` +
				` AND user_id = "` + usr + `"`,
		},
		{
			name:   "uppercase ids are canonicalised",
			query:  Query{WorkspaceID: strings.ToUpper(wsA), ChannelIDs: []string{strings.ToUpper(chA)}},
			wantOK: true,
			want:   `workspace_id = "` + wsA + `" AND NOT is_deleted = true AND channel_id IN ["` + chA + `"]`,
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

// A filter must never be constructible without both a workspace and an explicit
// channel allowlist — that combination is what stops cross-tenant reads.
func TestBuildFilterAlwaysConstrainsChannels(t *testing.T) {
	got, ok := buildFilter(Query{WorkspaceID: wsA, ChannelIDs: []string{chA, "bad", chB}})
	if !ok {
		t.Fatal("expected a filter when at least one channel id is valid")
	}
	if !strings.Contains(got, "channel_id IN [") {
		t.Fatalf("filter %q does not constrain channel_id", got)
	}
	if strings.Contains(got, "bad") {
		t.Fatalf("filter %q leaked an invalid id", got)
	}
}

func TestCanonicalUUID(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{chA, chA, true},
		{strings.ToUpper(chA), chA, true},
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
