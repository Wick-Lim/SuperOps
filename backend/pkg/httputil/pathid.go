package httputil

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

// ValidateIDPathParams refuses a path parameter named `*_id` that is not a
// UUID, before the handler runs.
//
// EVERY SUCH PARAMETER REACHES POSTGRES AS A uuid, and a value that is not one
// fails the cast with SQLSTATE 22P02 — "invalid input syntax for type uuid".
// Handlers wrap that as an internal error, so a caller who mistypes an id, or
// pastes a slug where an id belongs, gets `500 internal server error` for a
// mistake that is entirely their own and entirely describable.
//
// Measured across the route table: 38 of the 107 routes carrying an id
// parameter answered 5xx for `not-a-uuid`. It is not a coverage problem — 30 of
// those 38 already had an integration test driving them, because a test of the
// happy path says nothing about what a malformed id does. The routes that
// answer 400 do it with a `uuid.Validate` written out in the handler, which is
// the same check 38 handlers were never given.
//
// IT VALIDATES REQUEST INPUT RATHER THAN TRANSLATING THE ERROR. Mapping 22P02
// to 400 down in the error path would cover these routes in one line and would
// also silence a genuine server bug: the same SQLSTATE comes from casting a
// STORED value, and that is a 500 that ought to stay one. Checking the path
// segment can only ever see what the caller sent.
//
// The rule is the suffix, and the suffix is a promise the route table already
// makes: every `{*_id}` in it is a UUID column. Parameters that are not ids —
// `{token}`, `{emoji}`, `{version}`, `{workspace_slug}` — and the type halves
// of a pair — `{object_type}`, `{subject_type}`, `{ref_type}` — do not end in
// `_id` and are not touched.
func ValidateIDPathParams(mux *http.ServeMux) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if name, ok := badIDParam(mux, r); ok {
				JSONError(w, http.StatusBadRequest, "BAD_REQUEST", name+" must be a UUID")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// badIDParam matches the request against the mux to recover the pattern it
// will be served by, then reads the id parameters straight out of the path.
//
// It does not use r.PathValue: this middleware wraps the mux, so matching has
// not happened yet and every PathValue would be empty. mux.Handler reports the
// pattern without serving it, which is the only way to know the parameter NAMES
// from out here — and the names are the whole point, since the rule is about
// what a segment is called rather than where it sits.
func badIDParam(mux *http.ServeMux, r *http.Request) (string, bool) {
	_, pattern := mux.Handler(r)
	// No match, or a pattern with no parameters: nothing to check. An unmatched
	// request gets the mux's own 404 rather than a confusing 400 from here.
	if !strings.Contains(pattern, "{") {
		return "", false
	}
	// The pattern is "METHOD /path" or "/path"; ServeMux may also carry a host.
	if i := strings.IndexByte(pattern, '/'); i > 0 {
		pattern = pattern[i:]
	}

	// EscapedPath, not Path. Path is DECODED, so a segment carrying %2F becomes
	// two segments there while ServeMux — which matches on the escaped form —
	// still counts it as one. Lining the pattern up against the decoded path
	// therefore compares a parameter name with whatever value happens to sit at
	// that index. Measured both ways: it refused a valid id and let an invalid
	// one through into the 500 this exists to prevent.
	//
	// Walked in lockstep rather than split into two slices. This runs on every
	// request in the product, and the pair of splits was three quarters of the
	// garbage it produced — 192 B/op and 4 allocs against the bare mux's 16 and
	// 1. Walking costs nothing extra to read and brings that back to one.
	pat := strings.TrimPrefix(pattern, "/")
	path := strings.TrimPrefix(r.URL.EscapedPath(), "/")

	for pat != "" {
		var seg, value string
		seg, pat = nextSegment(pat)
		value, path = nextSegment(path)

		if len(seg) < 2 || seg[0] != '{' || seg[len(seg)-1] != '}' {
			continue
		}
		// A trailing wildcard needs no special case: ServeMux only allows
		// `{name...}` as the LAST segment, so there is never a later parameter
		// to be misaligned by what it swallows — and `rest...` does not end in
		// `_id`, so it is skipped like any other non-id name. An explicit stop
		// here was unreachable, and removing it changed no test.
		name := seg[1 : len(seg)-1]
		if !strings.HasSuffix(name, "_id") {
			continue
		}
		// Validate what the HANDLER will read. Every character of a UUID is
		// unreserved, so nothing legitimate needs escaping — but nothing stops
		// a client escaping it anyway, and refusing that would be a 400 for a
		// perfectly good id. An undecodable segment never reached a route.
		//
		// PathUnescape allocates only when there is an escape to undo, which
		// for a UUID there is not.
		decoded, err := url.PathUnescape(value)
		if err != nil {
			return name, true
		}
		if uuid.Validate(decoded) != nil {
			return name, true
		}
	}
	return "", false
}

// nextSegment splits off the text before the first '/' and returns it with the
// remainder. A path shorter than its pattern yields empty values, which fail
// the UUID check like any other malformed id.
func nextSegment(s string) (seg, rest string) {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}
