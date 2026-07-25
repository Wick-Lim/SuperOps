package sso

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/audit"
	"github.com/Wick-Lim/SuperOps/backend/internal/auth"
	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/user"
	cryptopkg "github.com/Wick-Lim/SuperOps/backend/pkg/crypto"
)

// seq keeps every test's slugs, usernames and email addresses distinct. The
// database is shared by the whole package and users.email is globally unique,
// so isolation is by construction rather than by teardown — the same approach
// test/integration takes.
var seq atomic.Int64

func unique(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), seq.Add(1))
}

// testEnv is one workspace with one configured provider, wired to the real
// auth.Service so that everything SSO must not bypass — deactivation, the TOTP
// enrolment, session issuance — is exercised against the real implementation
// rather than a double that agrees with it.
type testEnv struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool

	repo *Repository
	svc  *Service
	auth *auth.Service
	az   *authz.Checker
	idp  *fakeIDP
	cfg  Config

	workspaceID   string
	workspaceSlug string
	ownerID       string
	provider      *Provider
}

// testSecretKey is a fixed 32-byte AES key. Fixed on purpose: a random key per
// run would hide a bug where a secret sealed by one process cannot be opened by
// the next.
var testSecretKey = []byte("0123456789abcdef0123456789abcdef")

func newEnv(t *testing.T, configure func(*SaveProviderInput)) *testEnv {
	t.Helper()

	pool := testDB(t)
	idp := newFakeIDP(t)

	cfg := Config{
		SecretKey: testSecretKey,
		// The provider is on 127.0.0.1 over plain http, which production
		// refuses. Allowing it here is also the only coverage the switch gets.
		AllowInsecureIssuer: true,
	}.Defaults()

	client := NewClient(cfg)
	// Rotation is picked up by refetching on an unknown kid; the throttle that
	// makes that safe in production would make it untestable in a run that
	// takes milliseconds.
	client.minRefresh = 0

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	repo := NewRepository(pool)
	az := authz.New(pool)
	auditSvc := audit.NewService(pool, logger)

	authSvc := auth.NewService(
		auth.NewRepository(pool),
		user.NewRepository(pool),
		pool,
		auth.NewJWTManager(strings.Repeat("test-jwt-secret-", 4), 15*time.Minute),
		24*time.Hour,
		auditSvc,
		auth.WithAuthz(az),
	)

	env := &testEnv{
		t:    t,
		ctx:  context.Background(),
		pool: pool,
		repo: repo,
		svc:  NewService(repo, client, authSvc, auditSvc, cfg, logger),
		auth: authSvc,
		az:   az,
		idp:  idp,
		cfg:  cfg,
	}

	env.ownerID = env.createUser(unique("owner")+"@example.com", "owner-password")
	env.workspaceSlug = unique("ws")
	env.workspaceID = env.createWorkspace(env.workspaceSlug, env.ownerID)
	env.addMember(env.workspaceID, env.ownerID, authz.RoleOwner)

	sealed, err := sealSecret(testSecretKey, idp.clientSecret)
	if err != nil {
		t.Fatalf("seal client secret: %v", err)
	}
	save := SaveProviderInput{
		WorkspaceID:             env.workspaceID,
		Name:                    "Test IdP",
		Issuer:                  idp.issuer(),
		ClientID:                idp.clientID,
		ClientSecretEnc:         sealed,
		RedirectURI:             "https://app.example.com/sso/callback",
		Scopes:                  "openid email profile",
		Enabled:                 true,
		AllowOwnerPasswordLogin: true,
		AllowJIT:                true,
		AllowLinking:            true,
		RequireVerifiedEmail:    true,
		DefaultRole:             authz.RoleMember,
	}
	if configure != nil {
		configure(&save)
	}
	provider, err := repo.SaveProvider(env.ctx, save)
	if err != nil {
		t.Fatalf("save provider: %v", err)
	}
	env.provider = provider

	return env
}

// signIn drives a whole round trip: start, "authenticate" at the provider, come
// back. spec is how a test bends the assertion it gets back.
func (e *testEnv) signIn(spec issueSpec) (*LoginResult, error) {
	e.t.Helper()

	start, err := e.svc.Start(e.ctx, e.workspaceSlug)
	if err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	nonce, challenge := e.parseAuthorizeURL(start.AuthorizationURL)
	code := e.idp.authorize(nonce, challenge, spec)
	return e.svc.Callback(e.ctx, start.State, code, "test-agent", "203.0.113.10:4444")
}

func (e *testEnv) parseAuthorizeURL(raw string) (nonce, challenge string) {
	e.t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		e.t.Fatalf("parse authorization url: %v", err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" {
		e.t.Fatalf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
	}
	if q.Get("response_type") != "code" {
		e.t.Fatalf("response_type = %q, want code", q.Get("response_type"))
	}
	return q.Get("nonce"), q.Get("code_challenge")
}

