package user

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
)

// The device-token tests run against a real Postgres because everything that
// can go wrong here is in the SQL: the ON CONFLICT target that decides whether
// a shared handset keeps ringing for its previous owner, and the atomicity of
// the handover when two sessions claim the same token at once. Neither is
// observable through a mocked pool.

func deviceToken(suffix string) string { return "ExponentPushToken[" + suffix + "]" }

// deviceCall drives a request through the device routes, which RegisterRoutes
// (and therefore the shared `call` helper) deliberately does not mount — they
// only exist when PUSH_ENABLED is on.
func deviceCall(t *testing.T, h *Handler, method, path, caller string, body any) (int, envelope) {
	t.Helper()

	mux := http.NewServeMux()
	h.RegisterDeviceRoutes(mux, func(next http.Handler) http.Handler {
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
	return rec.Code, resp
}

func tokensOf(t *testing.T, repo *Repository, userID string) []string {
	t.Helper()
	got, err := repo.PushTokensForUser(t.Context(), userID)
	if err != nil {
		t.Fatalf("PushTokensForUser(%s): %v", userID, err)
	}
	return got
}

func rowCount(t *testing.T, repo *Repository, token string) int {
	t.Helper()
	var n int
	if err := repo.pool.QueryRow(t.Context(),
		`SELECT COUNT(*) FROM device_tokens WHERE token = $1`, token).Scan(&n); err != nil {
		t.Fatalf("count rows for %s: %v", token, err)
	}
	return n
}

func ownerOf(t *testing.T, repo *Repository, token string) string {
	t.Helper()
	var owner string
	if err := repo.pool.QueryRow(t.Context(),
		`SELECT user_id FROM device_tokens WHERE token = $1`, token).Scan(&owner); err != nil {
		t.Fatalf("owner of %s: %v", token, err)
	}
	return owner
}

// The defect this exists to prevent: two people share a phone, and the one who
// signed out keeps receiving the new user's DMs on the lock screen (or vice
// versa) because the old row survived alongside the new one.
func TestRegisterDeviceMovesTokenToTheNewUser(t *testing.T) {
	repo := mustRepo(t)
	alice := insertUser(t, repo, "dev-alice")
	bob := insertUser(t, repo, "dev-bob")
	token := deviceToken("shared-handset")

	if err := repo.RegisterDevice(t.Context(), alice.ID, token, "ios"); err != nil {
		t.Fatalf("register for alice: %v", err)
	}
	if got := tokensOf(t, repo, alice.ID); len(got) != 1 || got[0] != token {
		t.Fatalf("alice's tokens = %v, want [%s]", got, token)
	}

	// Bob signs in on the same handset; the OS hands the app the same token.
	if err := repo.RegisterDevice(t.Context(), bob.ID, token, "ios"); err != nil {
		t.Fatalf("register for bob: %v", err)
	}

	if got := tokensOf(t, repo, alice.ID); len(got) != 0 {
		t.Fatalf("alice still owns %v after the handset changed hands", got)
	}
	if got := tokensOf(t, repo, bob.ID); len(got) != 1 || got[0] != token {
		t.Fatalf("bob's tokens = %v, want [%s]", got, token)
	}
	if n := rowCount(t, repo, token); n != 1 {
		t.Fatalf("%d rows for one token; the UNIQUE constraint is what makes the handover expressible", n)
	}
}

// Re-registration by the same user is the normal case (the client registers on
// every launch) and must not churn the row.
func TestRegisterDeviceIsIdempotentForTheSameOwner(t *testing.T) {
	repo := mustRepo(t)
	u := insertUser(t, repo, "dev-idem")
	token := deviceToken("idem")

	if err := repo.RegisterDevice(t.Context(), u.ID, token, "android"); err != nil {
		t.Fatalf("first register: %v", err)
	}
	var created1, seen1 time.Time
	if err := repo.pool.QueryRow(t.Context(),
		`SELECT created_at, last_seen_at FROM device_tokens WHERE token = $1`, token).
		Scan(&created1, &seen1); err != nil {
		t.Fatalf("read timestamps: %v", err)
	}

	time.Sleep(10 * time.Millisecond)
	if err := repo.RegisterDevice(t.Context(), u.ID, token, "android"); err != nil {
		t.Fatalf("second register: %v", err)
	}

	var created2, seen2 time.Time
	if err := repo.pool.QueryRow(t.Context(),
		`SELECT created_at, last_seen_at FROM device_tokens WHERE token = $1`, token).
		Scan(&created2, &seen2); err != nil {
		t.Fatalf("read timestamps: %v", err)
	}

	if n := rowCount(t, repo, token); n != 1 {
		t.Fatalf("%d rows after re-registering the same token", n)
	}
	if !created2.Equal(created1) {
		t.Errorf("created_at moved on a same-owner re-registration: %s -> %s", created1, created2)
	}
	if !seen2.After(seen1) {
		t.Errorf("last_seen_at did not advance: %s -> %s", seen1, seen2)
	}
}

// The handover has to be atomic. Two sessions racing to claim the same token —
// which is exactly what a fast sign-out/sign-in on one handset produces — must
// leave one row with one owner, never a duplicate and never none.
func TestRegisterDeviceHandoverIsAtomicUnderConcurrency(t *testing.T) {
	repo := mustRepo(t)
	token := deviceToken("contended")

	const racers = 8
	users := make([]string, racers)
	for i := range users {
		users[i] = insertUser(t, repo, "dev-race").ID
	}

	// Several rounds, so a schedule where one goroutine simply wins every time
	// is not what the test is measuring.
	for round := range 5 {
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, racers)

		for i := range racers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				errs[i] = repo.RegisterDevice(ctx, users[i], token, "ios")
			}()
		}
		close(start)
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d: racer %d failed: %v", round, i, err)
			}
		}

		if n := rowCount(t, repo, token); n != 1 {
			t.Fatalf("round %d: %d rows for one contended token, want 1", round, n)
		}

		// Exactly one racer may own it, and no one else may see it — a token
		// visible to two users is a message delivered to the wrong person.
		owner := ownerOf(t, repo, token)
		owners := 0
		for _, id := range users {
			got := tokensOf(t, repo, id)
			if len(got) > 0 {
				owners++
				if id != owner {
					t.Fatalf("round %d: %s sees a token owned by %s", round, id, owner)
				}
			}
		}
		if owners != 1 {
			t.Fatalf("round %d: %d users see the contended token, want 1", round, owners)
		}
	}
}

