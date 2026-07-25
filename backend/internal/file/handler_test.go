package file

import (
	"bytes"
	"strings"
	"testing"
)

func TestCanServeInline(t *testing.T) {
	tests := []struct {
		ct   string
		want bool
	}{
		{"image/png", true},
		{"image/jpeg", true},
		{"IMAGE/PNG", true},
		{"text/plain; charset=utf-8", true},
		{"application/pdf", true},
		// The whole point: nothing scriptable may render on our origin.
		{"text/html", false},
		{"text/html; charset=utf-8", false},
		{"image/svg+xml", false},
		{"application/xhtml+xml", false},
		{"application/javascript", false},
		{"text/xml", false},
		{"application/octet-stream", false},
		{"", false},
		{"garbage//", false},
		// A parameter must not smuggle an allowlisted type past the check.
		{`text/html; x="image/png"`, false},
	}
	for _, tt := range tests {
		if got := canServeInline(tt.ct); got != tt.want {
			t.Errorf("canServeInline(%q) = %v, want %v", tt.ct, got, tt.want)
		}
	}
}

func TestMediaTypeNormalises(t *testing.T) {
	tests := map[string]string{
		"text/plain; charset=utf-8": "text/plain",
		"TEXT/PLAIN":                "text/plain",
		"":                          "application/octet-stream",
		"not a media type":          "application/octet-stream",
	}
	for in, want := range tests {
		if got := mediaType(in); got != want {
			t.Errorf("mediaType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSniffContentTypeIgnoresClaimedType(t *testing.T) {
	// A .png named file whose bytes are HTML must be classified as HTML (and so
	// downloaded, not rendered).
	body := bytes.NewReader([]byte("<!DOCTYPE html><html><body><script>steal()</script></body></html>"))
	got, err := sniffContentType(body)
	if err != nil {
		t.Fatalf("sniffContentType: %v", err)
	}
	if got != "text/html" {
		t.Fatalf("sniffed %q, want text/html", got)
	}
	if canServeInline(got) {
		t.Fatal("html must never be servable inline")
	}
	// The reader must be rewound so the upload streams from byte zero.
	if pos, err := body.Seek(0, 1); err != nil || pos != 0 {
		t.Fatalf("reader not rewound: pos=%d err=%v", pos, err)
	}
}

func TestSniffContentTypeShortInput(t *testing.T) {
	got, err := sniffContentType(bytes.NewReader([]byte("hi")))
	if err != nil {
		t.Fatalf("sniffContentType: %v", err)
	}
	if got != "text/plain" {
		t.Fatalf("sniffed %q, want text/plain", got)
	}
}

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
		if got := sanitizeFileName(in); got != want {
			t.Errorf("sanitizeFileName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := sanitizeFileName(strings.Repeat("a", 400)); len(got) != maxFileNameLen {
		t.Errorf("long name not capped: len = %d", len(got))
	}
}

func TestContentDispositionEscapes(t *testing.T) {
	got := contentDisposition(`evil"; filename="x.html`, false)
	if !strings.HasPrefix(got, "attachment;") {
		t.Fatalf("disposition %q is not an attachment", got)
	}
	// A raw unescaped quote would let the value break out of the header.
	if strings.Count(got, `"`)%2 != 0 {
		t.Fatalf("unbalanced quoting in %q", got)
	}
	if inline := contentDisposition("a.png", true); !strings.HasPrefix(inline, "inline;") {
		t.Fatalf("inline disposition = %q", inline)
	}
}
