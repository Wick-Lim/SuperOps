package notification

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

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
			// resulting invalid UTF-8, so the recipient silently gets nothing.
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

// The old byte-slicing implementation, kept only to prove the test would have
// caught the bug.
func TestTruncateRegressionAgainstByteSlicing(t *testing.T) {
	korean := strings.Repeat("가", 100) // 300 bytes
	if utf8.ValidString(korean[:140]) {
		t.Skip("byte slice happened to land on a rune boundary")
	}
	if got := truncate(korean, 140); !utf8.ValidString(got) {
		t.Fatal("truncate must never emit invalid UTF-8")
	}
}

func TestMemberPrefWants(t *testing.T) {
	tests := []struct {
		name string
		p    memberPref
		typ  Type
		want bool
	}{
		{"default gets everything", memberPref{pref: "default"}, TypeDM, true},
		{"missing row gets everything", memberPref{}, TypeThreadReply, true},
		{"all gets everything", memberPref{pref: "all"}, TypeThreadReply, true},
		{"muted gets nothing", memberPref{muted: true, pref: "all"}, TypeMention, false},
		{"none gets nothing", memberPref{pref: "none"}, TypeMention, false},
		{"mentions gets mentions", memberPref{pref: "mentions"}, TypeMention, true},
		{"mentions drops dm", memberPref{pref: "mentions"}, TypeDM, false},
		{"mentions drops thread reply", memberPref{pref: "mentions"}, TypeThreadReply, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.wants(tt.typ); got != tt.want {
				t.Errorf("wants(%s) = %v, want %v", tt.typ, got, tt.want)
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
		if got := f.skip(uid, TypeDM); got != want {
			t.Errorf("skip(%q) = %v, want %v", uid, got, want)
		}
	}
}

func TestMarshalDataIsValidJSON(t *testing.T) {
	// %q is Go quoting, not JSON escaping; anything non-UUID used to corrupt the
	// jsonb cast.
	got, err := marshalData(map[string]string{
		"channel_id": "id\"with\\quotes\n",
		"message_id": "한글 \x01",
	})
	if err != nil {
		t.Fatalf("marshalData: %v", err)
	}
	var back map[string]string
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Fatalf("marshalData produced invalid JSON %q: %v", got, err)
	}
	if back["channel_id"] != "id\"with\\quotes\n" || back["message_id"] != "한글 \x01" {
		t.Fatalf("round trip mismatch: %#v", back)
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
