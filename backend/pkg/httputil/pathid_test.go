package httputil

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// muxFor builds a mux with the patterns a test needs and the middleware in
// front of it, answering 204 from the handler so any 400 came from here.
func muxFor(patterns ...string) http.Handler {
	mux := http.NewServeMux()
	for _, p := range patterns {
		mux.Handle(p, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
	}
	return ValidateIDPathParams(mux)(mux)
}

// THE RULE IS THE SUFFIX, AND IT HAS TO CUT BOTH WAYS.
//
// Refusing a bad id is half the job; the other half is not refusing anything
// else. A check that fired on every parameter would break `{token}`,
// `{workspace_slug}` and the type halves of an ACL pair, none of which are
// UUIDs and all of which are legitimate.
func TestOnlyIDParametersAreRequiredToBeUUIDs(t *testing.T) {
	const id = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"

	h := muxFor(
		"GET /api/v1/things/{thing_id}",
		"GET /api/v1/w/{workspace_id}/things/{thing_id}",
		"GET /api/v1/tokens/{token}",
		"GET /api/v1/slugs/{workspace_slug}",
		"GET /api/v1/acl/{object_type}/{object_id}",
	)

	for _, c := range []struct {
		name, path string
		want       int
	}{
		{"a bad id", "/api/v1/things/not-a-uuid", http.StatusBadRequest},
		{"a good id", "/api/v1/things/" + id, http.StatusNoContent},
		{"a bad id in the second position", "/api/v1/w/" + id + "/things/nope",
			http.StatusBadRequest},
		{"a bad id in the first position", "/api/v1/w/nope/things/" + id,
			http.StatusBadRequest},
		{"both good", "/api/v1/w/" + id + "/things/" + id, http.StatusNoContent},

		// Not ids. These are the reason the rule is a suffix and not "every
		// parameter": each is a real route in this codebase.
		{"a token is not an id", "/api/v1/tokens/abc123", http.StatusNoContent},
		{"a slug is not an id", "/api/v1/slugs/acme-corp", http.StatusNoContent},
		{"the type half of a pair", "/api/v1/acl/file/" + id, http.StatusNoContent},

		// An unmatched path is the mux's 404 to answer, not a 400 from here:
		// a request that reaches no route has no parameters to be wrong about.
		{"no route matches", "/api/v1/nothing/here", http.StatusNotFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", c.path, nil))
			if w.Code != c.want {
				t.Errorf("%s = %d, want %d", c.path, w.Code, c.want)
			}
		})
	}
}

// AN ENCODED SLASH MUST NOT SHIFT THE SEGMENTS UNDER THE PATTERN.
//
// The parameter names come from the pattern and the values from the path, and
// the two are lined up BY POSITION. r.URL.Path is the DECODED path, so a
// segment containing %2F becomes two segments there while ServeMux — which
// matches on the escaped path — still sees one. Reading positions out of the
// decoded path therefore compares `{thing_id}` against whatever happens to sit
// at that index, which is a different parameter's value or nothing at all.
//
// Both directions are wrong and both are reachable: a good id can be refused,
// and a bad one waved through into the 500 this exists to prevent.
func TestAnEncodedSlashDoesNotMisalignTheParameters(t *testing.T) {
	const id = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	h := muxFor("GET /api/v1/w/{workspace_slug}/things/{thing_id}")

	for _, c := range []struct {
		name, path string
		want       int
	}{
		{
			// Decoded, this is 4 segments before {thing_id} instead of 3, so a
			// positional read lands on "things" and refuses a valid id.
			"a slug carrying an encoded slash, with a good id",
			"/api/v1/w/a%2Fb/things/" + id, http.StatusNoContent,
		},
		{
			"a slug carrying an encoded slash, with a bad id",
			"/api/v1/w/a%2Fb/things/not-a-uuid", http.StatusBadRequest,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", c.path, nil))
			if w.Code != c.want {
				t.Errorf("%s = %d, want %d", c.path, w.Code, c.want)
			}
		})
	}
}

// A UUID NEEDS NO ESCAPING, SO AN ESCAPED ONE IS STILL A UUID.
//
// Validating the raw segment would refuse it — every character of a UUID is
// unreserved, so nothing legitimate arrives encoded, but nothing stops a client
// encoding it anyway and the handler will see the decoded form regardless.
func TestAPercentEncodedUUIDIsAccepted(t *testing.T) {
	const id = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	h := muxFor("GET /api/v1/things/{thing_id}")
	// The same UUID with its leading '3' written as %33. Spelled by
	// substitution rather than retyped: the first attempt at this constant was
	// a hand-copied UUID with a digit added and another dropped, and it failed
	// the test for a reason that had nothing to do with the code.
	encoded := "%33" + id[1:]

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/things/"+encoded, nil))
	if w.Code != http.StatusNoContent {
		t.Errorf("an escaped UUID = %d, want %d: the raw segment was validated "+
			"instead of the value the handler will read", w.Code, http.StatusNoContent)
	}
}
