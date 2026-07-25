//go:build integration

// Package integration drives the fully-wired application (app.New) against real
// infrastructure (Postgres/Redis/NATS, plus optional MinIO/Meilisearch) over an
// httptest server. Run with:
//
//	go test -tags=integration ./test/integration/...
//
// It requires a migrated database; CI runs `cmd/migrate` first.
//
// If the infrastructure is unreachable the suite skips locally but FAILS under
// CI (CI=true, or SUPEROPS_REQUIRE_INFRA=1 to force it anywhere). Skipping
// everywhere is how the suite silently went green for months on a Redis
// password mismatch.
//
// There is no per-test transaction and no teardown of rows: the app is shared
// (one sync.Once for the whole package) and tearing its data down would race
// every other test against it. Isolation is by construction instead — every
// name, slug and email a test creates is timestamp-suffixed via uniqueSlug, and
// every cross-tenant assertion is made against workspaces the test created
// itself rather than against whatever happens to be in the database.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Wick-Lim/SuperOps/backend/internal/app"
	"github.com/Wick-Lim/SuperOps/backend/internal/mail"
	"github.com/Wick-Lim/SuperOps/backend/internal/search"
	"github.com/Wick-Lim/SuperOps/backend/pkg/logger"
)

type harness struct {
	app  *app.App
	srv  *httptest.Server
	base string

	// search is a second client onto the same Meilisearch index the app reads
	// through. The API never writes to the index — cmd/worker does, from the
	// JetStream event stream — so a search test running without a worker has to
	// index its own fixtures, exactly as the worker's indexer would. It is nil
	// when search is disabled (which is how CI runs), and the search tests skip.
	search *search.Service
}

var (
	once       sync.Once
	shared     *harness
	setupErr   error
	adminEmail = "admin@company.com"
	adminPass  = "changeme_admin_password"
)

// env returns the environment value for k, honouring a set-but-empty variable
// as an explicit empty value.
//
// It used to treat the empty string as unset. CI sets REDIS_PASSWORD to an
// empty value because its redis service runs without requirepass, so the
// harness substituted the compose default, go-redis sent AUTH, Redis replied
// "ERR Client sent AUTH, but no password is set", app.New failed and every test
// skipped — with a green job. MEILI_HOST had the same problem.
func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

