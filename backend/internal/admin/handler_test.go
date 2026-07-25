package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/audit"
	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/mail"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
)

// The fixture is two unrelated tenants.
//
//	alpha  — owned by alphaOwner, administered by alphaAdmin.
//	         members: alphaMember (plain), alphaGuest.
//	beta   — owned by betaOwner. alphaOwner has no membership in it at all.
//
// Every assertion below is "what can an admin of alpha see and do", and the
// answer must never include anything belonging to beta.
type fixture struct {
	alphaWS, betaWS string

	alphaOwner  string
	alphaAdmin  string
	alphaMember string
	alphaGuest  string

	betaOwner  string
	betaMember string
}

var (
	fixOnce sync.Once
	fix     *fixture
	fixErr  error
)

type suite struct {
	pool *pgxpool.Pool
	h    *Handler
	f    *fixture
	mail *fakeMailQueue
}

// fakeMailQueue records what the handler tried to send, so the invitation tests
// can assert on delivery without a NATS server.
type fakeMailQueue struct {
	mu   sync.Mutex
	sent []queuedMail
	err  error
}

type queuedMail struct {
	workspaceID string
	kind        string
	msg         *mail.Message
}

func (q *fakeMailQueue) Queue(_ context.Context, workspaceID, kind string, msg *mail.Message) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return q.err
	}
	q.sent = append(q.sent, queuedMail{workspaceID: workspaceID, kind: kind, msg: msg})
	return nil
}

func (q *fakeMailQueue) all() []queuedMail {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]queuedMail(nil), q.sent...)
}

func setup(t *testing.T) *suite {
	t.Helper()
	pool := testDB(t)
	fixOnce.Do(func() {
		fix, fixErr = buildFixture(context.Background(), pool)
	})
	if fixErr != nil {
		t.Fatalf("seed fixtures: %v", fixErr)
	}
	auditSvc := audit.NewService(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))

	queue := &fakeMailQueue{}
	renderer, err := mail.NewRenderer(mail.RendererConfig{BaseURL: "https://chat.example.com", ProductName: "SuperOps"})
	if err != nil {
		t.Fatalf("build mail renderer: %v", err)
	}
	deps := MailDeps{
		Publisher: queue,
		Renderer:  renderer,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return &suite{pool: pool, h: NewHandler(pool, auditSvc, authz.New(pool), deps), f: fix, mail: queue}
}

func buildFixture(ctx context.Context, pool *pgxpool.Pool) (*fixture, error) {
	f := &fixture{}
	var err error

	user := func(name string) string {
		if err != nil {
			return ""
		}
		var id string
		err = pool.QueryRow(ctx,
			`INSERT INTO users (email, username) VALUES ($1, $2) RETURNING id`,
			name+"@test.local", name).Scan(&id)
		return id
	}
	workspace := func(slug, ownerID string) string {
		if err != nil {
			return ""
		}
		var id string
		err = pool.QueryRow(ctx,
			`INSERT INTO workspaces (name, slug, owner_id) VALUES ($1, $1, $2) RETURNING id`,
			slug, ownerID).Scan(&id)
		return id
	}
	join := func(wsID, userID, role string) {
		if err != nil {
			return
		}
		_, err = pool.Exec(ctx,
			`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, $3)`,
			wsID, userID, role)
	}
	channel := func(wsID, slug string) string {
		if err != nil {
			return ""
		}
		var id string
		err = pool.QueryRow(ctx,
			`INSERT INTO channels (workspace_id, name, slug) VALUES ($1, $2, $2) RETURNING id`,
			wsID, slug).Scan(&id)
		return id
	}

	f.alphaOwner = user("alpha-owner")
	f.alphaAdmin = user("alpha-admin")
	f.alphaMember = user("alpha-member")
	f.alphaGuest = user("alpha-guest")
	f.betaOwner = user("beta-owner")
	f.betaMember = user("beta-member")

	f.alphaWS = workspace("alpha", f.alphaOwner)
	join(f.alphaWS, f.alphaOwner, authz.RoleOwner)
	join(f.alphaWS, f.alphaAdmin, authz.RoleAdmin)
	join(f.alphaWS, f.alphaMember, authz.RoleMember)
	join(f.alphaWS, f.alphaGuest, authz.RoleGuest)

	f.betaWS = workspace("beta", f.betaOwner)
	join(f.betaWS, f.betaOwner, authz.RoleOwner)
	join(f.betaWS, f.betaMember, authz.RoleMember)

	channel(f.alphaWS, "alpha-general")
	channel(f.betaWS, "beta-general")

	if err != nil {
		return nil, err
	}
	return f, nil
}

