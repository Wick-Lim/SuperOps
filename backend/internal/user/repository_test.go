package user

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The fixture is two tenants that share nothing.
//
//	alpha — caller, colleague, hidden (inactive), blocked, blocker
//	beta  — outsider
//
// "caller" is the identity every read is performed as; every negative
// assertion below is about something caller must not be able to see.
type fixture struct {
	alphaWS, betaWS string

	caller    string
	colleague string
	inactive  string
	blockedBy string // caller blocked them
	blocking  string // they blocked caller
	outsider  string
	loner     string // belongs to no workspace at all
}

var (
	fixOnce sync.Once
	fix     *fixture
	fixErr  error
)

// tag is embedded in every fixture username so a search can be aimed at this
// test's rows and nothing else in the database.
const tag = "zqx"

func setup(t *testing.T) (*Repository, *fixture) {
	t.Helper()
	pool := testDB(t)
	fixOnce.Do(func() {
		fix, fixErr = buildFixture(context.Background(), pool)
	})
	if fixErr != nil {
		t.Fatalf("seed fixtures: %v", fixErr)
	}
	return NewRepository(pool), fix
}

func buildFixture(ctx context.Context, pool *pgxpool.Pool) (*fixture, error) {
	f := &fixture{}
	var err error

	user := func(name, fullName string, active bool) string {
		if err != nil {
			return ""
		}
		var id string
		err = pool.QueryRow(ctx,
			`INSERT INTO users (email, username, full_name, is_active)
			 VALUES ($1, $2, $3, $4) RETURNING id`,
			name+"@test.local", name, fullName, active).Scan(&id)
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
	join := func(wsID, userID string) {
		if err != nil {
			return
		}
		_, err = pool.Exec(ctx,
			`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, 'member')`,
			wsID, userID)
	}
	block := func(blocker, blocked string) {
		if err != nil {
			return
		}
		_, err = pool.Exec(ctx,
			`INSERT INTO user_blocks (blocker_id, blocked_id) VALUES ($1, $2)`, blocker, blocked)
	}

	f.caller = user(tag+"caller", "Cara Caller", true)
	f.colleague = user(tag+"colleague", "Colin Colleague", true)
	f.inactive = user(tag+"inactive", "Ida Inactive", false)
	f.blockedBy = user(tag+"blockedby", "Ben Blockedby", true)
	f.blocking = user(tag+"blocking", "Bella Blocking", true)
	f.outsider = user(tag+"outsider", "Otto Outsider", true)
	f.loner = user(tag+"loner", "Lena Loner", true)

	f.alphaWS = workspace(tag+"-alpha", f.caller)
	for _, id := range []string{f.caller, f.colleague, f.inactive, f.blockedBy, f.blocking} {
		join(f.alphaWS, id)
	}

	f.betaWS = workspace(tag+"-beta", f.outsider)
	join(f.betaWS, f.outsider)

	block(f.caller, f.blockedBy)
	block(f.blocking, f.caller)

	if err != nil {
		return nil, err
	}
	return f, nil
}

// --- basic reads -------------------------------------------------------------

func TestGetByMissingRowIsNilNil(t *testing.T) {
	repo, _ := setup(t)

	tests := []struct {
		name string
		get  func() (*User, error)
	}{
		{"by id", func() (*User, error) { return repo.GetByID(t.Context(), missingID) }},
		{"by email", func() (*User, error) { return repo.GetByEmail(t.Context(), "nobody@nowhere.invalid") }},
		{"by username", func() (*User, error) { return repo.GetByUsername(t.Context(), "nobody-at-all") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := tt.get()
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if u != nil {
				// Collapsing "missing" into an error is what turns a 404 into
				// a 500 at the handler.
				t.Errorf("got %+v, want nil", u)
			}
		})
	}
}

func TestCreateAndReadBack(t *testing.T) {
	repo, _ := setup(t)
	ctx := t.Context()

	in := &User{
		ID:           newUUID(t, repo),
		Email:        tag + "created@test.local",
		Username:     tag + "created",
		FullName:     "Created User",
		PasswordHash: "not-a-real-hash",
		AvatarURL:    "https://example.invalid/a.png",
		Timezone:     "Europe/Berlin",
		Locale:       "de",
		IsActive:     true,
	}
	if err := repo.Create(ctx, in); err != nil {
		t.Fatalf("Create: %v", err)
	}

	byID, err := repo.GetByID(ctx, in.ID)
	if err != nil || byID == nil {
		t.Fatalf("GetByID = %v, %v", byID, err)
	}
	byEmail, err := repo.GetByEmail(ctx, in.Email)
	if err != nil || byEmail == nil || byEmail.ID != in.ID {
		t.Fatalf("GetByEmail = %v, %v", byEmail, err)
	}
	byUsername, err := repo.GetByUsername(ctx, in.Username)
	if err != nil || byUsername == nil || byUsername.ID != in.ID {
		t.Fatalf("GetByUsername = %v, %v", byUsername, err)
	}

	if byID.FullName != in.FullName || byID.Timezone != in.Timezone || byID.Locale != in.Locale {
		t.Errorf("read back %+v, want the values that were written", byID)
	}
	if byID.PasswordHash != in.PasswordHash {
		t.Errorf("password hash = %q, want it to round-trip for the auth path", byID.PasswordHash)
	}
	if byID.LastActiveAt != nil {
		t.Errorf("last_active_at = %v on a fresh row, want nil", byID.LastActiveAt)
	}
}

func TestUpdateWritesProfileFields(t *testing.T) {
	repo, _ := setup(t)
	ctx := t.Context()

	u := insertUser(t, repo, tag+"updatable")
	u.FullName = "Renamed"
	u.AvatarURL = "https://example.invalid/new.png"
	u.Timezone = "Asia/Seoul"
	u.Locale = "ko"
	if err := repo.Update(ctx, u); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := repo.GetByID(ctx, u.ID)
	if err != nil || got == nil {
		t.Fatalf("GetByID = %v, %v", got, err)
	}
	if got.FullName != "Renamed" || got.AvatarURL != u.AvatarURL || got.Timezone != "Asia/Seoul" || got.Locale != "ko" {
		t.Errorf("after Update = %+v, want the new values", got)
	}
	// Migration 009's BEFORE UPDATE trigger owns updated_at; the repository
	// deliberately does not set it, so a missing trigger would show up here.
	if !got.UpdatedAt.After(got.CreatedAt) {
		t.Errorf("updated_at (%v) did not move past created_at (%v)", got.UpdatedAt, got.CreatedAt)
	}
}

// TestUpdateStatusIsReadable is the regression this package's column list
// exists for: status_text/status_emoji were written by the handler and missing
// from userColumns, so PUT /users/me/status was write-only.
func TestUpdateStatusIsReadable(t *testing.T) {
	repo, _ := setup(t)
	ctx := t.Context()

	u := insertUser(t, repo, tag+"statusable")
	if err := repo.UpdateStatus(ctx, u.ID, "in a meeting", ":calendar:"); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	got, err := repo.GetByID(ctx, u.ID)
	if err != nil || got == nil {
		t.Fatalf("GetByID = %v, %v", got, err)
	}
	if got.StatusText != "in a meeting" || got.StatusEmoji != ":calendar:" {
		t.Errorf("status = (%q, %q), want the values that were written", got.StatusText, got.StatusEmoji)
	}

	// Clearing is a write of empty strings, not a no-op.
	if err := repo.UpdateStatus(ctx, u.ID, "", ""); err != nil {
		t.Fatalf("UpdateStatus (clear): %v", err)
	}
	got, err = repo.GetByID(ctx, u.ID)
	if err != nil || got == nil {
		t.Fatalf("GetByID = %v, %v", got, err)
	}
	if got.StatusText != "" || got.StatusEmoji != "" {
		t.Errorf("status = (%q, %q) after clearing, want empty", got.StatusText, got.StatusEmoji)
	}
}

// TestTouchLastActiveIsThrottled pins the WHERE clause: the first call stamps
// the column, an immediate second call must not, or an active client costs one
// write per request.
func TestTouchLastActiveIsThrottled(t *testing.T) {
	repo, _ := setup(t)
	ctx := t.Context()

	u := insertUser(t, repo, tag+"heartbeat")
	if u.LastActiveAt != nil {
		t.Fatalf("fixture drifted: last_active_at = %v on a fresh row", u.LastActiveAt)
	}

	if err := repo.TouchLastActive(ctx, u.ID); err != nil {
		t.Fatalf("TouchLastActive: %v", err)
	}
	first, err := repo.GetByID(ctx, u.ID)
	if err != nil || first == nil {
		t.Fatalf("GetByID = %v, %v", first, err)
	}
	if first.LastActiveAt == nil {
		t.Fatal("last_active_at is still NULL after the first touch")
	}

	if err := repo.TouchLastActive(ctx, u.ID); err != nil {
		t.Fatalf("TouchLastActive (second): %v", err)
	}
	second, err := repo.GetByID(ctx, u.ID)
	if err != nil || second == nil {
		t.Fatalf("GetByID = %v, %v", second, err)
	}
	if !second.LastActiveAt.Equal(*first.LastActiveAt) {
		t.Errorf("second touch moved last_active_at from %v to %v; the 5-minute throttle did not hold",
			first.LastActiveAt, second.LastActiveAt)
	}

	// Backdating past the interval makes the next touch write again, which
	// proves the throttle is a window and not a one-shot.
	if _, err := repo.pool.Exec(ctx,
		`UPDATE users SET last_active_at = NOW() - INTERVAL '10 minutes' WHERE id = $1`, u.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if err := repo.TouchLastActive(ctx, u.ID); err != nil {
		t.Fatalf("TouchLastActive (third): %v", err)
	}
	third, err := repo.GetByID(ctx, u.ID)
	if err != nil || third == nil {
		t.Fatalf("GetByID = %v, %v", third, err)
	}
	if !third.LastActiveAt.After(*first.LastActiveAt) {
		t.Error("a touch after the throttle window did not stamp last_active_at")
	}
}

// --- search ------------------------------------------------------------------

func TestSearchInSharedWorkspaces(t *testing.T) {
	repo, f := setup(t)

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{
			// Matching everything this fixture created: only the colleague
			// survives all four filters.
			name:  "only active, unblocked, workspace-sharing users",
			query: tag,
			want:  []string{f.colleague},
		},
		{"matches on username", "colleague", []string{f.colleague}},
		{"matches on full_name", "Colin", []string{f.colleague}},
		{"match is case-insensitive", "COLIN COLLEAGUE", []string{f.colleague}},

		// Every exclusion, isolated. Each of these once returned a hit.
		{"the caller never matches themselves", "caller", nil},
		{"inactive accounts are hidden", "inactive", nil},
		{"a user the caller blocked is hidden", "blockedby", nil},
		{"a user who blocked the caller is hidden", "blocking", nil},
		{"a user in another tenant is hidden", "outsider", nil},
		{"a user in no workspace is hidden", "loner", nil},

		// The leak: searching by email address let any authenticated caller
		// confirm an account existed in an unrelated tenant.
		{"email is not searchable", tag + "colleague@test.local", nil},
		{"email of a stranger is not searchable", tag + "outsider@test.local", nil},

		{"no match", "definitely-not-a-user", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			users, err := repo.SearchInSharedWorkspaces(t.Context(), f.caller, tt.query, 20)
			if err != nil {
				t.Fatalf("SearchInSharedWorkspaces: unexpected error %v", err)
			}
			if users == nil {
				t.Fatal("SearchInSharedWorkspaces returned nil; want an empty slice")
			}
			ids := make([]string, 0, len(users))
			for _, u := range users {
				ids = append(ids, u.ID)
			}
			if !sameSet(ids, tt.want) {
				t.Errorf("got %v, want %v", ids, tt.want)
			}
		})
	}

	t.Run("limit is applied", func(t *testing.T) {
		// Two extra colleagues, so the unlimited result is 3 and the limit has
		// something to cut.
		for _, name := range []string{tag + "colleague2", tag + "colleague3"} {
			u := insertUser(t, repo, name)
			if _, err := repo.pool.Exec(t.Context(),
				`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, 'member')
				 ON CONFLICT DO NOTHING`, f.alphaWS, u.ID); err != nil {
				t.Fatalf("join alpha: %v", err)
			}
		}
		all, err := repo.SearchInSharedWorkspaces(t.Context(), f.caller, "colleague", 20)
		if err != nil {
			t.Fatalf("SearchInSharedWorkspaces: %v", err)
		}
		if len(all) < 3 {
			t.Fatalf("expected at least 3 colleagues, got %d", len(all))
		}
		limited, err := repo.SearchInSharedWorkspaces(t.Context(), f.caller, "colleague", 2)
		if err != nil {
			t.Fatalf("SearchInSharedWorkspaces: %v", err)
		}
		if len(limited) != 2 {
			t.Errorf("limit 2 returned %d rows", len(limited))
		}
	})
}

