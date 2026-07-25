package user

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
)

type envelope struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// call drives a request through the handler's own routes so {user_id} is
// populated exactly as net/http populates it in production. The stand-in auth
// middleware only installs the caller identity.
func call(t *testing.T, h *Handler, method, path, caller string, body any) (int, envelope) {
	t.Helper()

	mux := http.NewServeMux()
	h.RegisterRoutes(mux, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(authctx.WithUserID(r.Context(), caller)))
		})
	})

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

	var resp envelope
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s %s: body is not an envelope: %q", method, path, rec.Body.String())
		}
	}
	if rec.Code >= 400 && (resp.Error == nil || resp.Error.Code == "") {
		t.Errorf("%s %s: %d carried no error code (%q)", method, path, rec.Code, rec.Body.String())
	}
	return rec.Code, resp
}

func expect(t *testing.T, h *Handler, want int, method, path, caller string, body any) envelope {
	t.Helper()
	code, resp := call(t, h, method, path, caller, body)
	if code != want {
		t.Fatalf("%s %s = %d, want %d (error=%+v)", method, path, code, want, resp.Error)
	}
	return resp
}

func handler(t *testing.T) (*Handler, *Repository, *fixture) {
	t.Helper()
	repo, f := setup(t)
	return NewHandler(repo), repo, f
}

func TestGetMe(t *testing.T) {
	h, _, f := handler(t)

	t.Run("returns the caller's own record", func(t *testing.T) {
		resp := expect(t, h, http.StatusOK, http.MethodGet, "/api/v1/users/me", f.caller, nil)
		var body map[string]any
		if err := json.Unmarshal(resp.Data, &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body["id"] != f.caller {
			t.Errorf("id = %v, want %v", body["id"], f.caller)
		}
		// /me is the caller's own record, so email belongs in it — but the
		// password hash never does, on any path.
		if body["email"] == nil {
			t.Error("own record omitted email")
		}
		if _, ok := body["password_hash"]; ok {
			t.Error("GET /users/me leaked password_hash")
		}
	})

	t.Run("doubles as the liveness heartbeat", func(t *testing.T) {
		u := insertUser(t, mustRepo(t), tag+"heartbeat-handler")
		if u.LastActiveAt != nil {
			t.Fatalf("fixture drifted: last_active_at = %v", u.LastActiveAt)
		}
		expect(t, h, http.StatusOK, http.MethodGet, "/api/v1/users/me", u.ID, nil)

		got, err := mustRepo(t).GetByID(t.Context(), u.ID)
		if err != nil || got == nil {
			t.Fatalf("GetByID = %v, %v", got, err)
		}
		if got.LastActiveAt == nil {
			t.Error("GET /users/me did not stamp last_active_at")
		}
	})

	t.Run("an identity with no row is 404, not 500", func(t *testing.T) {
		expect(t, h, http.StatusNotFound, http.MethodGet, "/api/v1/users/me", missingID, nil)
	})
}

func TestUpdateMe(t *testing.T) {
	h, repo, _ := handler(t)

	t.Run("applies only the fields that were sent", func(t *testing.T) {
		u := insertUser(t, repo, tag+"patchable")
		resp := expect(t, h, http.StatusOK, http.MethodPatch, "/api/v1/users/me", u.ID,
			map[string]any{"full_name": "New Name"})

		var got User
		if err := json.Unmarshal(resp.Data, &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.FullName != "New Name" {
			t.Errorf("full_name = %q, want %q", got.FullName, "New Name")
		}
		// timezone was not in the body, so it must be untouched — a nil-pointer
		// field is "absent", not "set to empty".
		if got.Timezone != u.Timezone {
			t.Errorf("timezone = %q, want it unchanged at %q", got.Timezone, u.Timezone)
		}

		stored, err := repo.GetByID(t.Context(), u.ID)
		if err != nil || stored == nil {
			t.Fatalf("GetByID = %v, %v", stored, err)
		}
		if stored.FullName != "New Name" || stored.Timezone != u.Timezone {
			t.Errorf("stored %+v, want full_name updated and timezone untouched", stored)
		}
	})

	t.Run("an explicit empty string does clear the field", func(t *testing.T) {
		u := insertUser(t, repo, tag+"clearable")
		expect(t, h, http.StatusOK, http.MethodPatch, "/api/v1/users/me", u.ID,
			map[string]any{"full_name": ""})
		stored, err := repo.GetByID(t.Context(), u.ID)
		if err != nil || stored == nil {
			t.Fatalf("GetByID = %v, %v", stored, err)
		}
		if stored.FullName != "" {
			t.Errorf("full_name = %q, want it cleared", stored.FullName)
		}
	})

	tests := []struct {
		name   string
		caller string
		body   any
		want   int
	}{
		{"unknown field is rejected", "", map[string]any{"is_admin": true}, http.StatusBadRequest},
		{"malformed body is rejected", "", "not-an-object", http.StatusBadRequest},
		{"identity with no row is 404", missingID, map[string]any{"full_name": "x"}, http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caller := tt.caller
			if caller == "" {
				caller = insertUser(t, repo, tag+"patch-invalid").ID
			}
			expect(t, h, tt.want, http.MethodPatch, "/api/v1/users/me", caller, tt.body)
		})
	}
}

func TestUpdateStatus(t *testing.T) {
	h, repo, _ := handler(t)

	t.Run("round-trips through the repository", func(t *testing.T) {
		u := insertUser(t, repo, tag+"status-handler")
		expect(t, h, http.StatusOK, http.MethodPut, "/api/v1/users/me/status", u.ID,
			map[string]any{"status_text": "heads down", "status_emoji": ":no_bell:"})

		stored, err := repo.GetByID(t.Context(), u.ID)
		if err != nil || stored == nil {
			t.Fatalf("GetByID = %v, %v", stored, err)
		}
		if stored.StatusText != "heads down" || stored.StatusEmoji != ":no_bell:" {
			t.Errorf("stored status = (%q, %q), want what was sent", stored.StatusText, stored.StatusEmoji)
		}
	})

	// Migration 009 CHECKs both lengths. Without the handler-side guard an
	// over-long status is a constraint violation surfacing as a 500.
	t.Run("length limits are a 400, not a constraint violation", func(t *testing.T) {
		u := insertUser(t, repo, tag+"status-toolong")

		tests := []struct {
			name string
			body map[string]any
		}{
			{"status_text", map[string]any{"status_text": strings.Repeat("a", MaxStatusTextLen+1)}},
			{"status_emoji", map[string]any{"status_emoji": strings.Repeat("x", MaxStatusEmojiLen+1)}},
			// The limits are counted in runes, not bytes: a multi-byte status
			// that is short enough must still be accepted.
			{"multi-byte text over the rune limit", map[string]any{"status_text": strings.Repeat("가", MaxStatusTextLen+1)}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				resp := expect(t, h, http.StatusBadRequest, http.MethodPut, "/api/v1/users/me/status", u.ID, tt.body)
				if resp.Error.Code != "STATUS_TOO_LONG" {
					t.Errorf("error code = %q, want STATUS_TOO_LONG", resp.Error.Code)
				}
			})
		}

		// Exactly at the rune limit is allowed, multi-byte included.
		expect(t, h, http.StatusOK, http.MethodPut, "/api/v1/users/me/status", u.ID,
			map[string]any{"status_text": strings.Repeat("가", MaxStatusTextLen)})
	})
}

