//go:build integration

package integration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wick-Lim/SuperOps/backend/pkg/crypto"
)

// A NUL IN A REQUEST BODY IS REMOVED, NOT A SERVER FAILURE — EVERYWHERE.
//
// U+0000 is legal JSON, encoding/json decodes the escape to a real NUL, and no
// Postgres text or jsonb column can store one. Nothing checked for it, so the
// answer was `500 internal server error` for a body that is simply unstorable.
//
// The funnel STRIPS it rather than refusing, because refusing ran before the
// handler sanitisers that already remove every rune below 0x20 — see
// TestStrippingKeepsSanitisedPathsWorking in pkg/httputil for what that broke.
//
// Measured on seven routes before the guard existed, one byte apart in the same
// request — a NUL versus the letter X. All seven answered 500 on the NUL and
// succeeded on the control, including POSTing a channel message, which is the
// hottest write in the product. The workflow save that surfaced this was the
// LEAST reachable instance.
//
// FIVE of those seven are covered below. Editing a message and creating an
// invitation are not, because the guard lives in the shared decode funnel and
// these five already drive every arm of it — said here so the gap is a choice
// rather than something the comment above quietly overstates.
//
// Each case sends the same body twice. The control is what says the request is
// otherwise well-formed: without it, a route that 400s for an unrelated reason
// would look like a pass.
func TestANulInAnyRequestBodyIsStrippedNotAFailure(t *testing.T) {
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
			// The NUL: the request SUCCEEDS with the byte removed. It used to
			// answer 500, and briefly answered 400 — which broke two paths that
			// had working sanitisers, so the funnel strips instead.
			code, body := h.do(t, c.method, c.path, admin, c.body("\x00"))
			raw, _ := json.Marshal(body)
			ok := false
			for _, w := range c.wantOK {
				if code == w {
					ok = true
				}
			}
			if !ok {
				t.Errorf("with a NUL = %d, want one of %v: %s", code, c.wantOK, raw)
			}
			// And the byte did not come back. Searching `raw` for a raw NUL
			// could never fire — json.Marshal writes U+0000 as the six-character
			// escape — so this looks for the escape instead, which is what a
			// stored-and-echoed NUL actually renders as.
			if strings.Contains(string(raw), `\u0000`) {
				t.Errorf("the response echoes a NUL, so it was stored: %s", raw)
			}

			// The control: the same request with an ordinary letter in place of
			// the NUL, so any difference between the two is about that byte and
			// not about the shape of the request.
			code, body = h.do(t, c.method, c.path, admin, c.body("X"))
			raw, _ = json.Marshal(body)
			ctrlOK := false
			for _, w := range c.wantOK {
				if code == w {
					ctrlOK = true
				}
			}
			if !ctrlOK {
				t.Errorf("the control = %d, want one of %v: %s", code, c.wantOK, raw)
			}
		})
	}
}

