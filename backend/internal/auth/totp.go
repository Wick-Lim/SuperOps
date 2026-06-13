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

	"github.com/Wick-Lim/SuperOps/backend/pkg/crypto"
)

// ErrTOTPRequired signals that a correct password was supplied but the account
// has 2FA enabled and no (valid) TOTP code was provided. Handlers translate
// this into a distinct response so the client can prompt for a code.
var ErrTOTPRequired = errors.New("totp code required")

const (
	totpDigits     = 6
	totpPeriodSecs = 30
	backupCodeCount = 10
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

func totpAt(secret string, t time.Time) (string, error) {
	key, err := base32NoPad.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return "", err
	}
	return hotp(key, uint64(t.Unix())/totpPeriodSecs), nil
}

// ValidateTOTP checks a code against the secret, allowing ±1 time step of skew.
func ValidateTOTP(secret, code string) bool {
	code = strings.TrimSpace(code)
	if len(code) != totpDigits {
		return false
	}
	now := time.Now()
	for _, skew := range []int{0, -1, 1} {
		want, err := totpAt(secret, now.Add(time.Duration(skew*totpPeriodSecs)*time.Second))
		if err == nil && subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return true
		}
	}
	return false
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

// SetupTOTP generates and stores a new (not-yet-enabled) secret for the user.
func (s *Service) SetupTOTP(ctx context.Context, userID string) (*TOTPSetup, error) {
	u, err := s.userRepo.GetByID(ctx, userID)
	if err != nil || u == nil {
		return nil, errors.New("user not found")
	}
	secret, err := GenerateTOTPSecret()
	if err != nil {
		return nil, err
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE users SET totp_secret = $2, totp_enabled = FALSE WHERE id = $1`, userID, secret); err != nil {
		return nil, fmt.Errorf("store totp secret: %w", err)
	}
	return &TOTPSetup{Secret: secret, OTPURL: TOTPProvisioningURI(secret, u.Email, "SuperOps")}, nil
}

// EnableTOTP verifies a code against the pending secret and, on success, enables
// 2FA and returns a fresh set of one-time backup codes (shown only once).
func (s *Service) EnableTOTP(ctx context.Context, userID, code string) ([]string, error) {
	var secret string
	var enabled bool
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(totp_secret,''), totp_enabled FROM users WHERE id = $1`, userID).Scan(&secret, &enabled); err != nil {
		return nil, errors.New("user not found")
	}
	if secret == "" {
		return nil, errors.New("run totp setup first")
	}
	if !ValidateTOTP(secret, code) {
		return nil, errors.New("invalid code")
	}

	codes := make([]string, backupCodeCount)
	for i := range codes {
		c, err := crypto.GenerateRandomToken(6) // ~8 url-safe chars
		if err != nil {
			return nil, err
		}
		codes[i] = strings.ToLower(c[:8])
	}

	err := func() error {
		if _, err := s.pool.Exec(ctx, `UPDATE users SET totp_enabled = TRUE WHERE id = $1`, userID); err != nil {
			return err
		}
		s.pool.Exec(ctx, `DELETE FROM totp_backup_codes WHERE user_id = $1`, userID)
		for _, c := range codes {
			h, err := crypto.HashPassword(c)
			if err != nil {
				return err
			}
			if _, err := s.pool.Exec(ctx,
				`INSERT INTO totp_backup_codes (user_id, code_hash) VALUES ($1, $2)`, userID, h); err != nil {
				return err
			}
		}
		return nil
	}()
	if err != nil {
		return nil, fmt.Errorf("enable totp: %w", err)
	}
	return codes, nil
}

// DisableTOTP turns off 2FA after verifying a current code (or backup code).
func (s *Service) DisableTOTP(ctx context.Context, userID, code string) error {
	var secret string
	var enabled bool
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(totp_secret,''), totp_enabled FROM users WHERE id = $1`, userID).Scan(&secret, &enabled); err != nil {
		return errors.New("user not found")
	}
	if !enabled {
		return nil
	}
	if !s.verifyTOTPOrBackup(ctx, userID, secret, code) {
		return errors.New("invalid code")
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE users SET totp_enabled = FALSE, totp_secret = NULL WHERE id = $1`, userID); err != nil {
		return err
	}
	s.pool.Exec(ctx, `DELETE FROM totp_backup_codes WHERE user_id = $1`, userID)
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
	if secret != "" && ValidateTOTP(secret, code) {
		return true
	}
	code = strings.ToLower(strings.TrimSpace(code))
	rows, err := s.pool.Query(ctx,
		`SELECT id, code_hash FROM totp_backup_codes WHERE user_id = $1 AND used = FALSE`, userID)
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
			s.pool.Exec(ctx, `UPDATE totp_backup_codes SET used = TRUE WHERE id = $1`, b.id)
			return true
		}
	}
	return false
}