// TestRepositorySharesWorkspaceIsSymmetric contrasts with
// authz.Checker.SharesWorkspace, which additionally requires the actor to be an
// owner/admin. This one must NOT: it is what lets an ordinary member look up a
// colleague, and adding the role predicate here would break every profile view.
func TestRepositorySharesWorkspaceIsSymmetric(t *testing.T) {
	repo, f := setup(t)

	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"two plain members of the same workspace", f.colleague, f.blockedBy, true},
		{"the same pair, reversed", f.blockedBy, f.colleague, true},
		{"the workspace owner and a member", f.caller, f.colleague, true},
		{"a member and the workspace owner", f.colleague, f.caller, true},

		{"across tenants", f.caller, f.outsider, false},
		{"across tenants, reversed", f.outsider, f.caller, false},
		{"a user in no workspace", f.caller, f.loner, false},
		{"an unknown user", f.caller, missingID, false},

		// Self is true without touching the tables at all — a user with no
		// membership anywhere must still be able to read their own profile.
		{"self", f.caller, f.caller, true},
		{"self, with no memberships", f.loner, f.loner, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := repo.SharesWorkspace(t.Context(), tt.a, tt.b)
			if err != nil {
				t.Fatalf("SharesWorkspace: unexpected error %v", err)
			}
			if got != tt.want {
				t.Errorf("SharesWorkspace = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- helpers -----------------------------------------------------------------

var userSeq int

func insertUser(t *testing.T, repo *Repository, name string) *User {
	t.Helper()
	userSeq++
	unique := fmt.Sprintf("%s-%d", name, userSeq)
	u := &User{
		ID:       newUUID(t, repo),
		Email:    unique + "@test.local",
		Username: unique,
		FullName: name,
		Timezone: "UTC",
		Locale:   "en",
		IsActive: true,
	}
	if err := repo.Create(t.Context(), u); err != nil {
		t.Fatalf("insert %s: %v", name, err)
	}
	got, err := repo.GetByID(t.Context(), u.ID)
	if err != nil || got == nil {
		t.Fatalf("read back %s: %v", name, err)
	}
	return got
}

func newUUID(t *testing.T, repo *Repository) string {
	t.Helper()
	var id string
	if err := repo.pool.QueryRow(t.Context(), `SELECT uuid_generate_v4()`).Scan(&id); err != nil {
		t.Fatalf("generate uuid: %v", err)
	}
	return id
}

func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	a := append([]string(nil), got...)
	b := append([]string(nil), want...)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// missingID is a syntactically valid uuid that no fixture uses.
const missingID = "00000000-0000-4000-8000-000000000000"
