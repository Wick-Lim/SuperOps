package notification

import "testing"

// The rune-safe truncation and JSON payload tests that used to live here moved
// to internal/inbox alongside the code: this package no longer formats a
// notification body or encodes its data map. It hands an inbox.Request over and
// the inbox does both. See internal/inbox/id_test.go and prefs_test.go.

func TestMemberPrefWants(t *testing.T) {
	tests := []struct {
		name string
		p    memberPref
		kind string
		want bool
	}{
		{"default gets everything", memberPref{pref: "default"}, KindDM, true},
		{"missing row gets everything", memberPref{}, KindThreadReply, true},
		{"all gets everything", memberPref{pref: "all"}, KindThreadReply, true},
		{"muted gets nothing", memberPref{muted: true, pref: "all"}, KindMention, false},
		{"none gets nothing", memberPref{pref: "none"}, KindMention, false},
		{"mentions gets mentions", memberPref{pref: "mentions"}, KindMention, true},
		{"mentions drops dm", memberPref{pref: "mentions"}, KindDM, false},
		{"mentions drops thread reply", memberPref{pref: "mentions"}, KindThreadReply, false},
		{"mentions drops reactions", memberPref{pref: "mentions"}, KindReaction, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.wants(tt.kind); got != tt.want {
				t.Errorf("wants(%s) = %v, want %v", tt.kind, got, tt.want)
			}
		})
	}
}

func TestFanoutSkip(t *testing.T) {
	const author, blockedUser, muted, ok = "a", "b", "c", "d"
	f := &fanout{
		seen:    map[string]bool{author: true},
		blocked: map[string]bool{blockedUser: true},
		prefs:   map[string]memberPref{muted: {muted: true}},
	}
	cases := map[string]bool{author: true, blockedUser: true, muted: true, ok: false, "": true}
	for uid, want := range cases {
		if got := f.skip(uid, KindDM); got != want {
			t.Errorf("skip(%q) = %v, want %v", uid, got, want)
		}
	}
}

func TestExtractMentions(t *testing.T) {
	got := extractMentions("hey @alice, and @bob! not@anemail @")
	want := []string{"alice", "bob"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