// envInt is env for a numeric setting. An unparseable value falls back to the
// default rather than silently becoming 0, which would be a valid-looking port.
func envInt(k string, def int) int {
	raw, ok := os.LookupEnv(k)
	if !ok || raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

// requireInfra reports whether unreachable infrastructure must fail the suite
// instead of skipping it. Every CI provider sets CI=true;
// SUPEROPS_REQUIRE_INFRA forces the same behaviour (or opts out of it) anywhere.
func requireInfra() bool {
	if v, ok := os.LookupEnv("SUPEROPS_REQUIRE_INFRA"); ok {
		b, err := strconv.ParseBool(v)
		return err == nil && b
	}
	b, err := strconv.ParseBool(os.Getenv("CI"))
	return err == nil && b
}

// TestMain tears the shared app down after the package finishes. Without it the
// pgx pool, the Redis client, the NATS connection and the hub goroutine outlive
// the suite, which -race and leak-sensitive tooling both notice.
//
// It used to also read the authz dual-run comparison counter here — the only
// place it could be read after EVERY test, including the tenancy regressions.
// That counter is gone with the legacy checker it compared against
// (docs/plans/00-permissions.md step 5); see authz_test.go for what replaced it
// and why a zero from an emptied comparison is worse than no number at all.
func TestMain(m *testing.M) {
	code := m.Run()
	if shared != nil {
		shared.srv.Close()
		shared.app.Close()
	}
	os.Exit(code)
}

func buildConfig() *app.Config {
	cfg := &app.Config{LogLevel: "error"}
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.Port = 0
	cfg.Server.ReadTimeout = 15 * time.Second
	cfg.Server.WriteTimeout = 15 * time.Second
	cfg.Server.IdleTimeout = 60 * time.Second
	cfg.DB.Host = env("DB_HOST", "localhost")
	// DB_PORT was hardcoded to 5432 and the CI env var that sets it was
	// decorative. Any environment whose Postgres is not on the default port
	// (a local instance alongside a system one, a tunnel) could not run the
	// suite at all.
	cfg.DB.Port = envInt("DB_PORT", 5432)
	cfg.DB.User = env("DB_USER", "superops")
	cfg.DB.Password = env("DB_PASSWORD", "changeme_db_password")
	cfg.DB.Name = env("DB_NAME", "superops")
	cfg.DB.SSLMode = env("DB_SSLMODE", "disable")
	cfg.DB.MaxConns = 10
	cfg.DB.MinConns = 2
	cfg.Redis.Addr = env("REDIS_ADDR", "localhost:6379")
	cfg.Redis.Password = env("REDIS_PASSWORD", "changeme_redis_password")
	cfg.Redis.DB = envInt("REDIS_DB", 0)
	cfg.NATS.URL = env("NATS_URL", "nats://localhost:4222")
	cfg.JWT.Secret = env("JWT_SECRET", "changeme_jwt_secret_at_least_32_chars_long")
	cfg.JWT.AccessTokenTTL = 15 * time.Minute
	cfg.JWT.RefreshTokenTTL = 720 * time.Hour
	cfg.NATS.DrainTimeout = 10 * time.Second
	// Optional services — left at defaults; app.New degrades gracefully if down.
	// Enabled must be set explicitly: this literal never goes through
	// LoadConfig, so the SEARCH_ENABLED/FILES_ENABLED defaults (both true) do
	// not apply and MinIO.IsEnabled()/Meili.IsEnabled() would be false for
	// every run, silently unregistering the file and search routes.
	cfg.MinIO.Enabled = true
	cfg.Meili.Enabled = true
	cfg.MinIO.Endpoint = env("MINIO_ENDPOINT", "localhost:19000")
	cfg.MinIO.AccessKey = env("MINIO_ACCESS_KEY", "minioadmin")
	cfg.MinIO.SecretKey = env("MINIO_SECRET_KEY", "changeme_minio_password")
	cfg.MinIO.Bucket = env("MINIO_BUCKET", "superops")
	cfg.Meili.Host = env("MEILI_HOST", "http://localhost:7700")
	cfg.Meili.MasterKey = env("MEILI_MASTER_KEY", "changeme_meili_master_key")
	// Mail. This literal never goes through LoadConfig, so nothing here has a
	// default: an unset PublicBaseURL fails NewMailRenderer and takes app.New
	// with it. The log transport is what a fresh deployment gets, and it is what
	// the suite exercises — the queue publish is the part that is worth
	// integrating, not the relay conversation (internal/mail tests that against
	// a real listener).
	cfg.Mail.Transport = mail.TransportLog
	cfg.Mail.From = "no-reply@superops.test"
	cfg.Mail.FromName = "SuperOps"
	cfg.Mail.PublicBaseURL = env("PUBLIC_BASE_URL", "http://127.0.0.1:8080")
	cfg.Mail.ProductName = "SuperOps"
	cfg.Mail.Timeout = 30 * time.Second
	cfg.Mail.TestPerMinute = 60
	cfg.Admin.Email = adminEmail
	cfg.Admin.Password = adminPass
	cfg.Admin.Username = "admin"
	cfg.RateLimit.Enabled = false // don't throttle the rapid test traffic
	cfg.CORS.AllowedOrigins = []string{"*"}
	return cfg
}

// getHarness builds the shared app+server once. If the infrastructure (or the
// schema) is not available it fails under CI and skips otherwise; see
// requireInfra.
func getHarness(t *testing.T) *harness {
	t.Helper()
	once.Do(func() {
		cfg := buildConfig()
		l := logger.New("error")
		ctx := context.Background()
		application, err := app.New(ctx, cfg, l)
		if err != nil {
			setupErr = fmt.Errorf("app.New: %w", err)
			return
		}
		// Verify the schema is present (migrations applied).
		if _, err := application.DB.Exec(ctx, "SELECT 1 FROM users LIMIT 1"); err != nil {
			setupErr = fmt.Errorf("schema not migrated (run cmd/migrate): %w", err)
			return
		}
		srv := httptest.NewServer(application.Server.Handler)
		shared = &harness{app: application, srv: srv, base: srv.URL}

		// Mirror what app.New decided about search, so searchEnabled and the
		// fixture indexer agree with whether the route is even registered.
		if cfg.Meili.IsEnabled() {
			if svc, serr := search.NewService(cfg.Meili.Host, cfg.Meili.MasterKey, l); serr == nil {
				shared.search = svc
			}
		}
	})
	if setupErr != nil {
		if requireInfra() {
			t.Fatalf("integration infrastructure unavailable: %v", setupErr)
		}
		t.Skipf("integration infra unavailable: %v", setupErr)
	}
	return shared
}

// --- HTTP helpers ---

type apiResp struct {
	Data json.RawMessage `json:"data"`
	Meta *struct {
		Cursor  string `json:"cursor"`
		HasMore bool   `json:"has_more"`
	} `json:"meta"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (h *harness) do(t *testing.T, method, path, token string, body any) (int, apiResp) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.base+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var ar apiResp
	if len(raw) > 0 && (raw[0] == '{' || raw[0] == '[') {
		_ = json.Unmarshal(raw, &ar)
	}
	return res.StatusCode, ar
}

// req is do plus a status assertion. Almost every call in this suite has one
// expected status, and spelling the check out each time buried the actual
// assertions.
func (h *harness) req(t *testing.T, want int, method, path, token string, body any) apiResp {
	t.Helper()
	code, r := h.do(t, method, path, token, body)
	if code != want {
		t.Fatalf("%s %s = %d, want %d (error=%v)", method, path, code, want, r.Error)
	}
	return r
}

// denied asserts that a call is refused with want, and that the refusal is an
// envelope carrying an error code rather than an empty body.
func (h *harness) denied(t *testing.T, want int, method, path, token string, body any) {
	t.Helper()
	code, r := h.do(t, method, path, token, body)
	if code != want {
		t.Errorf("%s %s = %d, want %d (data=%s)", method, path, code, want, string(r.Data))
		return
	}
	if r.Error == nil || r.Error.Code == "" {
		t.Errorf("%s %s: %d carried no error code", method, path, code)
	}
}

func decodeInto(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode %q: %v", string(raw), err)
	}
}

// --- identities ---

// adminToken caches the seeded admin's access token for the whole package.
// Logging in costs a bcrypt verify at cost 12; the suite does it dozens of
// times per run otherwise.
var (
	adminTokMu sync.Mutex
	adminTok   string
)

func (h *harness) adminToken(t *testing.T) string {
	t.Helper()
	adminTokMu.Lock()
	defer adminTokMu.Unlock()
	if adminTok == "" {
		adminTok = h.login(t, adminEmail, adminPass)
	}
	return adminTok
}

func (h *harness) login(t *testing.T, email, pass string) string {
	t.Helper()
	code, r := h.do(t, "POST", "/api/v1/auth/login", "", map[string]string{"email": email, "password": pass})
	if code != 200 {
		t.Fatalf("login %s: status %d (%v)", email, code, r.Error)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(r.Data, &tok); err != nil || tok.AccessToken == "" {
		t.Fatalf("login %s: no access token (%v)", email, err)
	}
	return tok.AccessToken
}

// firstWorkspace returns the seeded 'superops' workspace. It resolves by slug
// rather than by position: tests create workspaces of their own, and
// GET /workspaces orders by name, so index 0 is not stable.
func (h *harness) firstWorkspace(t *testing.T, token string) string {
	t.Helper()
	r := h.req(t, http.StatusOK, "GET", "/api/v1/workspaces", token, nil)
	var ws []struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}
	decodeInto(t, r.Data, &ws)
	if len(ws) == 0 {
		t.Fatal("no workspace for admin (seedAdmin should have created 'superops')")
	}
	for _, w := range ws {
		if w.Slug == "superops" {
			return w.ID
		}
	}
	return ws[0].ID
}

// userID reports who a token belongs to.
func (h *harness) userID(t *testing.T, token string) string {
	t.Helper()
	r := h.req(t, http.StatusOK, "GET", "/api/v1/users/me", token, nil)
	var u struct {
		ID string `json:"id"`
	}
	decodeInto(t, r.Data, &u)
	if u.ID == "" {
		t.Fatalf("/users/me returned no id: %s", string(r.Data))
	}
	return u.ID
}

// actor is a test identity: an account plus the credentials to act as it.
type actor struct {
	id      string
	email   string
	pass    string
	token   string
	refresh string
}

const actorPassword = "integration-pass-123"

// invite creates an invitation and returns its single-use token.
//
// POST /admin/invitations now requires workspace_id in the body and the caller
// must administer that workspace: it used to pick a workspace with an unordered
// `LIMIT 1`, which could be one the caller was only a plain member of. The
// response no longer contains the token either — only an invite_url — because
// the token is stored as a hash and shown exactly once.
func (h *harness) invite(t *testing.T, inviterToken, workspaceID, email, role string) string {
	t.Helper()
	r := h.req(t, http.StatusCreated, "POST", "/api/v1/admin/invitations", inviterToken,
		map[string]string{"workspace_id": workspaceID, "email": email, "role": role})
	var inv struct {
		InviteURL   string `json:"invite_url"`
		WorkspaceID string `json:"workspace_id"`
		Token       string `json:"token"`
	}
	decodeInto(t, r.Data, &inv)
	if inv.Token != "" {
		t.Error("create invitation leaked the raw token in its response body")
	}
	if inv.WorkspaceID != workspaceID {
		t.Errorf("invitation workspace_id = %q, want %q", inv.WorkspaceID, workspaceID)
	}
	tok := strings.TrimPrefix(inv.InviteURL, "/invite/")
	if tok == "" || tok == inv.InviteURL {
		t.Fatalf("invitation invite_url = %q, want /invite/<token>", inv.InviteURL)
	}
	return tok
}

// newUser mints a brand-new account inside workspaceID.
func (h *harness) newUser(t *testing.T, inviterToken, workspaceID, prefix string) *actor {
	t.Helper()
	n := time.Now().UnixNano()
	a := &actor{
		email: fmt.Sprintf("%s-%d@demo.local", prefix, n),
		pass:  actorPassword,
	}
	tok := h.invite(t, inviterToken, workspaceID, a.email, "member")

	r := h.req(t, http.StatusCreated, "POST", "/api/v1/auth/accept-invite", "", map[string]string{
		"token":     tok,
		"username":  fmt.Sprintf("%s%d", prefix, n),
		"password":  a.pass,
		"full_name": prefix,
	})
	var tp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	decodeInto(t, r.Data, &tp)
	if tp.AccessToken == "" || tp.RefreshToken == "" {
		t.Fatalf("accept-invite returned no token pair: %s", string(r.Data))
	}
	a.token, a.refresh = tp.AccessToken, tp.RefreshToken
	a.id = h.userID(t, a.token)
	return a
}

// joinWorkspace admits an existing account to another workspace. The invite is
// accepted with the invitee's own bearer token, which is how accept-invite
// proves an already-registered address belongs to the caller.
func (h *harness) joinWorkspace(t *testing.T, inviterToken, workspaceID string, a *actor) {
	t.Helper()
	tok := h.invite(t, inviterToken, workspaceID, a.email, "member")
	r := h.req(t, http.StatusCreated, "POST", "/api/v1/auth/accept-invite", a.token,
		map[string]string{"token": tok})
	var tp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	decodeInto(t, r.Data, &tp)
	if tp.AccessToken != "" {
		a.token, a.refresh = tp.AccessToken, tp.RefreshToken
	}
}

// tenant is one isolated organisation: an owner and the workspace they own.
type tenant struct {
	*actor
	workspaceID string
}

// newTenant builds an account that belongs to exactly one workspace, which it
// owns. Every new account necessarily starts life inside the inviting
// workspace, so the seed membership is dropped again afterwards — two tenants
// built this way share nothing at all, which is what makes a cross-tenant
// assertion mean something.
func (h *harness) newTenant(t *testing.T, prefix string) *tenant {
	t.Helper()
	admin := h.adminToken(t)
	seed := h.firstWorkspace(t, admin)

	a := h.newUser(t, admin, seed, prefix)

	slug := uniqueSlug(prefix + "ws")
	r := h.req(t, http.StatusCreated, "POST", "/api/v1/workspaces", a.token,
		map[string]string{"name": slug, "slug": slug})
	var ws struct {
		ID      string `json:"id"`
		OwnerID string `json:"owner_id"`
	}
	decodeInto(t, r.Data, &ws)
	if ws.ID == "" {
		t.Fatalf("create workspace: empty id (%s)", string(r.Data))
	}
	if ws.OwnerID != a.id {
		t.Fatalf("create workspace: owner_id = %q, want %q", ws.OwnerID, a.id)
	}

	h.req(t, http.StatusOK, "DELETE", "/api/v1/workspaces/"+seed+"/members/"+a.id, admin, nil)

	// The tenant must now be in exactly one workspace, or every "cross-tenant"
	// assertion below is testing the wrong thing.
	list := h.req(t, http.StatusOK, "GET", "/api/v1/workspaces", a.token, nil)
	var mine []struct {
		ID string `json:"id"`
	}
	decodeInto(t, list.Data, &mine)
	if len(mine) != 1 || mine[0].ID != ws.ID {
		t.Fatalf("tenant %s is in %d workspaces, want only its own", prefix, len(mine))
	}

	return &tenant{actor: a, workspaceID: ws.ID}
}

// --- channels & messages ---

// createChannel makes a fresh public channel and returns its id.
func (h *harness) createChannel(t *testing.T, token, wsID, slug string) string {
	t.Helper()
	return h.createTypedChannel(t, token, wsID, slug, "public")
}

func (h *harness) createTypedChannel(t *testing.T, token, wsID, slug, chType string) string {
	t.Helper()
	r := h.req(t, http.StatusCreated, "POST", "/api/v1/workspaces/"+wsID+"/channels", token,
		map[string]string{"name": slug, "slug": slug, "type": chType})
	var ch struct {
		ID string `json:"id"`
	}
	decodeInto(t, r.Data, &ch)
	if ch.ID == "" {
		t.Fatal("create channel: empty id")
	}
	return ch.ID
}

// postedMessage is the subset of a message the tests assert on.
type postedMessage struct {
	ID        string    `json:"id"`
	ChannelID string    `json:"channel_id"`
	UserID    string    `json:"user_id"`
	Content   string    `json:"content"`
	IsEdited  bool      `json:"is_edited"`
	IsPinned  bool      `json:"is_pinned"`
	CreatedAt time.Time `json:"created_at"`
	Reactions []struct {
		Emoji string `json:"emoji"`
	} `json:"reactions"`
}

func (h *harness) postMessage(t *testing.T, token, channelID, content string) postedMessage {
	t.Helper()
	r := h.req(t, http.StatusCreated, "POST", "/api/v1/channels/"+channelID+"/messages", token,
		map[string]string{"content": content})
	var m postedMessage
	decodeInto(t, r.Data, &m)
	if m.ID == "" || m.Content != content {
		t.Fatalf("send message returned %+v", m)
	}
	return m
}

func (h *harness) getMessage(t *testing.T, token, channelID, messageID string) postedMessage {
	t.Helper()
	r := h.req(t, http.StatusOK, "GET", "/api/v1/channels/"+channelID+"/messages/"+messageID, token, nil)
	var m postedMessage
	decodeInto(t, r.Data, &m)
	return m
}

func uniqueSlug(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// --- search fixtures ---

// requireSearch skips a test when search is not wired. CI deliberately runs
// with MEILI_HOST="" (no Meilisearch service), which unregisters the route
// entirely; asserting "the cross-tenant search returned nothing" against a
// 404 would be a test that can never fail.
func (h *harness) requireSearch(t *testing.T) {
	t.Helper()
	if h.search == nil {
		t.Skip("search disabled (MEILI_HOST unset or Meilisearch unreachable)")
	}
}

// indexMessage puts a message into the search index the way cmd/worker's
// indexer would, and removes it again when the test ends. Nothing in the API
// process writes to the index, so without this every search assertion would be
// made against an empty index.
func (h *harness) indexMessage(t *testing.T, workspaceID string, m postedMessage) {
	t.Helper()
	if h.search == nil {
		t.Fatal("indexMessage called with search disabled")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := h.search.IndexMessageAwait(ctx, search.MessageDoc{
		ID:          m.ID,
		ChannelID:   m.ChannelID,
		WorkspaceID: workspaceID,
		UserID:      m.UserID,
		Content:     m.Content,
		CreatedAt:   m.CreatedAt.Unix(),
	})
	if err != nil {
		t.Fatalf("index fixture message %s: %v", m.ID, err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer ccancel()
		_ = h.search.DeleteMessageAwait(cctx, m.ID)
	})
}

// searchHits runs a search as token and returns the message ids it matched.
func (h *harness) searchHits(t *testing.T, token, workspaceID, query string) []string {
	t.Helper()
	r := h.req(t, http.StatusOK, "GET",
		"/api/v1/workspaces/"+workspaceID+"/search?q="+query, token, nil)
	var res struct {
		Hits []struct {
			ID string `json:"id"`
		} `json:"hits"`
	}
	decodeInto(t, r.Data, &res)
	ids := make([]string, 0, len(res.Hits))
	for _, hit := range res.Hits {
		ids = append(ids, hit.ID)
	}
	return ids
}

// --- WebSocket ---

// wsScheme converts the httptest http URL to a ws URL for the WS endpoint.
func (h *harness) wsURL(token string) string {
	u := strings.Replace(h.base, "http://", "ws://", 1)
	return u + "/api/v1/ws?token=" + token
}

type wsFrame struct {
	typ  string
	seq  int64
	data json.RawMessage
}

// wsClient is a connected test client with a background reader.
//
// The reader exists because the assertions have to survive an arbitrary
// interleaving of server pushes. The previous tests asserted on the very NEXT
// frame, so anything the server learned to send unprompted — the subscribed
// ack, unread.update, presence.changed, a member event — turned a passing test
// into a flaky one. Frames are buffered here and matched by type instead.
//
// It also verifies the per-connection sequence number, which is the client's
// only way to detect a frame dropped by backpressure: seq must be strictly
// increasing on every frame, forever.
type wsClient struct {
	t      *testing.T
	conn   *websocket.Conn
	frames chan wsFrame
	errc   chan error
	cancel context.CancelFunc
}

func (h *harness) dialWS(t *testing.T, token string) *wsClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	dialCtx, dialCancel := context.WithTimeout(ctx, 15*time.Second)
	defer dialCancel()
	conn, _, err := websocket.Dial(dialCtx, h.wsURL(token), nil)
	if err != nil {
		cancel()
		t.Fatalf("WS dial failed (upgrade broken?): %v", err)
	}

	c := &wsClient{
		t:      t,
		conn:   conn,
		frames: make(chan wsFrame, 256),
		errc:   make(chan error, 1),
		cancel: cancel,
	}
	go c.read(ctx)
	t.Cleanup(c.close)
	return c
}

func (c *wsClient) read(ctx context.Context) {
	var lastSeq int64
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			select {
			case c.errc <- err:
			default:
			}
			return
		}
		var f struct {
			Type string          `json:"type"`
			Seq  int64           `json:"seq"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(data, &f); err != nil {
			c.t.Errorf("ws frame unmarshal: %v (%s)", err, string(data))
			return
		}
		if f.Seq <= lastSeq {
			c.t.Errorf("ws seq went backwards: %d after %d (frame %s)", f.Seq, lastSeq, f.Type)
		}
		lastSeq = f.Seq
		select {
		case c.frames <- wsFrame{typ: f.Type, seq: f.Seq, data: f.Data}:
		case <-ctx.Done():
			return
		}
	}
}

