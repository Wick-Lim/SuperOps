package webhook

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBearerToken(t *testing.T) {
	tests := map[string]string{
		"Bearer abc123":   "abc123",
		"bearer abc123":   "abc123",
		"BEARER abc123":   "abc123",
		"Bearer  abc123 ": "abc123",
		"Bearer":          "",
		"Bearer ":         "",
		"":                "",
		"Basic abc123":    "",
		"abc123":          "",
	}
	for header, want := range tests {
		if got := bearerToken(header); got != want {
			t.Errorf("bearerToken(%q) = %q, want %q", header, got, want)
		}
	}
}

func TestSanitizeName(t *testing.T) {
	tests := map[string]string{
		"deploybot":             "deploybot",
		"":                      "webhook",
		"   ":                   "webhook",
		"a\r\nb":                "ab",
		"[admin]":               "admin",
		"nul\x00byte":           "nulbyte",
		"배포봇":                   "배포봇",
		strings.Repeat("한", 60): strings.Repeat("한", 40),
	}
	for in, want := range tests {
		got := sanitizeName(in)
		if got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("sanitizeName(%q) produced invalid UTF-8", in)
		}
	}
}
