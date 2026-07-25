package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Wick-Lim/SuperOps/backend/internal/audit"
	"github.com/Wick-Lim/SuperOps/backend/pkg/crypto"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
)

// ErrTOTPRequired signals that a correct password was supplied but the account
// has 2FA enabled and no (valid) TOTP code was provided. Handlers translate
// this into a distinct response so the client can prompt for a code.
var ErrTOTPRequired = errors.New("totp code required")

// ErrTOTPSetupMissing means EnableTOTP was called before SetupTOTP.
var ErrTOTPSetupMissing = errors.New("run totp setup first")

const (
	totpDigits      = 6
	totpPeriodSecs  = 30
	backupCodeCount = 10

	// backupCodeAlphabet is an explicit unambiguous alphabet: no i/l/o/0/1, so
	// a code can be read off a printout without transcription errors. Folding
	// base64url to lowercase (what this used to do) collapsed A-Z onto a-z and
	// threw away ~6 bits per code for nothing.
	backupCodeAlphabet = "abcdefghjkmnpqrstuvwxyz23456789" // 31 symbols
	// 31^10 ~= 2^49.6 — comfortably above the 40-bit floor.
	backupCodeLen = 10
	// Legacy codes issued before the alphabet change are 8 chars of lowercased
	// base64url; the shape test has to keep accepting them.
	backupCodeMinLen = 8
)

var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateTOTPSecret returns a new base32-encoded shared secret.
func GenerateTOTPSecret() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base32NoPad.EncodeToString(b), nil
}

// GenerateBackupCode returns one recovery code drawn uniformly from
// backupCodeAlphabet. Bytes at or above the largest multiple of the alphabet
// size are rejected rather than folded, so the distribution stays flat.
func GenerateBackupCode() (string, error) {
	const n = len(backupCodeAlphabet)
	limit := byte(256 - (256 % n)) // 248 for n=31
	out := make([]byte, 0, backupCodeLen)
	buf := make([]byte, backupCodeLen)
	for len(out) < backupCodeLen {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("generate backup code: %w", err)
		}
		for _, b := range buf {
			if b >= limit {
				continue
			}
			out = append(out, backupCodeAlphabet[int(b)%n])
			if len(out) == backupCodeLen {
				break
			}
		}
	}
	return string(out), nil
}