func (c *wsClient) close() {
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
	c.cancel()
}

func (c *wsClient) send(v any) {
	c.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		c.t.Fatalf("ws marshal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.conn.Write(ctx, websocket.MessageText, b); err != nil {
		c.t.Fatalf("ws write: %v", err)
	}
}

func (c *wsClient) subscribe(channelID string) {
	c.t.Helper()
	c.send(map[string]any{"type": "subscribe", "data": map[string]string{"channel_id": channelID}})
}

// waitFor returns the first buffered or incoming frame whose type is in want,
// discarding everything else. Anything not named is server noise this
// assertion does not care about.
func (c *wsClient) waitFor(timeout time.Duration, want ...string) wsFrame {
	c.t.Helper()
	f, ok := c.poll(timeout, want...)
	if !ok {
		c.t.Fatalf("no %v frame within %s", want, timeout)
	}
	return f
}

// expectNone fails if a frame of any named type arrives within d. Used to prove
// a revoked subscription really stops delivering.
func (c *wsClient) expectNone(d time.Duration, unwanted ...string) {
	c.t.Helper()
	if f, ok := c.poll(d, unwanted...); ok {
		c.t.Fatalf("received unexpected %s frame: %s", f.typ, string(f.data))
	}
}

func (c *wsClient) poll(d time.Duration, types ...string) (wsFrame, bool) {
	c.t.Helper()
	wanted := make(map[string]bool, len(types))
	for _, ty := range types {
		wanted[ty] = true
	}
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	for {
		select {
		case f := <-c.frames:
			if wanted[f.typ] {
				return f, true
			}
		case err := <-c.errc:
			c.t.Fatalf("ws read: %v", err)
		case <-deadline.C:
			return wsFrame{}, false
		}
	}
}
