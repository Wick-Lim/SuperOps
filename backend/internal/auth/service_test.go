package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

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
