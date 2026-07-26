//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A NUL IN A REQUEST BODY IS A BAD REQUEST, NOT A SERVER FAILURE — EVERYWHERE.
//
// U+0000 is legal JSON, encoding/json decodes the escape to a real NUL, and no
// Postgres text or jsonb column can store one. Nothing checked for it, so the
// answer was `500 internal server error` for a body that is simply unstorable.
//
// Measured on seven routes before the guard existed, one byte apart in the same
// request — a NUL versus the letter X. All seven answered 500 on the NUL and
// succeeded on the control, including POSTing a channel message, which is the
// hottest write in the product. The workflow save that surfaced this was the
// LEAST reachable instance.
//
// Each case sends the same body twice. The control is what says the request is
// otherwise well-formed: without it, a route that 400s for an unrelated reason
// would look like a pass.
func TestANulInAnyRequestBodyIsARejectionNotAFailure(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)

	uniq := func(p string) string { return fmt.Sprintf("%s-%d", p, time.Now().UnixNano()) }

	// A channel to post into, created with the control value.
	slug := uniq("nulcase")
	ch := h.req(t, http.StatusCreated, http.MethodPost, "/api/v1/workspaces/"+ws+"/channels", admin,
		map[string]any{"name": slug, "slug": slug, "type": "public"})
	var channel struct {
		ID string `json:"id"`
	}
	decodeInto(t, ch.Data, &channel)

	// %s is where the byte under test goes: a real NUL, then the letter X.
	cases := []struct {
		name   string
		method string
		path   string
		body   func(marker string) map[string]any
		wantOK []int
	}{
		{
			name: "posting a channel message", method: http.MethodPost,
			path: "/api/v1/channels/" + channel.ID + "/messages",
			body: func(m string) map[string]any {
				return map[string]any{"content": "a" + m + "b"}
			},
			wantOK: []int{http.StatusCreated},
		},
		{
			name: "creating a channel", method: http.MethodPost,
			path: "/api/v1/workspaces/" + ws + "/channels",
			body: func(m string) map[string]any {
				s := uniq("nul" + strings.ReplaceAll(m, "\x00", "z"))
				return map[string]any{"name": "n" + m, "slug": s, "type": "public"}
			},
			wantOK: []int{http.StatusCreated},
		},
		{
			name: "creating a Drive folder", method: http.MethodPost,
			path: "/api/v1/workspaces/" + ws + "/drive/folders",
			body: func(m string) map[string]any {
				return map[string]any{"name": uniq("folder") + m}
			},
			wantOK: []int{http.StatusCreated, http.StatusOK},
		},
		{
			name: "changing your own display name", method: http.MethodPatch,
			path: "/api/v1/users/me",
			body: func(m string) map[string]any {
				return map[string]any{"full_name": "Admin" + m}
			},
			wantOK: []int{http.StatusOK},
		},
		{
			name: "creating a workspace", method: http.MethodPost,
			path: "/api/v1/workspaces",
			body: func(m string) map[string]any {
				s := uniq("nulws" + strings.ReplaceAll(m, "\x00", "z"))
				return map[string]any{"name": "w" + m, "slug": s}
			},
			wantOK: []int{http.StatusCreated},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The NUL: a 400 that names the offending field.
			code, body := h.do(t, c.method, c.path, admin, c.body("\x00"))
			raw, _ := json.Marshal(body)
			if code != http.StatusBadRequest {
				t.Errorf("with a NUL = %d, want 400: %s", code, raw)
			}
			// The MESSAGE is not asserted here. Many handlers in this codebase
			// replace a decode error with their own "invalid request body"
			// before it reaches the client — a pre-existing pattern, unrelated
			// to this guard. What the funnel itself says, and that it names the
			// field, is pinned by TestDecodeRejectsNUL in pkg/httputil.
			//
			// What matters on every route is the status: 400 says "fix your
			// request", 500 says "the server is broken", and this input is the
			// former.
			// And no raw NUL byte echoed back into the response.
			if strings.ContainsRune(string(raw), 0) {
				t.Errorf("a raw NUL byte is in the response body: %q", raw)
			}

			// The control: the same request with an ordinary letter succeeds, so
			// the 400 above is about the byte and not about the shape.
			code, body = h.do(t, c.method, c.path, admin, c.body("X"))
			raw, _ = json.Marshal(body)
			ok := false
			for _, w := range c.wantOK {
				if code == w {
					ok = true
				}
			}
			if !ok {
				t.Errorf("the control = %d, want one of %v: %s", code, c.wantOK, raw)
			}
		})
	}
}

