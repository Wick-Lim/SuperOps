package emoji

import (
	"strings"
	"testing"
)

func TestValidateImageURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		ok   bool
	}{
		{"https", "https://cdn.example.com/a.png", true},
		{"http", "http://cdn.example.com/a.png", true},
		{"uppercase scheme", "HTTPS://cdn.example.com/a.png", true},
		{"with query", "https://cdn.example.com/a.png?v=2", true},

		{"empty", "", false},
		{"javascript", "javascript:alert(document.cookie)", false},
		{"javascript mixed case", "JavaScript:alert(1)", false},
		{"data uri", "data:image/svg+xml;base64,PHN2Zz48L3N2Zz4=", false},
		{"file", "file:///etc/passwd", false},
		{"protocol relative", "//cdn.example.com/a.png", false},
		{"relative", "/static/a.png", false},
		{"scheme without host", "https:///a.png", false},
		{"newline injection", "https://cdn.example.com/a.png\r\nX: y", false},
		{"null byte", "https://cdn.example.com/\x00.png", false},
		{"too long", "https://cdn.example.com/" + strings.Repeat("a", maxImageURLLen), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateImageURL(tt.in)
			if tt.ok && err != nil {
				t.Fatalf("validateImageURL(%q) = %v, want nil", tt.in, err)
			}
			if !tt.ok && err == nil {
				t.Fatalf("validateImageURL(%q) = nil, want an error", tt.in)
			}
		})
	}
}

func TestNameRe(t *testing.T) {
	valid := []string{"party_parrot", "shipit", "a-b-c", "x1"}
	invalid := []string{"", "Party", "with space", "emoji:", "한글", "a.b"}
	for _, v := range valid {
		if !nameRe.MatchString(v) {
			t.Errorf("name %q should be valid", v)
		}
	}
	for _, v := range invalid {
		if nameRe.MatchString(v) {
			t.Errorf("name %q should be invalid", v)
		}
	}
}