// --- request plumbing --------------------------------------------------------

type envelope struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// do drives a request through the handler's own routes, so the {user_id} path
// parameter is populated the way net/http populates it in production. The
// "auth middleware" only installs the actor identity — authorization is what is
// under test.
func (e *suite) do(t *testing.T, method, path, actor string, body any) (int, envelope) {
	t.Helper()

	mux := http.NewServeMux()
	authMw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(authctx.WithUserID(r.Context(), actor)))
		})
	}
	e.h.RegisterRoutes(mux, authMw)
	// The mail configuration test carries an extra rate limiter in production;
	// what is under test here is the handler's own authorization, which is the
	// same either way.
	e.h.RegisterMailRoutes(mux, authMw)

	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var body2 envelope
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &body2); err != nil {
			t.Fatalf("%s %s: body is not an envelope: %q", method, path, rec.Body.String())
		}
	}
	if rec.Code >= 400 && (body2.Error == nil || body2.Error.Code == "") {
		t.Errorf("%s %s: %d carried no error code (%q)", method, path, rec.Code, rec.Body.String())
	}
	return rec.Code, body2
}

func (e *suite) expect(t *testing.T, want int, method, path, actor string, body any) envelope {
	t.Helper()
	code, resp := e.do(t, method, path, actor, body)
	if code != want {
		t.Fatalf("%s %s as %s = %d, want %d (error=%+v)", method, path, actor, code, want, resp.Error)
	}
	return resp
}

func decode(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode %q: %v", string(raw), err)
	}
}

// --- scoping -----------------------------------------------------------------

// TestScopeRefusesNonAdmins covers the guard every /admin/* read shares: route
// middleware only proves "administers something somewhere", so the handler
// re-derives the administered set and refuses an empty one.
func TestScopeRefusesNonAdmins(t *testing.T) {
	e := setup(t)

	paths := []string{
		"/api/v1/admin/users",
		"/api/v1/admin/stats",
		"/api/v1/admin/audit-logs",
		"/api/v1/admin/invitations",
	}
	actors := []struct {
		name string
		id   string
	}{
		{"plain member", e.f.alphaMember},
		{"guest", e.f.alphaGuest},
		{"unknown user", missingID},
	}

	for _, p := range paths {
		for _, a := range actors {
			t.Run(p+" as "+a.name, func(t *testing.T) {
				e.expect(t, http.StatusForbidden, http.MethodGet, p, a.id, nil)
			})
		}
	}
}

func TestListUsersIsScopedToAdministeredWorkspaces(t *testing.T) {
	e := setup(t)

	seen := func(actor string) map[string]bool {
		t.Helper()
		resp := e.expect(t, http.StatusOK, http.MethodGet, "/api/v1/admin/users", actor, nil)
		var users []struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		}
		decode(t, resp.Data, &users)
		got := map[string]bool{}
		for _, u := range users {
			got[u.ID] = true
		}
		return got
	}

	alpha := seen(e.f.alphaAdmin)
	for _, id := range []string{e.f.alphaOwner, e.f.alphaAdmin, e.f.alphaMember, e.f.alphaGuest} {
		if !alpha[id] {
			t.Errorf("alpha admin cannot see alpha member %s", id)
		}
	}
	// The regression: an admin of any throwaway workspace used to list every
	// account on the instance.
	for _, id := range []string{e.f.betaOwner, e.f.betaMember} {
		if alpha[id] {
			t.Errorf("alpha admin listed beta account %s", id)
		}
	}

	beta := seen(e.f.betaOwner)
	if !beta[e.f.betaMember] {
		t.Error("beta owner cannot see their own member")
	}
	if beta[e.f.alphaMember] {
		t.Error("beta owner listed an alpha account")
	}
}

