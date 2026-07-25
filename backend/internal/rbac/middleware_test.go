package rbac

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
)

// fixture is the smallest graph both gates need: one workspace holding one of
// each role, plus an account that belongs to a second workspace and therefore
// has no business anywhere near the first.
type fixture struct {
	wsA, wsB                    string
	owner, admin, member, guest string
	stranger                    string
}

var (
	fixOnce sync.Once
	fix     *fixture
	fixErr  error
)

func seed(t *testing.T) (*authz.Checker, *fixture) {
	t.Helper()
	pool := testDB(t)
	fixOnce.Do(func() {
		fix, fixErr = buildFixture(context.Background(), pool)
	})
	if fixErr != nil {
		t.Fatalf("seed fixtures: %v", fixErr)
	}
	return authz.New(pool), fix
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

	f.owner = user("rbac-owner")
	f.admin = user("rbac-admin")
	f.member = user("rbac-member")
	f.guest = user("rbac-guest")
	f.stranger = user("rbac-stranger")

	f.wsA = workspace("rbac-alpha", f.owner)
	join(f.wsA, f.owner, authz.RoleOwner)
	join(f.wsA, f.admin, authz.RoleAdmin)
	join(f.wsA, f.member, authz.RoleMember)
	join(f.wsA, f.guest, authz.RoleGuest)

	f.wsB = workspace("rbac-beta", f.stranger)
	join(f.wsB, f.stranger, authz.RoleOwner)

	if err != nil {
		return nil, err
	}
	return f, nil
}

// call runs a request through mw mounted on a route that declares
// {workspace_id}, which is the only way http.ServeMux populates PathValue.
// It reports the status and whether the wrapped handler was reached.
func call(t *testing.T, mw func(http.Handler) http.Handler, pattern, target, userID string) (int, bool) {
	t.Helper()

	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	mux := http.NewServeMux()
	mux.Handle(pattern, mw(next))

	req := httptest.NewRequest(http.MethodGet, target, nil)
	if userID != "" {
		req = req.WithContext(authctx.WithUserID(req.Context(), userID))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	// Every refusal must be an error envelope, not a bare status: the client
	// branches on the code.
	if rec.Code != http.StatusOK {
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("refusal body is not JSON: %q", rec.Body.String())
		} else if body.Error.Code == "" {
			t.Errorf("refusal carried no error code: %q", rec.Body.String())
		}
	}
	return rec.Code, reached
}

func TestRequireWorkspaceRole(t *testing.T) {
	az, f := seed(t)

	const pattern = "GET /ws/{workspace_id}"
	target := func(wsID string) string { return "/ws/" + wsID }

	tests := []struct {
		name   string
		roles  []string
		wsID   string
		userID string
		want   int
	}{
		{"owner admitted by an owner-only gate", []string{authz.RoleOwner}, f.wsA, f.owner, http.StatusOK},
		{"admin admitted by an owner/admin gate", []string{authz.RoleOwner, authz.RoleAdmin}, f.wsA, f.admin, http.StatusOK},
		{"member admitted by a member gate", []string{authz.RoleMember}, f.wsA, f.member, http.StatusOK},
		{"guest admitted when guests are listed", []string{authz.RoleGuest}, f.wsA, f.guest, http.StatusOK},

		{"admin refused by an owner-only gate", []string{authz.RoleOwner}, f.wsA, f.admin, http.StatusForbidden},
		{"member refused by an owner/admin gate", []string{authz.RoleOwner, authz.RoleAdmin}, f.wsA, f.member, http.StatusForbidden},
		{"guest refused by an owner/admin gate", []string{authz.RoleOwner, authz.RoleAdmin}, f.wsA, f.guest, http.StatusForbidden},

		// The role is per workspace: being an owner elsewhere buys nothing here.
		{"owner of another workspace refused", []string{authz.RoleOwner}, f.wsA, f.stranger, http.StatusForbidden},
		{"owner refused in a workspace they do not belong to", []string{authz.RoleOwner}, f.wsB, f.owner, http.StatusForbidden},
		{"workspace that does not exist", []string{authz.RoleOwner}, missingID, f.owner, http.StatusForbidden},

		// A gate with no roles admits nobody. Spelled out because an empty
		// variadic is easy to produce by accident at a call site.
		{"empty role list admits nobody", nil, f.wsA, f.owner, http.StatusForbidden},

		{"unauthenticated caller", []string{authz.RoleOwner}, f.wsA, "", http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, reached := call(t, RequireWorkspaceRole(az, tt.roles...), pattern, target(tt.wsID), tt.userID)
			if status != tt.want {
				t.Errorf("status = %d, want %d", status, tt.want)
			}
			if reached != (tt.want == http.StatusOK) {
				t.Errorf("handler reached = %v, want %v", reached, tt.want == http.StatusOK)
			}
		})
	}

	// The path parameter is mandatory. Mounted on a route without one the gate
	// must refuse rather than silently fall back to some ambient workspace —
	// that fallback is what made the previous version deny every route while
	// looking wired up.
	t.Run("route without a workspace_id parameter refuses", func(t *testing.T) {
		status, reached := call(t, RequireWorkspaceRole(az, authz.RoleOwner), "GET /nows", "/nows", f.owner)
		if status != http.StatusForbidden || reached {
			t.Errorf("status = %d, reached = %v; want 403 and not reached", status, reached)
		}
	})
}