// TestGetUserRequiresASharedWorkspace covers the directory leak: the lookup used
// to be global, so any leaked uuid resolved to a profile across tenant
// boundaries. The refusal is 404 rather than 403 so the endpoint cannot be used
// to test which ids exist.
func TestGetUserRequiresASharedWorkspace(t *testing.T) {
	h, _, f := handler(t)

	tests := []struct {
		name   string
		caller string
		target string
		want   int
	}{
		{"colleague in the same workspace", f.caller, f.colleague, http.StatusOK},
		{"self", f.caller, f.caller, http.StatusOK},
		{"an inactive colleague is still a profile", f.caller, f.inactive, http.StatusOK},

		{"a user in another tenant", f.caller, f.outsider, http.StatusNotFound},
		{"a user in no workspace at all", f.caller, f.loner, http.StatusNotFound},
		{"a user id that does not exist", f.caller, missingID, http.StatusNotFound},
		{"the other direction across tenants", f.outsider, f.caller, http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expect(t, h, tt.want, http.MethodGet, "/api/v1/users/"+tt.target, tt.caller, nil)
		})
	}

	t.Run("returns the public projection only", func(t *testing.T) {
		resp := expect(t, h, http.StatusOK, http.MethodGet, "/api/v1/users/"+f.colleague, f.caller, nil)
		var body map[string]any
		if err := json.Unmarshal(resp.Data, &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, leaked := range []string{"email", "password_hash", "last_active_at", "is_active"} {
			if _, ok := body[leaked]; ok {
				t.Errorf("another user's profile exposed %q", leaked)
			}
		}
		if body["id"] != f.colleague || body["username"] == nil {
			t.Errorf("profile = %+v, want the public fields", body)
		}
	})
}

func TestSearchUsers(t *testing.T) {
	h, _, f := handler(t)

	t.Run("q is required", func(t *testing.T) {
		// Without it the query would be `%%`, which matches every user the
		// caller may see — a directory dump behind a missing parameter.
		expect(t, h, http.StatusBadRequest, http.MethodGet, "/api/v1/users/search", f.caller, nil)
		expect(t, h, http.StatusBadRequest, http.MethodGet, "/api/v1/users/search?q=", f.caller, nil)
	})

	t.Run("returns only users the caller shares a workspace with", func(t *testing.T) {
		resp := expect(t, h, http.StatusOK, http.MethodGet, "/api/v1/users/search?q="+tag, f.caller, nil)
		var users []map[string]any
		if err := json.Unmarshal(resp.Data, &users); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(users) == 0 {
			t.Fatal("search returned nothing; the fixture colleague should match")
		}
		for _, u := range users {
			if u["id"] == f.outsider || u["id"] == f.loner || u["id"] == f.caller {
				t.Errorf("search returned %v, which the caller must not see", u["id"])
			}
			if _, ok := u["email"]; ok {
				t.Error("search results carried email addresses")
			}
		}
	})

	t.Run("an empty result is [] and not null", func(t *testing.T) {
		// The client iterates the array directly; null would be a runtime error
		// rather than an empty list.
		resp := expect(t, h, http.StatusOK, http.MethodGet,
			"/api/v1/users/search?q=definitely-no-such-user", f.caller, nil)
		if strings.TrimSpace(string(resp.Data)) != "[]" {
			t.Errorf("data = %s, want []", string(resp.Data))
		}
	})
}

// mustRepo returns the shared repository for helpers that need it outside the
// handler triple.
func mustRepo(t *testing.T) *Repository {
	t.Helper()
	repo, _ := setup(t)
	return repo
}