func TestStatsCountOnlyAdministeredWorkspaces(t *testing.T) {
	e := setup(t)

	stats := func(actor string) map[string]int {
		t.Helper()
		resp := e.expect(t, http.StatusOK, http.MethodGet, "/api/v1/admin/stats", actor, nil)
		var s map[string]int
		decode(t, resp.Data, &s)
		return s
	}

	alpha := stats(e.f.alphaOwner)
	if alpha["workspaces"] != 1 {
		t.Errorf("alpha owner sees %d workspaces, want 1", alpha["workspaces"])
	}
	if alpha["users"] != 4 {
		t.Errorf("alpha owner sees %d users, want the 4 in alpha", alpha["users"])
	}
	if alpha["channels"] != 1 {
		t.Errorf("alpha owner sees %d channels, want only alpha's", alpha["channels"])
	}

	beta := stats(e.f.betaOwner)
	if beta["workspaces"] != 1 || beta["users"] != 2 || beta["channels"] != 1 {
		t.Errorf("beta owner stats = %+v, want its own tenant only", beta)
	}
}

// --- invitations -------------------------------------------------------------

func TestCreateInvitationValidation(t *testing.T) {
	e := setup(t)

	tests := []struct {
		name string
		body map[string]any
		want int
	}{
		{"missing email", map[string]any{"workspace_id": e.f.alphaWS}, http.StatusBadRequest},
		{"email without an @", map[string]any{"workspace_id": e.f.alphaWS, "email": "nope"}, http.StatusBadRequest},
		// workspace_id is mandatory and must be a uuid: it used to be whichever
		// row an unordered LIMIT 1 returned.
		{"missing workspace", map[string]any{"email": "x@test.local"}, http.StatusBadRequest},
		{"workspace is not a uuid", map[string]any{"workspace_id": "alpha", "email": "x@test.local"}, http.StatusBadRequest},
		{"owner is not an invitable role", map[string]any{"workspace_id": e.f.alphaWS, "email": "x@test.local", "role": authz.RoleOwner}, http.StatusBadRequest},
		{"unknown role", map[string]any{"workspace_id": e.f.alphaWS, "email": "x@test.local", "role": "superuser"}, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.expect(t, tt.want, http.MethodPost, "/api/v1/admin/invitations", e.f.alphaOwner, tt.body)
		})
	}
}

func TestCreateInvitationAuthorization(t *testing.T) {
	e := setup(t)

	tests := []struct {
		name  string
		actor string
		wsID  string
		want  int
	}{
		{"owner invites into their workspace", e.f.alphaOwner, e.f.alphaWS, http.StatusCreated},
		{"admin invites into their workspace", e.f.alphaAdmin, e.f.alphaWS, http.StatusCreated},
		// The caller must administer the workspace they name — not merely
		// administer one somewhere.
		{"alpha owner cannot invite into beta", e.f.alphaOwner, e.f.betaWS, http.StatusForbidden},
		{"plain member cannot invite", e.f.alphaMember, e.f.alphaWS, http.StatusForbidden},
		{"guest cannot invite", e.f.alphaGuest, e.f.alphaWS, http.StatusForbidden},
		{"nobody can invite into a workspace that does not exist", e.f.alphaOwner, missingID, http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := map[string]any{
				"workspace_id": tt.wsID,
				"email":        newEmail(),
			}
			e.expect(t, tt.want, http.MethodPost, "/api/v1/admin/invitations", tt.actor, body)
		})
	}
}

