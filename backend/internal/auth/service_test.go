package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/user"
	"github.com/Wick-Lim/SuperOps/backend/pkg/crypto"
)

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		name string
		pw   string
		want error
	}{
		{"empty", "", ErrPasswordTooShort},
		{"seven ascii", strings.Repeat("a", 7), ErrPasswordTooShort},
		{"exactly minimum", strings.Repeat("a", 8), nil},
		{"exactly 72 bytes", strings.Repeat("a", 72), nil},
		// bcrypt hashes only the first 72 bytes; anything longer must be
		// rejected rather than silently truncated.
		{"73 bytes", strings.Repeat("a", 73), ErrPasswordTooLong},
		{"long", strings.Repeat("a", 200), ErrPasswordTooLong},
		// The limit is bytes, not runes: "é" is two bytes in UTF-8.
		{"36 two-byte runes is 72 bytes", strings.Repeat("é", 36), nil},
		{"37 two-byte runes is 74 bytes", strings.Repeat("é", 37), ErrPasswordTooLong},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validatePassword(tt.pw); !errors.Is(got, tt.want) {
				t.Errorf("validatePassword(%d bytes) = %v, want %v", len(tt.pw), got, tt.want)
			}
		})
	}
}

// The dummy hash exists so that a login for a nonexistent account still pays
// for exactly one bcrypt comparison. If it stopped being a real cost-12 hash,
// CheckPassword would return early and the timing oracle would come back.
func TestDummyPasswordHash(t *testing.T) {
	if !strings.HasPrefix(dummyPasswordHash, "$2a$12$") {
		t.Fatalf("dummy hash is not a cost-12 bcrypt hash: %q", dummyPasswordHash)
	}
	if len(dummyPasswordHash) != 60 {
		t.Fatalf("dummy hash has length %d, want 60", len(dummyPasswordHash))
	}
	for _, guess := range []string{"", "password", "superops-login-timing-equalizer!"} {
		if crypto.CheckPassword(guess, dummyPasswordHash) {
			t.Errorf("dummy hash must not match %q", guess)
		}
	}
}

func TestLoginFailureReason(t *testing.T) {
	tests := []struct {
		name       string
		u          *user.User
		passwordOK bool
		want       string
	}{
		{"unknown email", nil, false, "unknown_email"},
		{"deactivated", &user.User{IsActive: false, PasswordHash: "h"}, true, "account_deactivated"},
		{"no password set", &user.User{IsActive: true}, false, "no_password_set"},
		{"bad password", &user.User{IsActive: true, PasswordHash: "h"}, false, "bad_password"},
		{"success", &user.User{IsActive: true, PasswordHash: "h"}, true, "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := loginFailureReason(tt.u, tt.passwordOK); got != tt.want {
				t.Errorf("got %q want %q", got, tt.want)
			}
		})
	}
}

func decodeErrorBody(t *testing.T, body string) (code, message string) {
	t.Helper()
	var resp struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	if resp.Error == nil {
		t.Fatalf("no error object in body %q", body)
	}
	return resp.Error.Code, resp.Error.Message
}

