package comment

import (
	"strings"
	"testing"
)

// A mention is <@uuid>, never "@name".
//
// Names are not unique, they change, and resolving one at read time means a
// comment's meaning depends on who is called what today — so "@alice" notifies
// nobody, deliberately, and the client is what turns a typed name into the
// encoded form.
func TestExtractMentions(t *testing.T) {
	const a = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	const b = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"

	tests := []struct {
		name string
		body string
		want []string
	}{
		{"none", "just some text", nil},
		{"one", "hey <@" + a + "> look", []string{a}},
		{"two, in order of appearance", "<@" + b + "> and <@" + a + ">", []string{b, a}},
		{"deduplicated", "<@" + a + "> ... <@" + a + ">", []string{a}},
		{"a bare name is not a mention", "hey @alice", nil},
		{"a bare uuid is not a mention", a, nil},
		{"case is normalised", "<@" + strings.ToUpper(a) + ">", []string{a}},
		{"malformed is ignored", "<@not-a-uuid> <@>", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractMentions(tt.body)
			if len(got) != len(tt.want) {
				t.Fatalf("ExtractMentions(%q) = %v, want %v", tt.body, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("ExtractMentions(%q) = %v, want %v", tt.body, got, tt.want)
				}
			}
		})
	}
}

// A preview is cut on a RUNE boundary. Slicing bytes puts a replacement glyph
// into a push notification somebody actually reads.
func TestPreviewIsRuneSafe(t *testing.T) {
	body := strings.Repeat("한", 200)
	got := preview(body)
	if strings.Contains(got, "�") {
		t.Fatalf("preview cut through a character: %q", got)
	}
	if len([]rune(got)) != 141 { // 140 + the ellipsis
		t.Fatalf("preview length = %d runes, want 141", len([]rune(got)))
	}
	if short := preview("short"); short != "short" {
		t.Errorf("preview shortened a short body: %q", short)
	}
}