// TestCreateInvitationShowsTheTokenOnce pins the two properties the stored hash
// exists for: the raw token appears only inside invite_url, and never as a
// field a later read could return.
func TestCreateInvitationShowsTheTokenOnce(t *testing.T) {
	e := setup(t)

	email := newEmail()
	resp := e.expect(t, http.StatusCreated, http.MethodPost, "/api/v1/admin/invitations", e.f.alphaOwner,
		map[string]any{"workspace_id": e.f.alphaWS, "email": email, "role": authz.RoleMember})

	var created struct {
		ID          string `json:"id"`
		InviteURL   string `json:"invite_url"`
		Email       string `json:"email"`
		Role        string `json:"role"`
		WorkspaceID string `json:"workspace_id"`
		Token       string `json:"token"`
	}
	decode(t, resp.Data, &created)

	if created.Token != "" {
		t.Error("create invitation returned a raw token field")
	}
	if len(created.InviteURL) <= len("/invite/") {
		t.Errorf("invite_url = %q, want /invite/<token>", created.InviteURL)
	}
	if created.Email != email || created.Role != authz.RoleMember || created.WorkspaceID != e.f.alphaWS {
		t.Errorf("created invitation = %+v, want the values that were asked for", created)
	}

	// Only the hash is persisted, so the same email cannot be invited twice
	// while the first invitation is pending.
	e.expect(t, http.StatusConflict, http.MethodPost, "/api/v1/admin/invitations", e.f.alphaOwner,
		map[string]any{"workspace_id": e.f.alphaWS, "email": email})

	// And the list never carries the token back.
	list := e.expect(t, http.StatusOK, http.MethodGet, "/api/v1/admin/invitations", e.f.alphaOwner, nil)
	var invites []map[string]any
	decode(t, list.Data, &invites)
	found := false
	for _, inv := range invites {
		if _, ok := inv["token"]; ok {
			t.Error("list invitations returned a token field")
		}
		if inv["id"] == created.ID {
			found = true
		}
	}
	if !found {
		t.Error("the invitation just created is missing from the list")
	}
}

func TestListInvitationsIsScopedToAdministeredWorkspaces(t *testing.T) {
	e := setup(t)

	betaEmail := newEmail()
	e.expect(t, http.StatusCreated, http.MethodPost, "/api/v1/admin/invitations", e.f.betaOwner,
		map[string]any{"workspace_id": e.f.betaWS, "email": betaEmail})

	resp := e.expect(t, http.StatusOK, http.MethodGet, "/api/v1/admin/invitations", e.f.alphaOwner, nil)
	var invites []struct {
		WorkspaceID string `json:"workspace_id"`
		Email       string `json:"email"`
	}
	decode(t, resp.Data, &invites)
	for _, inv := range invites {
		if inv.WorkspaceID != e.f.alphaWS {
			t.Errorf("alpha owner was shown an invitation for workspace %s", inv.WorkspaceID)
		}
		if inv.Email == betaEmail {
			t.Error("alpha owner was shown beta's pending invitation")
		}
	}
}

// --- user updates ------------------------------------------------------------

func TestUpdateUserValidation(t *testing.T) {
	e := setup(t)

	tests := []struct {
		name   string
		target string
		body   map[string]any
		want   int
	}{
		{"target is not a uuid", "not-a-uuid", map[string]any{"is_active": false}, http.StatusBadRequest},
		{"nothing to change", e.f.alphaMember, map[string]any{}, http.StatusBadRequest},
		{"role without a workspace", e.f.alphaMember, map[string]any{"role": authz.RoleAdmin}, http.StatusBadRequest},
		{"role with an empty workspace", e.f.alphaMember, map[string]any{"role": authz.RoleAdmin, "workspace_id": ""}, http.StatusBadRequest},
		{"promotion to owner is not a role change", e.f.alphaMember, map[string]any{"role": authz.RoleOwner, "workspace_id": e.f.alphaWS}, http.StatusBadRequest},
		{"deactivating yourself", e.f.alphaAdmin, map[string]any{"is_active": false}, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.expect(t, tt.want, http.MethodPatch, "/api/v1/admin/users/"+tt.target, e.f.alphaAdmin, tt.body)
		})
	}
}