// Every way a login can fail for credential reasons must produce the exact
// same response, so the endpoint cannot be used to enumerate accounts or to
// tell an existing-but-disabled account apart from an unknown one.
func TestLoginErrorsAreUniform(t *testing.T) {
	// Login returns ErrInvalidCredentials for all of: unknown email, wrong
	// password, deactivated account, account with no password hash.
	rec := httptest.NewRecorder()
	writeAuthError(rec, ErrInvalidCredentials)
	if rec.Code != 401 {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	code, msg := decodeErrorBody(t, rec.Body.String())
	if code != "UNAUTHORIZED" || msg != "invalid credentials" {
		t.Fatalf("got code=%q message=%q, want UNAUTHORIZED/invalid credentials", code, msg)
	}

	// A wrapped sentinel must still map to the same uniform answer.
	rec = httptest.NewRecorder()
	writeAuthError(rec, fmt.Errorf("login: %w", ErrInvalidCredentials))
	if rec.Code != 401 {
		t.Fatalf("wrapped sentinel status = %d, want 401", rec.Code)
	}
	if code, msg := decodeErrorBody(t, rec.Body.String()); code != "UNAUTHORIZED" || msg != "invalid credentials" {
		t.Fatalf("wrapped sentinel got code=%q message=%q", code, msg)
	}
}

// Anything that is not an explicitly classified caller fault is an internal
// error and must be masked: unauthenticated endpoints previously echoed raw
// Postgres constraint text back to the client.
func TestInternalErrorsAreMasked(t *testing.T) {
	leaky := errors.New(`ERROR: duplicate key value violates unique constraint "users_email_key" (SQLSTATE 23505)`)

	rec := httptest.NewRecorder()
	writeAuthError(rec, leaky)

	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "users_email_key") || strings.Contains(body, "SQLSTATE") {
		t.Fatalf("internal error leaked to client: %s", body)
	}
	if code, msg := decodeErrorBody(t, body); code != "INTERNAL_ERROR" || msg != "internal server error" {
		t.Fatalf("got code=%q message=%q", code, msg)
	}
}

func TestAuthErrorResponses(t *testing.T) {
	tests := []struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		{ErrTOTPRequired, 401, "TOTP_REQUIRED"},
		{ErrInvalidTOTPCode, 401, "INVALID_TOTP_CODE"},
		{ErrInvalidRefreshToken, 401, "UNAUTHORIZED"},
		{ErrReauthRequired, 401, "REAUTH_REQUIRED"},
		{ErrInviteWrongAccount, 403, "INVITE_WRONG_ACCOUNT"},
		{ErrUsernameTaken, 409, "USERNAME_TAKEN"},
		{ErrCurrentPassword, 400, "INVALID_PASSWORD"},
		{ErrPasswordTooLong, 400, "BAD_REQUEST"},
		{ErrPasswordTooShort, 400, "BAD_REQUEST"},
		{ErrInviteInvalid, 400, "INVITE_INVALID"},
		{ErrInviteExpired, 400, "INVITE_EXPIRED"},
		{ErrTOTPSetupMissing, 400, "TOTP_SETUP_REQUIRED"},
	}
	for _, tt := range tests {
		t.Run(tt.wantCode, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeAuthError(rec, tt.err)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if code, _ := decodeErrorBody(t, rec.Body.String()); code != tt.wantCode {
				t.Errorf("code = %q, want %q", code, tt.wantCode)
			}
		})
	}
}

var testDBName = fmt.Sprintf("superops_auth_test_%d", os.Getpid())

var (
	dbOnce sync.Once
	dbPool *pgxpool.Pool
	dbErr  error
)

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

func requireInfra() bool {
	b, err := strconv.ParseBool(os.Getenv("SUPEROPS_REQUIRE_INFRA"))
	return err == nil && b
}

func dsn(database string) string {
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s",
		url.QueryEscape(env("DB_USER", "superops")),
		url.QueryEscape(env("DB_PASSWORD", "")),
		env("DB_HOST", "127.0.0.1"),
		env("DB_PORT", "5432"),
		database,
		env("DB_SSLMODE", "disable"),
	)
}

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		dbPool, dbErr = createTestDB(ctx)
	})
	if dbErr != nil {
		if requireInfra() {
			t.Fatalf("SUPEROPS_REQUIRE_INFRA=1 but Postgres is unusable: %v", dbErr)
		}
		t.Skipf("postgres unavailable, skipping auth database tests: %v", dbErr)
	}
	return dbPool
}