// UNSTORABLE BYTES IN A QUERY PARAMETER ARE THE SAME BUG WITH NO FUNNEL.
//
// DecodeJSON never sees a query string. net/url turns `%00` into a real NUL and
// the handler hands it to Postgres, so `GET /api/v1/users/search?q=a%00b`
// answered 500 — any authenticated user, one GET, no body. Twelve handlers read
// r.URL.Query() directly rather than through the shared accessor, which is why
// the guard is middleware.
func TestANulInAQueryParameterIsARejectionNotAFailure(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)

	// EVERY SPELLING OF THE CLASS, not one of them. Matching the literal `%00`
	// was right about the byte and wrong about the bug: what Postgres refuses is
	// any byte sequence that is not valid UTF-8, all with SQLSTATE 22021, and
	// `%c0%80` is an overlong encoding of the very character the guard was
	// written for — spelled so a substring match cannot see it.
	for _, c := range []struct {
		name, path, bad string
		wantOK          int
	}{
		{"a NUL", "/api/v1/users/search?q=a%sb", "%00", http.StatusOK},
		{"a lone 0xFF", "/api/v1/users/search?q=a%sb", "%ff", http.StatusOK},
		{"an overlong encoding of NUL", "/api/v1/users/search?q=a%sb", "%c0%80", http.StatusOK},
		{"a truncated 3-byte sequence", "/api/v1/users/search?q=a%sb", "%e2%82", http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, body := h.do(t, http.MethodGet, fmt.Sprintf(c.path, c.bad), admin, nil)
			raw, _ := json.Marshal(body)
			if code != http.StatusBadRequest {
				t.Errorf("with a NUL = %d, want 400: %s", code, raw)
			}
			if !strings.Contains(string(raw), "UTF-8") {
				t.Errorf("the refusal does not say what is wrong: %s", raw)
			}

			code, body = h.do(t, http.MethodGet, fmt.Sprintf(c.path, "X"), admin, nil)
			raw, _ = json.Marshal(body)
			if code != c.wantOK {
				t.Errorf("the control = %d, want %d: %s", code, c.wantOK, raw)
			}
		})
	}

	// SEVEN EXTRA CHARACTERS MUST NOT TURN THE GUARD OFF.
	//
	// url.ParseQuery returns a PARTIAL map alongside its error, and the error is
	// global to the query string: one malformed escape or one semicolon anywhere
	// sets it while every well-formed pair survives in the map. Returning early
	// on that error allowed the whole request, so appending `&x=%zz` sent the
	// poisoned pair straight to the handler — all four encodings above went back
	// to 500.
	for _, suffix := range []string{"&x=%zz", "&x=a;b", "&x=;", "&%zz=1"} {
		for _, bad := range []string{"%00", "%ff", "%c0%80"} {
			path := "/api/v1/users/search?q=a" + bad + "b" + suffix
			code, body := h.do(t, http.MethodGet, path, admin, nil)
			if code != http.StatusBadRequest {
				raw, _ := json.Marshal(body)
				t.Errorf("%s = %d, want 400: a malformed pair elsewhere in the query "+
					"disarmed the guard: %s", path, code, raw)
			}
		}
		// And a clean query with the same suffix is still served, so the fix
		// refuses nothing that worked before.
		if code, _ := h.do(t, http.MethodGet,
			"/api/v1/users/search?q=hello"+suffix, admin, nil); code != http.StatusOK {
			t.Errorf("a clean query with %q = %d, want 200", suffix, code)
		}
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

// THE PATH A REFUSAL BROKE WORST, END TO END.
//
// `mailbox.safeFilename` strips every rune below 0x20 and is documented as
// existing because "the name is attacker-supplied — it comes from an email
// anybody can send". Refusing at the funnel ran BEFORE it, so an inbound
// customer email carrying a NUL in an attachment filename went from being filed
// to being rejected outright: the body, the subject and every other attachment
// went with it, and a sender could make somebody's message disappear by naming a
// file carefully.
//
// The first case is the hostile filename this suite already covers, unchanged.
// The second is that filename plus one byte.
func TestAHostileAttachmentFilenameDoesNotDropTheEmail(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	mb := newMailbox(t, ws, me)
	ctx := context.Background()

	token := fmt.Sprintf("nul-ingest-%d", time.Now().UnixNano())
	if _, err := h.app.DB.Exec(ctx,
		`INSERT INTO mail_ingest_tokens (workspace_id, name, token_hash) VALUES ($1, 'nul', $2)`,
		ws, crypto.HashToken(token)); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ label, filename string }{
		{"the hostile filename this suite already covers", "../../etc/passwd\r\nX-Injected: yes"},
		{"a control character safeFilename strips", "../../etc/passwd\x01\r\nX-Injected: yes"},
		{"the same filename with a NUL in it", "../../etc/passwd\x00\r\nX-Injected: yes"},
	} {
		n := time.Now().UnixNano()
		code, resp := h.doBearer(t, http.MethodPost, "/api/v1/mail/inbound", token,
			map[string]any{
				"event_id": fmt.Sprintf("ev-nul-%d", n), "recipient": mb.Address,
				"message_id": fmt.Sprintf("<nul-%d@customer.test>", n),
				"from":       "customer@customer.test", "subject": "with an attachment",
				"body_text": "please see attached",
				"attachments": []map[string]any{{
					"filename": c.filename,
					"content":  base64.StdEncoding.EncodeToString([]byte("hello")),
				}},
			})
		if code != http.StatusCreated {
			raw, _ := json.Marshal(resp)
			t.Errorf("%s: inbound mail = %d, want 201: %s\n"+
				"a refusal here loses the customer's entire email over one byte in "+
				"a filename that safeFilename exists to strip", c.label, code, raw)
		}
	}
}