// TestUpdateUserCrossTenant is the takedown case: an admin of one tenant must
// not be able to reach an account in another, and the refusal is 404 so the
// endpoint cannot be used to probe which user ids exist.
func TestUpdateUserCrossTenant(t *testing.T) {
	e := setup(t)

	tests := []struct {
		name   string
		actor  string
		target string
		body   map[string]any
	}{
		{"deactivate an account in another tenant", e.f.alphaOwner, e.f.betaMember, map[string]any{"is_active": false}},
		{"change a role in another tenant", e.f.alphaOwner, e.f.betaMember, map[string]any{"role": authz.RoleAdmin, "workspace_id": e.f.betaWS}},
		{"touch an account that does not exist", e.f.alphaOwner, missingID, map[string]any{"is_active": false}},
		// A plain member of alpha administers nothing, so even a colleague is
		// out of reach.
		{"plain member acting on a colleague", e.f.alphaMember, e.f.alphaGuest, map[string]any{"is_active": false}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.expect(t, http.StatusNotFound, http.MethodPatch,
				"/api/v1/admin/users/"+tt.target, tt.actor, tt.body)
			assertActive(t, e, tt.target, true)
		})
	}
}

// TestDeactivateRevokesSessions covers the transaction: is_active is instance
// wide, so a ban that left the refresh token alive was a ban in name only.
func TestDeactivateRevokesSessions(t *testing.T) {
	e := setup(t)
	ctx := t.Context()

	victim := insertUser(t, e.pool, "deactivate-me")
	if _, err := e.pool.Exec(ctx,
		`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, $3)`,
		e.f.alphaWS, victim, authz.RoleMember); err != nil {
		t.Fatalf("add victim to alpha: %v", err)
	}
	if _, err := e.pool.Exec(ctx,
		`INSERT INTO sessions (user_id, refresh_token_hash, expires_at)
		 VALUES ($1, $2, NOW() + INTERVAL '30 days')`, victim, "hash-"+victim); err != nil {
		t.Fatalf("create session: %v", err)
	}

	e.expect(t, http.StatusOK, http.MethodPatch, "/api/v1/admin/users/"+victim, e.f.alphaOwner,
		map[string]any{"is_active": false})

	assertActive(t, e, victim, false)

	var sessions int
	if err := e.pool.QueryRow(ctx, `SELECT COUNT(*) FROM sessions WHERE user_id = $1`, victim).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 0 {
		t.Errorf("%d sessions survived deactivation, want 0", sessions)
	}

	// Reactivation is allowed and does not need to touch sessions.
	e.expect(t, http.StatusOK, http.MethodPatch, "/api/v1/admin/users/"+victim, e.f.alphaOwner,
		map[string]any{"is_active": true})
	assertActive(t, e, victim, true)
}

