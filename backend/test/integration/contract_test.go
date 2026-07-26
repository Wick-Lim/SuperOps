//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// THE SEAM NO COMPILER CAN SEE.
//
// The client's API paths are hand-written strings and the server's routes are
// hand-written patterns. `tsc` type-checks one side, `go build` the other, and
// nothing checks that they agree — so a client that called
// POST /webhooks/{id}/rotate against a server registering
// PUT /webhooks/{id}/token type-checked, compiled, passed every test, and
// 404'd in production. Token rotation is the only remedy for a leaked webhook
// token, and it was unreachable from the shipped client.
//
// This walks app/src/api/**, extracts every path the client calls, and asserts
// each one is matched by a registered route. It is the cheap half of the check:
// a client call with no route is a broken feature, while a route with no caller
// is only unused.

// api.get<T>('/path'), api.post(`/path/${x}`), api.del('/path'), …
//
// The regex finds the CALL; a scanner reads the literal, because a regex
// cannot. `/projects/${id}/issues${q ? `?${q}` : ”}` nests a template literal
// inside an interpolation, so "up to the closing quote" and "up to the closing
// brace" are both wrong — the first version of this test reported that line as
// a missing route, which is exactly the false positive that gets a check
// deleted rather than trusted.
var clientCallRe = regexp.MustCompile(
	`\bapi\.(get|post|put|patch|del)\s*(?:<[^>]*>)?\s*\(\s*(['"` + "`" + `])`)

// readLiteral returns the string literal starting at src[0], with balanced
// ${...} groups replaced by "{}", and whether it terminated cleanly.
func readLiteral(src string, quote byte) (string, bool) {
	var out strings.Builder
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '\\':
			i++ // escaped: skip whatever follows
		case c == quote:
			return out.String(), true
		case c == '$' && quote == '`' && i+1 < len(src) && src[i+1] == '{':
			depth, j := 0, i+1
			for ; j < len(src); j++ {
				if src[j] == '{' {
					depth++
				} else if src[j] == '}' {
					depth--
					if depth == 0 {
						break
					}
				}
			}
			if depth != 0 {
				return out.String(), false
			}
			out.WriteString("{}")
			i = j
		case c == '\n':
			return out.String(), false
		default:
			out.WriteByte(c)
		}
	}
	return out.String(), false
}

func TestEveryClientAPICallHasARoute(t *testing.T) {
	h := getHarness(t)

	root, err := filepath.Abs("../../../app/src/api")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("the client API directory is not where this test expects it (%s): %v", root, err)
	}

	routes := h.registeredRoutes(t)
	total := 0
	for _, ps := range routes {
		total += len(ps)
	}
	// A floor, so a broken extractor fails loudly instead of vacuously passing
	// by finding nothing to check.
	if total < 100 {
		t.Fatalf("only %d routes were discovered; the extractor is broken, not the server", total)
	}

	var checked int
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".ts") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		text := string(src)
		for _, loc := range clientCallRe.FindAllStringSubmatchIndex(text, -1) {
			method := strings.ToUpper(text[loc[2]:loc[3]])
			if method == "DEL" {
				method = "DELETE"
			}
			quote := text[loc[4]]
			raw, ok := readLiteral(text[loc[5]:], quote)
			if !ok {
				continue // not a plain literal; nothing to check
			}
			// Query strings are not part of the pattern, and a path built
			// entirely from a variable cannot be checked at all.
			raw = strings.SplitN(raw, "?", 2)[0]
			if raw == "" || strings.HasPrefix(raw, "{}") {
				continue
			}
			candidate := strings.TrimSuffix("/api/v1"+raw, "/")
			checked++
			if !matchesAnyRoute(candidate, method, routes) {
				t.Errorf("%s: the client calls %s %s and no route matches it",
					rel, method, candidate)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 80 {
		t.Fatalf("only %d client calls were extracted; the regex is broken, not the client", checked)
	}
	t.Logf("checked %d client calls against %d routes", checked, total)
}

// matchesAnyRoute compares a path whose variable segments are "{}" against the
// server's patterns, whose variable segments are "{name}".
func matchesAnyRoute(path, method string, routes map[string][]string) bool {
	want := strings.Split(strings.Trim(path, "/"), "/")
	for _, pattern := range routes[method] {
		got := strings.Split(strings.Trim(pattern, "/"), "/")
		// A trailing wildcard segment absorbs the rest.
		if len(got) > 0 && got[len(got)-1] == "..." {
			if len(want) >= len(got)-1 {
				return true
			}
			continue
		}
		if len(got) != len(want) {
			continue
		}
		ok := true
		for i := range got {
			serverVar := strings.HasPrefix(got[i], "{")
			// A server pattern variable matches any single client segment.
			if serverVar {
				continue
			}
			// A CLIENT interpolation matches only a server VARIABLE, never a
			// literal. Allowing it to match a literal is how the first version
			// of this test passed against the very bug it was written for:
			// POST /webhooks/{}/rotate matched POST /webhooks/incoming/{token},
			// because "{}" was let through against "incoming". An interpolated
			// id is a uuid; it is never the fixed word the route spells out.
			if want[i] == "{}" {
				ok = false
				break
			}
			if strings.HasSuffix(want[i], "{}") {
				if !strings.HasPrefix(got[i], strings.TrimSuffix(want[i], "{}")) &&
					!strings.HasPrefix(want[i], got[i]) {
					ok = false
					break
				}
				continue
			}
			if got[i] != want[i] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// registeredRoutes reads the patterns out of the source, because ServeMux does
// not enumerate them. Source is the honest place: it is what the server will
// register on the next boot, and it cannot drift from what is running.
func (h *harness) registeredRoutes(t *testing.T) map[string][]string {
	t.Helper()
	handleRe := regexp.MustCompile(`mux\.(?:Handle|HandleFunc)\(\s*"([A-Z]+) ([^"]+)"`)

	out := map[string][]string{}
	root, err := filepath.Abs("../../internal")
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range handleRe.FindAllStringSubmatch(string(src), -1) {
			pattern := m[2]
			// Strip a host prefix if one is ever used, and normalise the
			// trailing-subtree form Go writes as "/".
			if strings.HasSuffix(pattern, "/") && pattern != "/" {
				pattern = strings.TrimSuffix(pattern, "/") + "/..."
			}
			out[m[1]] = append(out[m[1]], pattern)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

var _ = fmt.Sprintf