// looksLikeBackupCode reports whether a submitted second factor could be a
// backup code at all. Backup-code verification costs one bcrypt comparison per
// stored code, so an unauthenticated login must not reach it with a value that
// plainly is not one (a 6-digit TOTP code, or arbitrary attacker input).
func looksLikeBackupCode(code string) bool {
	if len(code) < backupCodeMinLen || len(code) > backupCodeLen {
		return false
	}
	for i := 0; i < len(code); i++ {
		c := code[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

func hotp(key []byte, counter uint64) string {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(buf)
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := (int(sum[offset])&0x7f)<<24 |
		(int(sum[offset+1])&0xff)<<16 |
		(int(sum[offset+2])&0xff)<<8 |
		(int(sum[offset+3]) & 0xff)
	return fmt.Sprintf("%0*d", totpDigits, value%1_000_000)
}

func totpAtStep(secret string, step int64) (string, error) {
	key, err := base32NoPad.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return "", err
	}
	return hotp(key, uint64(step)), nil
}

func totpAt(secret string, t time.Time) (string, error) {
	return totpAtStep(secret, t.Unix()/totpPeriodSecs)
}

// matchTOTPStep returns the time step a code corresponds to, allowing ±1 step
// of clock skew. The step is what makes single-use enforcement possible.
func matchTOTPStep(secret, code string, now time.Time) (int64, bool) {
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return 0, false
	}
	current := now.Unix() / totpPeriodSecs
	for _, skew := range []int64{0, -1, 1} {
		step := current + skew
		want, err := totpAtStep(secret, step)
		if err == nil && subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// ValidateTOTP checks a code against the secret, allowing ±1 time step of skew.
// It does NOT consume the code — service code must use Service.consumeTOTP so
// that a code cannot be replayed inside the skew window.
func ValidateTOTP(secret, code string) bool {
	_, ok := matchTOTPStep(secret, code, time.Now())
	return ok
}

// TOTPProvisioningURI builds an otpauth:// URI for authenticator-app QR codes.
func TOTPProvisioningURI(secret, account, issuer string) string {
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", issuer)
	v.Set("algorithm", "SHA1")
	v.Set("digits", fmt.Sprint(totpDigits))
	v.Set("period", fmt.Sprint(totpPeriodSecs))
	label := url.PathEscape(issuer + ":" + account)
	return "otpauth://totp/" + label + "?" + v.Encode()
}

// --- Service methods ---

type TOTPSetup struct {
	Secret string `json:"secret"`
	OTPURL string `json:"otpauth_url"`
}

// consumeTOTP validates a code and atomically burns its time step.
//
// users.totp_last_step holds the highest step this account has authenticated
// with; the guarded UPDATE both rejects a replay of an already-spent code
// (including one still inside the ±1-step skew window) and settles a race
// between two concurrent logins presenting the same code.
func (s *Service) consumeTOTP(ctx context.Context, userID, secret, code string) bool {
	if secret == "" {
		return false
	}
	step, ok := matchTOTPStep(secret, code, time.Now())
	if !ok {
		return false
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET totp_last_step = $2 WHERE id = $1 AND totp_last_step < $2`, userID, step)
	return err == nil && tag.RowsAffected() > 0
}

// SetupTOTP generates and stores a new (not-yet-enabled) secret for the user.
//
// Because it also clears totp_enabled, an unguarded call is a 2FA *disable*
// reachable with nothing but a stolen access token. When 2FA is already on,
// the caller must re-authenticate first — the same bar DisableTOTP sets.
func (s *Service) SetupTOTP(ctx context.Context, userID, password, code, ipAddress string) (*TOTPSetup, error) {
	u, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("load user: %w", err)
	}
	if u == nil {
		return nil, ErrInvalidCredentials
	}

	var currentSecret string
	var enabled bool
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(totp_secret,''), totp_enabled FROM users WHERE id = $1`, userID,
	).Scan(&currentSecret, &enabled); err != nil {
		return nil, fmt.Errorf("load totp state: %w", err)
	}
	if enabled && !s.reauthenticate(ctx, userID, currentSecret, u.PasswordHash, password, code) {
		return nil, ErrReauthRequired
	}

	secret, err := GenerateTOTPSecret()
	if err != nil {
		return nil, err
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE users SET totp_secret = $2, totp_enabled = FALSE WHERE id = $1`, userID, secret); err != nil {
		return nil, fmt.Errorf("store totp secret: %w", err)
	}
	if enabled {
		// Re-enrolment turned 2FA off; that is a security-relevant state change.
		s.record(ctx, audit.Entry{
			ActorID:      userID,
			Action:       audit.ActionTOTPDisabled,
			ResourceType: "user",
			ResourceID:   userID,
			IPAddress:    extractIP(ipAddress),
			Metadata:     map[string]interface{}{"reason": "re-enrollment"},
		})
	}
	return &TOTPSetup{Secret: secret, OTPURL: TOTPProvisioningURI(secret, u.Email, "SuperOps")}, nil
}

// reauthenticate accepts either the account password or a current second
// factor as proof that the request is not merely a replayed access token.
func (s *Service) reauthenticate(ctx context.Context, userID, secret, passwordHash, password, code string) bool {
	if password != "" && passwordHash != "" && crypto.CheckPassword(password, passwordHash) {
		return true
	}
	return code != "" && s.verifyTOTPOrBackup(ctx, userID, secret, code)
}

// EnableTOTP verifies a code against the pending secret and, on success, enables
// 2FA and returns a fresh set of one-time backup codes (shown only once).
func (s *Service) EnableTOTP(ctx context.Context, userID, code, ipAddress string) ([]string, error) {
	var secret string
	var enabled bool
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(totp_secret,''), totp_enabled FROM users WHERE id = $1`, userID,
	).Scan(&secret, &enabled); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("load totp state: %w", err)
	}
	if secret == "" {
		return nil, ErrTOTPSetupMissing
	}
	if !s.consumeTOTP(ctx, userID, secret, code) {
		return nil, ErrInvalidTOTPCode
	}

	codes := make([]string, backupCodeCount)
	for i := range codes {
		c, err := GenerateBackupCode()
		if err != nil {
			return nil, err
		}
		codes[i] = c
	}

	err := database.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE users SET totp_enabled = TRUE WHERE id = $1`, userID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM totp_backup_codes WHERE user_id = $1`, userID); err != nil {
			return err
		}
		for _, c := range codes {
			h, err := crypto.HashPassword(c)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO totp_backup_codes (user_id, code_hash) VALUES ($1, $2)`, userID, h); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("enable totp: %w", err)
	}

	s.record(ctx, audit.Entry{
		ActorID:      userID,
		Action:       audit.ActionTOTPEnabled,
		ResourceType: "user",
		ResourceID:   userID,
		IPAddress:    extractIP(ipAddress),
	})
	return codes, nil
}

// DisableTOTP turns off 2FA after verifying a current code (or backup code).
func (s *Service) DisableTOTP(ctx context.Context, userID, code, ipAddress string) error {
	var secret string
	var enabled bool
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(totp_secret,''), totp_enabled FROM users WHERE id = $1`, userID,
	).Scan(&secret, &enabled); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidCredentials
		}
		return fmt.Errorf("load totp state: %w", err)
	}
	if !enabled {
		return nil
	}
	if !s.verifyTOTPOrBackup(ctx, userID, secret, code) {
		return ErrInvalidTOTPCode
	}
	if err := database.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE users SET totp_enabled = FALSE, totp_secret = NULL WHERE id = $1`, userID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM totp_backup_codes WHERE user_id = $1`, userID)
		return err
	}); err != nil {
		return fmt.Errorf("disable totp: %w", err)
	}

	s.record(ctx, audit.Entry{
		ActorID:      userID,
		Action:       audit.ActionTOTPDisabled,
		ResourceType: "user",
		ResourceID:   userID,
		IPAddress:    extractIP(ipAddress),
	})
	return nil
}

