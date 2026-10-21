package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/pkg/crypto"
)

// Session mirrors a row of the sessions table. Only the SHA-256 of the refresh
// token is ever persisted or read back; the plaintext exists solely in the
// login/refresh response body.
type Session struct {
	ID               string
	UserID           string
	RefreshTokenHash string
	UserAgent        string
	IPAddress        string
	// AuthMethod is how the session was obtained: MethodPassword or MethodSSO.
	// Refresh re-evaluates SSO enforcement for a password session and not for
	// an SSO one, which is only expressible because the row remembers.
	AuthMethod string
	ExpiresAt  time.Time
	CreatedAt  time.Time
}

// Session authentication methods. Mirrors the CHECK constraint on
// sessions.auth_method (migrations/014).
const (
	MethodPassword = "password"
	MethodSSO      = "sso"
)

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

type querier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (r *Repository) CreateSession(ctx context.Context, s *Session) error {
	return createSession(ctx, r.pool, s)
}

func (r *Repository) CreateSessionTx(ctx context.Context, tx pgx.Tx, s *Session) error {
	return createSession(ctx, tx, s)
}

func createSession(ctx context.Context, q querier, s *Session) error {
	if s.AuthMethod == "" {
		s.AuthMethod = MethodPassword
	}
	_, err := q.Exec(ctx,
		`INSERT INTO sessions (id, user_id, refresh_token_hash, user_agent, ip_address, expires_at, auth_method)
		 VALUES ($1, $2, $3, $4, $5::inet, $6, $7)`,
		s.ID, s.UserID, s.RefreshTokenHash, s.UserAgent, s.IPAddress, s.ExpiresAt, s.AuthMethod,
	)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// GetSessionByToken looks a session up by the plaintext refresh token; the
// hashing happens here so no caller is tempted to query the column directly.
func (r *Repository) GetSessionByToken(ctx context.Context, refreshToken string) (*Session, error) {
	s := &Session{}
	err := r.pool.QueryRow(ctx,
		`SELECT id, user_id, refresh_token_hash, user_agent, COALESCE(host(ip_address),''), auth_method, expires_at, created_at
		 FROM sessions WHERE refresh_token_hash = $1`,
		crypto.HashToken(refreshToken),
	).Scan(&s.ID, &s.UserID, &s.RefreshTokenHash, &s.UserAgent, &s.IPAddress, &s.AuthMethod, &s.ExpiresAt, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	return s, nil
}

// ConsumeSessionByToken atomically deletes and returns the session matching
// refreshToken so concurrent callers carrying the same token cannot both claim
// it.
func (r *Repository) ConsumeSessionByToken(ctx context.Context, refreshToken string) (*Session, error) {
	return consumeSessionByToken(ctx, r.pool, refreshToken)
}

// ConsumeSessionByTokenTx atomically deletes and returns the session matching
// refreshToken inside tx. Concurrent transactions targeting the same token
// serialize on the DELETE row lock; once the winner commits, every other
// caller observes pgx.ErrNoRows (nil, nil).
func (r *Repository) ConsumeSessionByTokenTx(ctx context.Context, tx pgx.Tx, refreshToken string) (*Session, error) {
	return consumeSessionByToken(ctx, tx, refreshToken)
}

func consumeSessionByToken(ctx context.Context, q querier, refreshToken string) (*Session, error) {
	s := &Session{}
	err := q.QueryRow(ctx,
		`DELETE FROM sessions
		  WHERE refresh_token_hash = $1
		  RETURNING id, user_id, refresh_token_hash, user_agent, COALESCE(host(ip_address),''), auth_method, expires_at, created_at`,
		crypto.HashToken(refreshToken),
	).Scan(&s.ID, &s.UserID, &s.RefreshTokenHash, &s.UserAgent, &s.IPAddress, &s.AuthMethod, &s.ExpiresAt, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("consume session: %w", err)
	}
	return s, nil
}

func (r *Repository) DeleteSession(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

func (r *Repository) DeleteUserSessions(ctx context.Context, userID string) error {
	return deleteUserSessions(ctx, r.pool, userID)
}

func (r *Repository) DeleteUserSessionsTx(ctx context.Context, tx pgx.Tx, userID string) error {
	return deleteUserSessions(ctx, tx, userID)
}

func deleteUserSessions(ctx context.Context, q querier, userID string) error {
	_, err := q.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID)
	if err != nil {
		return fmt.Errorf("delete user sessions: %w", err)
	}
	return nil
}

// CleanExpiredSessions drops sessions past their expiry. The background worker
// runs this on a timer.
func (r *Repository) CleanExpiredSessions(ctx context.Context) (int64, error) {
	tag, err := r.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at < NOW()`)
	if err != nil {
		return 0, fmt.Errorf("clean expired sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}
