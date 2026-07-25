package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Wick-Lim/SuperOps/backend/pkg/crypto"
)

// The surface internal/sso is allowed to reach into.
//
// It is four methods, and the list of what is NOT here is the point: single
// sign-on can mint a session, ask whether a second factor exists, spend one,
// and check a password. It cannot enrol a factor, disable one, read a hash, or
// mint a session for an account it has not just proven something about.
//
// Everything credential-shaped stays in this package, so the rules about
// deactivation, replay and second factors have exactly one implementation.

// IssueSSOSession mints a token pair for a user who authenticated through an
// identity provider.
//
// It re-reads is_active rather than trusting the caller. internal/sso checks it
// too, but a deactivation landing between that check and this one must not
// produce a session — and this is the function that would produce it.
func (s *Service) IssueSSOSession(ctx context.Context, userID, userAgent, ipAddress string) (*TokenPair, error) {
	var isActive bool
	err := s.pool.QueryRow(ctx, `SELECT is_active FROM users WHERE id = $1`, userID).Scan(&isActive)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, fmt.Errorf("load user for sso session: %w", err)
	}
	if !isActive {
		return nil, ErrInvalidCredentials
	}

	// MethodSSO, so that refresh-token rotation does not re-evaluate password
	// enforcement against it — and so that an operator reading the sessions
	// table can tell the two apart.
	return s.issueTokens(ctx, userID, userAgent, ipAddress, MethodSSO)
}

// SecondFactorEnabled reports whether the account has TOTP enrolled.
//
// Unlike TOTPEnabled it returns the error. A federated login has to fail closed
// on a database fault: swallowing it would answer "no second factor" and let
// the login through without one.
func (s *Service) SecondFactorEnabled(ctx context.Context, userID string) (bool, error) {
	var enabled bool
	err := s.pool.QueryRow(ctx, `SELECT totp_enabled FROM users WHERE id = $1`, userID).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load totp state: %w", err)
	}
	return enabled, nil
}

// ConsumeSecondFactor spends a TOTP code or a backup code.
//
// It goes through the same verifyTOTPOrBackup as password login, which means
// the same replay guard: the time step is burned atomically, so a code cannot
// be used once at the password prompt and again at the SSO prompt.
func (s *Service) ConsumeSecondFactor(ctx context.Context, userID, code string) (bool, error) {
	if code == "" {
		return false, nil
	}
	var secret string
	var enabled bool
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(totp_secret,''), totp_enabled FROM users WHERE id = $1`, userID,
	).Scan(&secret, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load totp state: %w", err)
	}
	if !enabled {
		return false, nil
	}
	return s.verifyTOTPOrBackup(ctx, userID, secret, code), nil
}

// VerifyPassword checks an account's password without minting anything.
//
// It is the proof required to bind a single sign-on identity to an existing
// local account. An account with no password cannot be linked this way and
// answers false — there is nothing to prove control with, and "no password"
// must not read as "any password".
func (s *Service) VerifyPassword(ctx context.Context, userID, password string) (bool, error) {
	u, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("load user: %w", err)
	}

	// One bcrypt comparison either way, for the same reason Login pays for one:
	// the caller must not be able to tell "no such account" from "no password
	// set" from "wrong password" by timing.
	hash := dummyPasswordHash
	if u != nil && u.PasswordHash != "" {
		hash = u.PasswordHash
	}
	ok := crypto.CheckPassword(password, hash)

	if u == nil || !u.IsActive || u.PasswordHash == "" {
		return false, nil
	}
	return ok, nil
}