// TOTPEnabled reports whether the user has 2FA active.
func (s *Service) TOTPEnabled(ctx context.Context, userID string) bool {
	var enabled bool
	_ = s.pool.QueryRow(ctx, `SELECT totp_enabled FROM users WHERE id = $1`, userID).Scan(&enabled)
	return enabled
}

// verifyTOTPOrBackup accepts a valid TOTP code or consumes an unused backup code.
func (s *Service) verifyTOTPOrBackup(ctx context.Context, userID, secret, code string) bool {
	if s.consumeTOTP(ctx, userID, secret, code) {
		return true
	}
	code = strings.ToLower(strings.TrimSpace(code))
	// Bounded work: only a value shaped like a backup code reaches bcrypt, and
	// never more comparisons than a full set of codes.
	if !looksLikeBackupCode(code) {
		return false
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, code_hash FROM totp_backup_codes WHERE user_id = $1 AND used = FALSE
		 ORDER BY created_at LIMIT $2`, userID, backupCodeCount)
	if err != nil {
		return false
	}
	defer rows.Close()
	type bc struct{ id, hash string }
	var candidates []bc
	for rows.Next() {
		var b bc
		if err := rows.Scan(&b.id, &b.hash); err == nil {
			candidates = append(candidates, b)
		}
	}
	for _, b := range candidates {
		if crypto.CheckPassword(code, b.hash) {
			// Atomically consume the code; if a concurrent request already used
			// it (0 rows affected), this attempt does not authenticate.
			tag, err := s.pool.Exec(ctx, `UPDATE totp_backup_codes SET used = TRUE WHERE id = $1 AND used = FALSE`, b.id)
			if err == nil && tag.RowsAffected() > 0 {
				return true
			}
			return false
		}
	}
	return false
}