func TestRequireAnyWorkspaceAdmin(t *testing.T) {
	az, f := seed(t)

	tests := []struct {
		name   string
		userID string
		want   int
	}{
		{"owner of a workspace", f.owner, http.StatusOK},
		{"admin of a workspace", f.admin, http.StatusOK},
		{"owner of an unrelated workspace still gets through the door", f.stranger, http.StatusOK},

		// The gate only proves "administers something". Everything past it is
		// the handler's job to scope — see the package doc.
		{"plain member", f.member, http.StatusForbidden},
		{"guest", f.guest, http.StatusForbidden},
		{"unknown user", missingID, http.StatusForbidden},
		{"unauthenticated caller", "", http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, reached := call(t, RequireAnyWorkspaceAdmin(az), "GET /admin", "/admin", tt.userID)
			if status != tt.want {
				t.Errorf("status = %d, want %d", status, tt.want)
			}
			if reached != (tt.want == http.StatusOK) {
				t.Errorf("handler reached = %v, want %v", reached, tt.want == http.StatusOK)
			}
		})
	}
}

// TestDatabaseFailureIsNotADenial is the reason both gates return (bool, error)
// instead of a bare bool. If a dead database produced 403 the outage would look
// like a permissions bug: invisible to error-rate alerting, and indistinguishable
// from a real refusal in the logs.
func TestDatabaseFailureIsNotADenial(t *testing.T) {
	_, f := seed(t)

	// A closed pool is the cheapest faithful stand-in for an unreachable
	// database: every query on it fails, exactly as it would mid-outage.
	dead, err := pgxpool.New(t.Context(), dsn(testDBName))
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	dead.Close()
	az := authz.New(dead)

	t.Run("RequireWorkspaceRole", func(t *testing.T) {
		status, reached := call(t, RequireWorkspaceRole(az, authz.RoleOwner),
			"GET /ws/{workspace_id}", "/ws/"+f.wsA, f.owner)
		if status != http.StatusInternalServerError || reached {
			t.Errorf("status = %d, reached = %v; want 500 and not reached", status, reached)
		}
	})

	t.Run("RequireAnyWorkspaceAdmin", func(t *testing.T) {
		status, reached := call(t, RequireAnyWorkspaceAdmin(az), "GET /admin", "/admin", f.owner)
		if status != http.StatusInternalServerError || reached {
			t.Errorf("status = %d, reached = %v; want 500 and not reached", status, reached)
		}
	})
}

// missingID is a syntactically valid uuid that no fixture uses.
const missingID = "00000000-0000-4000-8000-000000000000"