func createTestDB(ctx context.Context) (*pgxpool.Pool, error) {
	maint, err := pgx.Connect(ctx, dsn(env("DB_MAINTENANCE_NAME", "postgres")))
	if err != nil {
		return nil, fmt.Errorf("connect to maintenance database: %w", err)
	}
	defer maint.Close(ctx)

	if _, err := maint.Exec(ctx, `DROP DATABASE IF EXISTS "`+testDBName+`"`); err != nil {
		return nil, fmt.Errorf("drop stale test database: %w", err)
	}
	if _, err := maint.Exec(ctx, `CREATE DATABASE "`+testDBName+`"`); err != nil {
		return nil, fmt.Errorf("create test database: %w", err)
	}

	pool, err := pgxpool.New(ctx, dsn(testDBName))
	if err != nil {
		return nil, fmt.Errorf("open test database: %w", err)
	}
	dir := filepath.Join("..", "..", "migrations")
	names, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	sort.Strings(names)
	for _, name := range names {
		body, err := os.ReadFile(name)
		if err != nil {
			pool.Close()
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			pool.Close()
			return nil, fmt.Errorf("apply %s: %w", filepath.Base(name), err)
		}
	}
	return pool, nil
}

func TestMain(m *testing.M) {
	code := m.Run()
	if dbPool != nil {
		dbPool.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if maint, err := pgx.Connect(ctx, dsn(env("DB_MAINTENANCE_NAME", "postgres"))); err == nil {
			_, _ = maint.Exec(ctx, `DROP DATABASE IF EXISTS "`+testDBName+`" WITH (FORCE)`)
			_ = maint.Close(ctx)
		}
	}
	os.Exit(code)
}

func TestRefreshTokensConcurrentSingleWinner(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	userID := uuid.NewString()
	pwHash, err := crypto.HashPassword("correct-horse-battery")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO users (id, email, username, full_name, password_hash, is_active)
		 VALUES ($1, $2, $3, 'Concurrent User', $4, true)`,
		userID, "concurrent-"+userID+"@example.com", "conc_"+strings.ReplaceAll(userID[:8], "-", ""), pwHash,
	)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}

	repo := NewRepository(pool)
	userRepo := user.NewRepository(pool)
	jwtMgr := NewJWTManager("test-secret-key-at-least-32-bytes-long", 15*time.Minute)
	svc := NewService(repo, userRepo, pool, jwtMgr, 24*time.Hour, nil)

	initial, err := svc.issueTokens(ctx, userID, "test-agent", "127.0.0.1:1234", MethodPassword)
	if err != nil {
		t.Fatalf("issue initial tokens: %v", err)
	}

	const concurrency = 10
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, concurrency)
	pairs := make([]*TokenPair, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			pairs[idx], errs[idx] = svc.RefreshTokens(ctx, initial.RefreshToken, "test-agent", "127.0.0.1:1234")
		}(i)
	}
	close(start)
	wg.Wait()

	var successes, invalidCount int
	var winningPair *TokenPair
	for i := 0; i < concurrency; i++ {
		switch {
		case errs[i] == nil:
			successes++
			winningPair = pairs[i]
		case errors.Is(errs[i], ErrInvalidRefreshToken):
			invalidCount++
		default:
			t.Errorf("goroutine %d returned unexpected error: %v", i, errs[i])
		}
	}

	if successes != 1 {
		t.Fatalf("RefreshTokens successes = %d, want 1 (invalid = %d)", successes, invalidCount)
	}
	if invalidCount != concurrency-1 {
		t.Fatalf("RefreshTokens ErrInvalidRefreshToken count = %d, want %d", invalidCount, concurrency-1)
	}
	if winningPair == nil || winningPair.RefreshToken == "" || winningPair.RefreshToken == initial.RefreshToken {
		t.Fatalf("winning token pair invalid: %+v", winningPair)
	}

	var sessionCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM sessions WHERE user_id = $1`, userID).Scan(&sessionCount); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessionCount != 1 {
		t.Fatalf("session rows for user = %d, want 1", sessionCount)
	}
}