// A NUL IN A QUERY PARAMETER IS THE SAME BUG WITH NO FUNNEL TO CATCH IT.
//
// DecodeJSON never sees a query string. net/url turns `%00` into a real NUL and
// the handler hands it to Postgres, so `GET /api/v1/users/search?q=a%00b`
// answered 500 — any authenticated user, one GET, no body. Twelve handlers read
// r.URL.Query() directly rather than through the shared accessor, which is why
// the guard is middleware.
func TestANulInAQueryParameterIsARejectionNotAFailure(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)

	for _, c := range []struct {
		name, path string
		wantOK     int
	}{
		{"searching for a user", "/api/v1/users/search?q=a%sb", http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, body := h.do(t, http.MethodGet, fmt.Sprintf(c.path, "%00"), admin, nil)
			raw, _ := json.Marshal(body)
			if code != http.StatusBadRequest {
				t.Errorf("with a NUL = %d, want 400: %s", code, raw)
			}
			if !strings.Contains(string(raw), "NUL") {
				t.Errorf("the refusal does not say what is wrong: %s", raw)
			}

			code, body = h.do(t, http.MethodGet, fmt.Sprintf(c.path, "X"), admin, nil)
			raw, _ = json.Marshal(body)
			if code != c.wantOK {
				t.Errorf("the control = %d, want %d: %s", code, c.wantOK, raw)
			}
		})
	}

	// A percent sign a caller genuinely wants is `%2500`, and it must still work
	// — the guard matches the encoded NUL, not the three characters.
	code, _ := h.do(t, http.MethodGet, "/api/v1/users/search?q=a%2500b", admin, nil)
	if code != http.StatusOK {
		t.Errorf("a literal %%00 in a query = %d, want 200: the guard is matching "+
			"the characters rather than the encoded byte", code)
	}
}

// A REFUSAL IS AN OBSERVABLE REQUEST, NOT AN INVISIBLE ONE.
//
// RejectNULInURL was assigned last in the chain, which — since each line wraps
// the previous — put it OUTSIDE RequestID, Logging, Metrics and the rate
// limiter. Every refusal came back with an empty X-Request-ID, wrote no access
// log line, incremented no metric, and was not counted against any limit, so
// probing a deployment with %00 URLs left no trace. The comment beside it
// claimed the opposite.
//
// THE HEADER ALONE DOES NOT PROVE IT. An earlier version of this test asserted
// only X-Request-ID, and survived two mutations: moving the guard back outside
// Logging, Metrics and the rate limiter (the header is still set, five refusals
// moved the 4xx counter by zero), and REMOVING the guard entirely (the route
// then answers 500, which carries an id too). So it also asserts the status —
// which says the guard ran — and a movement in the 4xx counter, which says the
// refusal reached Metrics and therefore everything assigned around it.
func TestANulRefusalStillCarriesACorrelationID(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)

	// THIS READS A PROCESS-GLOBAL COUNTER and depends on the package being
	// sequential — nothing here calls t.Parallel(), so nothing else issues a
	// request between the two snapshots. Adding t.Parallel() anywhere in this
	// package would let another test's 4xx cover for a guard contributing
	// nothing, which is the regression this assertion exists to catch.
	before := h.requests4xx(t)

	const refusals = 5
	for i := 0; i < refusals; i++ {
		req, err := http.NewRequest(http.MethodGet, h.base+"/api/v1/users/search?q=a%00b", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+admin)
		res, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		id := res.Header.Get("X-Request-ID")
		status := res.StatusCode
		_ = res.Body.Close()

		if status != http.StatusBadRequest {
			t.Fatalf("the URL guard did not refuse: status %d", status)
		}
		if id == "" {
			t.Errorf("a refusal came back with no X-Request-ID: the guard is " +
				"outside RequestIDMiddleware")
		}
	}

	// The counter is what proves it reached Metrics — and therefore Logging and
	// the rate limiter, which are assigned around it in the same chain.
	if got := h.requests4xx(t) - before; got < refusals {
		t.Errorf("%d refusals moved the 4xx counter by %d: the guard sits outside "+
			"MetricsMiddleware, so its refusals are unlogged, unmetered and not "+
			"counted against any rate limit", refusals, got)
	}
}

// requests4xx reads the 4xx counter out of /metrics.
func (h *harness) requests4xx(t *testing.T) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.base+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, `superops_http_requests_total{status="4xx"}`) {
			continue
		}
		fields := strings.Fields(line)
		n, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil {
			t.Fatalf("could not read the 4xx counter from %q: %v", line, err)
		}
		return n
	}
	t.Fatalf("no 4xx counter in /metrics (status %d)", res.StatusCode)
	return 0
}