func TestDeleteDeviceIsScopedToTheOwner(t *testing.T) {
	repo := mustRepo(t)
	alice := insertUser(t, repo, "dev-del-alice")
	bob := insertUser(t, repo, "dev-del-bob")
	token := deviceToken("scoped")

	if err := repo.RegisterDevice(t.Context(), bob.ID, token, "ios"); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Alice knows the token (she used to own the handset) but must not be able
	// to deregister Bob's device with it.
	ok, err := repo.DeleteDevice(t.Context(), alice.ID, token)
	if err != nil {
		t.Fatalf("delete as non-owner: %v", err)
	}
	if ok {
		t.Fatal("a non-owner deleted a device token")
	}
	if n := rowCount(t, repo, token); n != 1 {
		t.Fatalf("the owner's row was removed by someone else")
	}

	ok, err = repo.DeleteDevice(t.Context(), bob.ID, token)
	if err != nil {
		t.Fatalf("delete as owner: %v", err)
	}
	if !ok {
		t.Fatal("the owner could not deregister their own device")
	}
	if n := rowCount(t, repo, token); n != 0 {
		t.Fatalf("%d rows survived the owner's delete", n)
	}
}

// The DeviceNotRegistered cleanup path. It is deliberately not owner-scoped:
// the push service is making a statement about the device, not about a session.
func TestDeleteDeviceTokensRemovesDeadTokensRegardlessOfOwner(t *testing.T) {
	repo := mustRepo(t)
	alice := insertUser(t, repo, "dev-dead-alice")
	bob := insertUser(t, repo, "dev-dead-bob")

	dead1, dead2, alive := deviceToken("dead1"), deviceToken("dead2"), deviceToken("alive")
	for id, token := range map[string]string{alice.ID: dead1, bob.ID: dead2} {
		if err := repo.RegisterDevice(t.Context(), id, token, "ios"); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	if err := repo.RegisterDevice(t.Context(), alice.ID, alive, "ios"); err != nil {
		t.Fatalf("register: %v", err)
	}

	n, err := repo.DeleteDeviceTokens(t.Context(), []string{dead1, dead2, deviceToken("never-seen")})
	if err != nil {
		t.Fatalf("DeleteDeviceTokens: %v", err)
	}
	if n != 2 {
		t.Fatalf("deleted %d rows, want 2", n)
	}
	if got := tokensOf(t, repo, alice.ID); len(got) != 1 || got[0] != alive {
		t.Fatalf("alice's surviving tokens = %v, want [%s]", got, alive)
	}
	if got := tokensOf(t, repo, bob.ID); len(got) != 0 {
		t.Fatalf("bob's tokens = %v, want none", got)
	}

	// An empty list must not become `WHERE token = ANY('{}')` round trips or,
	// worse, an unfiltered DELETE.
	if n, err := repo.DeleteDeviceTokens(t.Context(), nil); err != nil || n != 0 {
		t.Fatalf("DeleteDeviceTokens(nil) = %d, %v; want 0, nil", n, err)
	}
	if got := tokensOf(t, repo, alice.ID); len(got) != 1 {
		t.Fatalf("an empty cleanup list deleted rows: %v", got)
	}
}

func TestPushTokensForUserIsEmptyNotNil(t *testing.T) {
	repo := mustRepo(t)
	u := insertUser(t, repo, "dev-none")
	got, err := repo.PushTokensForUser(t.Context(), u.ID)
	if err != nil {
		t.Fatalf("PushTokensForUser: %v", err)
	}
	if got == nil {
		t.Fatal("a user with no devices must yield an empty slice, not nil")
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want none", got)
	}
}

// --- handler -------------------------------------------------------------------

func TestRegisterDeviceEndpoint(t *testing.T) {
	h, repo, _ := handler(t)
	u := insertUser(t, repo, "dev-http")
	token := deviceToken("http-ok")

	code, resp := deviceCall(t, h, "POST", "/api/v1/users/me/devices", u.ID,
		map[string]string{"token": token, "platform": "ios"})
	if code != http.StatusCreated {
		t.Fatalf("POST devices = %d, want 201 (%+v)", code, resp.Error)
	}
	if got := tokensOf(t, repo, u.ID); len(got) != 1 || got[0] != token {
		t.Fatalf("token was not stored: %v", got)
	}

	// An unknown platform is normalised rather than rejected: the platform is a
	// diagnostic, and refusing over it would cost the user their notifications.
	code, _ = deviceCall(t, h, "POST", "/api/v1/users/me/devices", u.ID,
		map[string]string{"token": deviceToken("http-plat"), "platform": "blackberry"})
	if code != http.StatusCreated {
		t.Fatalf("POST with an unknown platform = %d, want 201", code)
	}
	var platform string
	if err := repo.pool.QueryRow(t.Context(),
		`SELECT platform FROM device_tokens WHERE token = $1`, deviceToken("http-plat")).Scan(&platform); err != nil {
		t.Fatalf("read platform: %v", err)
	}
	if platform != PlatformUnknown {
		t.Fatalf("platform = %q, want %q", platform, PlatformUnknown)
	}
}

// A token no push service can parse would be sent and rejected on every
// notification for this user forever, and nothing would ever delete it: the
// service never answers DeviceNotRegistered for a value it cannot read.
func TestRegisterDeviceRejectsTokensThatCanNeverWork(t *testing.T) {
	h, repo, _ := handler(t)
	u := insertUser(t, repo, "dev-bad")

	for _, bad := range []string{"", "not-a-token", "fA9k2_bare-fcm-token", strings.Repeat("x", MaxDeviceTokenLen+1)} {
		code, resp := deviceCall(t, h, "POST", "/api/v1/users/me/devices", u.ID,
			map[string]string{"token": bad, "platform": "ios"})
		if code != http.StatusBadRequest {
			t.Fatalf("POST %q = %d, want 400", bad, code)
		}
		if resp.Error == nil || resp.Error.Code != "INVALID_PUSH_TOKEN" {
			t.Fatalf("POST %q: error = %+v, want INVALID_PUSH_TOKEN", bad, resp.Error)
		}
	}
	if got := tokensOf(t, repo, u.ID); len(got) != 0 {
		t.Fatalf("a rejected token was stored anyway: %v", got)
	}
}

// The route is DELETE .../devices/{token} and an Expo token contains brackets,
// so it reaches the mux percent-encoded. If PathValue did not decode it, logout
// would silently fail to deregister and the next user of the handset would keep
// receiving the previous one's notifications.
func TestDeleteDeviceEndpointHandlesEncodedTokens(t *testing.T) {
	h, repo, _ := handler(t)
	u := insertUser(t, repo, "dev-http-del")
	other := insertUser(t, repo, "dev-http-other")
	token := deviceToken("http-del")

	if err := repo.RegisterDevice(t.Context(), u.ID, token, "ios"); err != nil {
		t.Fatalf("register: %v", err)
	}
	path := "/api/v1/users/me/devices/" + url.PathEscape(token)
	if !strings.Contains(path, "%5B") {
		t.Fatalf("the token was expected to arrive percent-encoded: %s", path)
	}

	// Someone else's delete must not touch it.
	if code, _ := deviceCall(t, h, "DELETE", path, other.ID, nil); code != http.StatusNotFound {
		t.Fatalf("DELETE as a non-owner = %d, want 404", code)
	}
	if got := tokensOf(t, repo, u.ID); len(got) != 1 {
		t.Fatalf("a non-owner deleted the token: %v", got)
	}

	if code, resp := deviceCall(t, h, "DELETE", path, u.ID, nil); code != http.StatusOK {
		t.Fatalf("DELETE = %d, want 200 (%+v)", code, resp.Error)
	}
	if got := tokensOf(t, repo, u.ID); len(got) != 0 {
		t.Fatalf("logout did not deregister the device: %v", got)
	}

	// A second logout is a 404, not a 500.
	if code, _ := deviceCall(t, h, "DELETE", path, u.ID, nil); code != http.StatusNotFound {
		t.Fatalf("second DELETE = %d, want 404", code)
	}
}