// TestCannotDeactivateAForeignOwner: the caller shares a workspace with the
// target (so the 404 gate lets them through) but the target also owns a
// workspace the caller does not administer. is_active is instance-wide, so
// allowing this would be a cross-tenant takedown through a shared workspace.
func TestCannotDeactivateAForeignOwner(t *testing.T) {
	e := setup(t)
	ctx := t.Context()

	// betaOwner joins alpha as a plain member.
	if _, err := e.pool.Exec(ctx,
		`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, $3)
		 ON CONFLICT DO NOTHING`,
		e.f.alphaWS, e.f.betaOwner, authz.RoleMember); err != nil {
		t.Fatalf("add beta owner to alpha: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(),
			`DELETE FROM workspace_members WHERE workspace_id = $1 AND user_id = $2`,
			e.f.alphaWS, e.f.betaOwner)
	})

	e.expect(t, http.StatusForbidden, http.MethodPatch, "/api/v1/admin/users/"+e.f.betaOwner,
		e.f.alphaOwner, map[string]any{"is_active": false})
	assertActive(t, e, e.f.betaOwner, true)
}

func TestSetRole(t *testing.T) {
	e := setup(t)
	ctx := t.Context()

	subject := insertUser(t, e.pool, "role-subject")
	if _, err := e.pool.Exec(ctx,
		`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, $3)`,
		e.f.alphaWS, subject, authz.RoleMember); err != nil {
		t.Fatalf("add subject: %v", err)
	}

	t.Run("admin promotes a member", func(t *testing.T) {
		e.expect(t, http.StatusOK, http.MethodPatch, "/api/v1/admin/users/"+subject, e.f.alphaOwner,
			map[string]any{"role": authz.RoleAdmin, "workspace_id": e.f.alphaWS})
		assertRole(t, e, e.f.alphaWS, subject, authz.RoleAdmin)
	})

	t.Run("and demotes them again", func(t *testing.T) {
		e.expect(t, http.StatusOK, http.MethodPatch, "/api/v1/admin/users/"+subject, e.f.alphaOwner,
			map[string]any{"role": authz.RoleGuest, "workspace_id": e.f.alphaWS})
		assertRole(t, e, e.f.alphaWS, subject, authz.RoleGuest)
	})

	// The UPDATE carries `role <> 'owner'` as a guard predicate, so an owner
	// matches no row rather than being silently demoted.
	t.Run("the owner cannot be demoted", func(t *testing.T) {
		e.expect(t, http.StatusConflict, http.MethodPatch, "/api/v1/admin/users/"+e.f.alphaOwner,
			e.f.alphaAdmin, map[string]any{"role": authz.RoleMember, "workspace_id": e.f.alphaWS})
		assertRole(t, e, e.f.alphaWS, e.f.alphaOwner, authz.RoleOwner)
	})

	// RowsAffected == 0 also covers "target is not in this workspace", which
	// must be a conflict rather than a silent success.
	t.Run("a non-member of the named workspace is a conflict", func(t *testing.T) {
		var betaAdmins int
		if err := e.pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM workspace_members WHERE workspace_id = $1 AND user_id = $2`,
			e.f.betaWS, subject).Scan(&betaAdmins); err != nil {
			t.Fatalf("count: %v", err)
		}
		if betaAdmins != 0 {
			t.Fatal("fixture drifted: subject should not be in beta")
		}
		// alphaOwner does not administer beta, so this is refused before the
		// UPDATE is even considered.
		e.expect(t, http.StatusForbidden, http.MethodPatch, "/api/v1/admin/users/"+subject,
			e.f.alphaOwner, map[string]any{"role": authz.RoleAdmin, "workspace_id": e.f.betaWS})
	})

	// The caller administers the named workspace and shares *another* workspace
	// with the target, so both gates pass — but the target is not a member of
	// the workspace whose role is being set. That must be a conflict, not a
	// silently successful no-op.
	t.Run("target is not a member of the named workspace", func(t *testing.T) {
		var gamma string
		if err := e.pool.QueryRow(ctx,
			`INSERT INTO workspaces (name, slug, owner_id) VALUES ($1, $1, $2) RETURNING id`,
			"gamma-"+subject, e.f.alphaOwner).Scan(&gamma); err != nil {
			t.Fatalf("create gamma: %v", err)
		}
		if _, err := e.pool.Exec(ctx,
			`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, $3)`,
			gamma, e.f.alphaOwner, authz.RoleOwner); err != nil {
			t.Fatalf("join gamma: %v", err)
		}
		e.expect(t, http.StatusConflict, http.MethodPatch, "/api/v1/admin/users/"+subject,
			e.f.alphaOwner, map[string]any{"role": authz.RoleAdmin, "workspace_id": gamma})
	})
}

// --- audit logs --------------------------------------------------------------

func TestAuditLogsAreScopedAndPaginated(t *testing.T) {
	e := setup(t)

	// Produce one audit entry in each tenant through the real endpoints.
	e.expect(t, http.StatusCreated, http.MethodPost, "/api/v1/admin/invitations", e.f.alphaOwner,
		map[string]any{"workspace_id": e.f.alphaWS, "email": newEmail()})
	e.expect(t, http.StatusCreated, http.MethodPost, "/api/v1/admin/invitations", e.f.betaOwner,
		map[string]any{"workspace_id": e.f.betaWS, "email": newEmail()})

	resp := e.expect(t, http.StatusOK, http.MethodGet, "/api/v1/admin/audit-logs", e.f.alphaOwner, nil)
	var logs []struct {
		WorkspaceID *string `json:"workspace_id"`
	}
	decode(t, resp.Data, &logs)
	if len(logs) == 0 {
		t.Fatal("no audit entries were recorded for alpha")
	}
	for _, l := range logs {
		if l.WorkspaceID == nil || *l.WorkspaceID != e.f.alphaWS {
			t.Errorf("alpha owner was shown an audit entry for workspace %v", l.WorkspaceID)
		}
	}

	// A malformed cursor is a 400, not a silent first page.
	e.expect(t, http.StatusBadRequest, http.MethodGet,
		"/api/v1/admin/audit-logs?cursor=not-a-cursor", e.f.alphaOwner, nil)
	e.expect(t, http.StatusBadRequest, http.MethodGet,
		"/api/v1/admin/audit-logs?limit=0", e.f.alphaOwner, nil)
	e.expect(t, http.StatusBadRequest, http.MethodGet,
		"/api/v1/admin/audit-logs?limit=101", e.f.alphaOwner, nil)
}

// --- helpers -----------------------------------------------------------------

func assertActive(t *testing.T, e *suite, userID string, want bool) {
	t.Helper()
	if userID == missingID {
		return
	}
	var active bool
	if err := e.pool.QueryRow(t.Context(),
		`SELECT is_active FROM users WHERE id = $1`, userID).Scan(&active); err != nil {
		t.Fatalf("read is_active: %v", err)
	}
	if active != want {
		t.Errorf("users.is_active = %v, want %v", active, want)
	}
}

func assertRole(t *testing.T, e *suite, workspaceID, userID, want string) {
	t.Helper()
	var role string
	if err := e.pool.QueryRow(t.Context(),
		`SELECT role FROM workspace_members WHERE workspace_id = $1 AND user_id = $2`,
		workspaceID, userID).Scan(&role); err != nil {
		t.Fatalf("read role: %v", err)
	}
	if role != want {
		t.Errorf("workspace_members.role = %q, want %q", role, want)
	}
}

func insertUser(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(t.Context(),
		`INSERT INTO users (email, username) VALUES ($1, $2) RETURNING id`,
		name+"@test.local", name).Scan(&id); err != nil {
		t.Fatalf("insert user %s: %v", name, err)
	}
	return id
}

// newEmail keeps invitation addresses unique. migration 009 adds a partial
// unique index on (workspace_id, lower(email)) for pending invitations, so
// reusing an address across tests would turn into a 409 in the wrong place.
var emailSeq atomic.Int64

func newEmail() string {
	return fmt.Sprintf("invitee-%d@test.local", emailSeq.Add(1))
}

// missingID is a syntactically valid uuid that no fixture uses.
const missingID = "00000000-0000-4000-8000-000000000000"
