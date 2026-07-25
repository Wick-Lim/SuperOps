package httputil

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// Content-type classification for uploads, and the allowlist that decides what
// may be rendered by a browser on our origin.
//
// This is a SECURITY CONTROL, not a convenience, and it lives here because two
// packages now need it: internal/file serves chat attachments through a proxy
// that sets the headers, and internal/drive presigns URLs where it cannot. A
// second copy is how one of them ends up with a shorter allowlist.

// sniffLen is the number of leading bytes http.DetectContentType inspects.
const sniffLen = 512

// inlineTypes is the closed set that may be served with
// `Content-Disposition: inline`.
//
// Closed on purpose, and small on purpose. Anything absent is downloaded rather
// than rendered, so a type nobody thought about cannot execute on our origin.
// SVG is deliberately NOT here: it is an XML document that can carry script, and
// "it is an image" is exactly the reasoning that puts it in an allowlist.
var inlineTypes = map[string]bool{
	"image/png":                true,
	"image/jpeg":               true,
	"image/gif":                true,
	"image/webp":               true,
	"image/bmp":                true,
	"image/x-icon":             true,
	"image/vnd.microsoft.icon": true,
	"application/pdf":          true,
	"text/plain":               true,
	"audio/mpeg":               true,
	"audio/wav":                true,
	"video/mp4":                true,
	"video/webm":               true,
}

// MediaType strips parameters and normalises case. An unparseable value is
// treated as the octet-stream default rather than passed through — a header
// that cannot be parsed is not a header to trust.
func MediaType(ct string) string {
	base, _, err := mime.ParseMediaType(ct)
	if err != nil || base == "" {
		return "application/octet-stream"
	}
	return strings.ToLower(base)
}

// CanServeInline reports whether a content type may be sent with
// `Content-Disposition: inline`.
func CanServeInline(ct string) bool {
	return inlineTypes[MediaType(ct)]
}

// SniffContentType classifies an upload SERVER-SIDE.
//
// The multipart part header is attacker-controlled and was once echoed straight
// back on download, which is how a .html uploaded as image/png renders as HTML.
// It is discarded entirely: nothing the client claims about its own bytes
// reaches a response header.
//
// The reader is rewound, so the caller streams the same bytes afterwards.
func SniffContentType(f io.ReadSeeker) (string, error) {
	buf := make([]byte, sniffLen)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", fmt.Errorf("read upload head: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind upload: %w", err)
	}
	return MediaType(http.DetectContentType(buf[:n])), nil
}