// hashCache memoizes bcrypt across fixtures. Cost 12 takes ~300ms under -race
// and every test builds a fixture user; hashing the same three passwords once
// takes the package from minutes to seconds. Verification is not cached — that
// is the part under test.
var (
	hashMu    sync.Mutex
	hashCache = map[string]string{}
)

func cachedHash(t *testing.T, password string) string {
	t.Helper()
	hashMu.Lock()
	defer hashMu.Unlock()
	if h, ok := hashCache[password]; ok {
		return h
	}
	h, err := cryptopkg.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	hashCache[password] = h
	return h
}

func (e *testEnv) createUser(email, password string) string {
	e.t.Helper()
	id := uuid.NewString()
	var hash any
	if password != "" {
		hash = cachedHash(e.t, password)
	}
	if _, err := e.pool.Exec(e.ctx,
		`INSERT INTO users (id, email, username, full_name, password_hash, is_active)
		 VALUES ($1, $2, $3, $4, $5, TRUE)`,
		id, email, unique("u"), "Test User", hash); err != nil {
		e.t.Fatalf("create user: %v", err)
	}
	return id
}

func (e *testEnv) createWorkspace(slug, ownerID string) string {
	e.t.Helper()
	var id string
	if err := e.pool.QueryRow(e.ctx,
		`INSERT INTO workspaces (name, slug, owner_id) VALUES ($1, $1, $2) RETURNING id`,
		slug, ownerID).Scan(&id); err != nil {
		e.t.Fatalf("create workspace: %v", err)
	}
	return id
}

func (e *testEnv) addMember(workspaceID, userID, role string) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx,
		`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, $3)
		 ON CONFLICT (workspace_id, user_id) DO NOTHING`,
		workspaceID, userID, role); err != nil {
		e.t.Fatalf("add member: %v", err)
	}
}

func (e *testEnv) deactivate(userID string) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, `UPDATE users SET is_active = FALSE WHERE id = $1`, userID); err != nil {
		e.t.Fatalf("deactivate user: %v", err)
	}
}

// enrolTOTP turns 2FA on for a user and returns a usable backup code.
//
// A backup code rather than a generated TOTP code because it exercises the same
// verifyTOTPOrBackup path with no clock dependency; the TOTP branch itself is
// covered by internal/auth's own tests.
func (e *testEnv) enrolTOTP(userID string) string {
	e.t.Helper()
	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		e.t.Fatalf("generate totp secret: %v", err)
	}
	if _, err := e.pool.Exec(e.ctx,
		`UPDATE users SET totp_secret = $2, totp_enabled = TRUE WHERE id = $1`, userID, secret); err != nil {
		e.t.Fatalf("enable totp: %v", err)
	}

	code := "recovery12"
	hash := cachedHash(e.t, code)
	if _, err := e.pool.Exec(e.ctx,
		`INSERT INTO totp_backup_codes (user_id, code_hash) VALUES ($1, $2)`, userID, hash); err != nil {
		e.t.Fatalf("insert backup code: %v", err)
	}
	return code
}

func (e *testEnv) userIDForEmail(email string) string {
	e.t.Helper()
	var id string
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM users WHERE lower(email) = lower($1)`, email).Scan(&id); err != nil {
		e.t.Fatalf("look up %s: %v", email, err)
	}
	return id
}

func (e *testEnv) roleOf(userID string) string {
	e.t.Helper()
	role, err := e.az.WorkspaceRole(e.ctx, e.workspaceID, userID)
	if err != nil {
		e.t.Fatalf("workspace role: %v", err)
	}
	return role
}

func (e *testEnv) countAudit(action, actorID string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx,
		`SELECT count(*) FROM audit_logs WHERE action = $1 AND actor_id = $2`, action, actorID).Scan(&n); err != nil {
		e.t.Fatalf("count audit: %v", err)
	}
	return n
}

// ownerEmail re-reads the address rather than caching it, because one test
// renames the owner to exercise the link challenge against them.
func (e *testEnv) ownerEmail() string {
	e.t.Helper()
	var email string
	if err := e.pool.QueryRow(e.ctx, `SELECT email FROM users WHERE id = $1`, e.ownerID).Scan(&email); err != nil {
		e.t.Fatalf("read owner email: %v", err)
	}
	return email
}

func boolPtr(v bool) *bool { return &v }

func hashForTest(token string) string { return cryptopkg.HashToken(token) }

// renderJSON is how the "no secret can be serialized" assertion is made: by
// marshalling what the handler would marshal.
func renderJSON(t *testing.T, v any) string {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(body)
}

// sessionMethod reports how the session behind a refresh token was obtained.
func (e *testEnv) sessionMethod(refreshToken string) string {
	e.t.Helper()
	var method string
	if err := e.pool.QueryRow(e.ctx,
		`SELECT auth_method FROM sessions WHERE refresh_token_hash = $1`,
		cryptopkg.HashToken(refreshToken)).Scan(&method); err != nil {
		e.t.Fatalf("load session: %v", err)
	}
	return method
}
