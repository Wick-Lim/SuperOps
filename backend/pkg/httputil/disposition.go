package httputil

import (
	"mime"
	"path/filepath"
	"strings"
)

// maxFileNameLen bounds what reaches a header. Long enough for any real name,
// short enough that a hostile one cannot make the header itself the payload.
const maxFileNameLen = 255

// SanitizeFileName reduces a client-supplied filename to a single safe path
// segment of bounded length.
//
// Three things it removes, and each is load-bearing:
//
//   - the directory component, both separators. A name is a leaf, and
//     "../../etc/passwd" reaching a Content-Disposition is how a download lands
//     somewhere the user did not choose.
//   - CONTROL CHARACTERS, including CR and LF. This is the header-injection
//     guard: a filename containing "\r\n" would end the Content-Disposition
//     header and start one of the attacker's choosing. mime.FormatMediaType
//     would encode them rather than emit them raw, but relying on that means
//     the guard lives in a dependency's implementation detail.
//   - a lone "." or "..", which become a placeholder rather than an empty
//     string: an empty filename parameter is a header some clients reject.
func SanitizeFileName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "file"
	}
	if len(name) > maxFileNameLen {
		name = name[:maxFileNameLen]
	}
	return name
}

// ContentDisposition builds an RFC 2231-encoded disposition header, so a
// filename containing quotes or non-ASCII cannot break out of it.
//
// It lives here rather than in internal/file because two packages now need it —
// the chat-attachment proxy and Drive's presigned URLs — and a second copy is
// how one of them ends up without the escaping. Drive's is the one that matters
// most: a presigned URL carries this string as a query parameter to a bucket
// that will echo it back as a header, so the escaping happens where the string
// is built or not at all.
func ContentDisposition(name string, inline bool) string {
	kind := "attachment"
	if inline {
		kind = "inline"
	}
	return mime.FormatMediaType(kind, map[string]string{"filename": SanitizeFileName(name)})
}
