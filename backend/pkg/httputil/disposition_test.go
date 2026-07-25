package httputil

import (
	"strings"
	"testing"
)

func TestSanitizeFileName(t *testing.T) {
	tests := map[string]string{
		"report.pdf":               "report.pdf",
		"../../etc/passwd":         "passwd",
		`..\..\windows\system.ini`: "system.ini",
		"a\r\nb.txt":               "ab.txt",
		"":                         "file",
		"..":                       "file",
		"보고서.pdf":                  "보고서.pdf",
	}
	for in, want := range tests {
		if got := SanitizeFileName(in); got != want {
			t.Errorf("SanitizeFileName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := SanitizeFileName(strings.Repeat("a", 400)); len(got) != maxFileNameLen {
		t.Errorf("long name not capped: len = %d", len(got))
	}
}

func TestContentDispositionEscapes(t *testing.T) {
	got := ContentDisposition(`evil"; filename="x.html`, false)
	if !strings.HasPrefix(got, "attachment;") {
		t.Fatalf("disposition %q is not an attachment", got)
	}
	// A raw unescaped quote would let the value break out of the header.
	if strings.Count(got, `"`)%2 != 0 {
		t.Fatalf("unbalanced quoting in %q", got)
	}
	if inline := ContentDisposition("a.png", true); !strings.HasPrefix(inline, "inline;") {
		t.Fatalf("inline disposition = %q", inline)
	}
}

// Header injection, stated as the attack. A filename carrying CR/LF would end
// the Content-Disposition header and begin one of the caller's choosing —
// which for a presigned URL means the bucket echoing an attacker's header back
// to a browser.
func TestSanitizeFileNameStripsHeaderInjection(t *testing.T) {
	for _, in := range []string{
		"a\r\nSet-Cookie: x=1.txt",
		"a\nContent-Type: text/html.txt",
		"a\x00b.txt",
		"a\x7fb.txt",
	} {
		got := SanitizeFileName(in)
		if strings.ContainsAny(got, "\r\n\x00\x7f") {
			t.Errorf("SanitizeFileName(%q) = %q, which still carries a control character", in, got)
		}
	}
}
