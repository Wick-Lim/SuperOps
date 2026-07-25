package user

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// userColumns is the column list behind every User scan. status_text and
// status_emoji were missing from it, so PUT /users/me/status was write-only:
// the values went into Postgres and no read path ever brought them back.
const userColumns = `id, email, username, full_name, COALESCE(password_hash,''), avatar_url, status_text, status_emoji, timezone, locale, is_bot, is_active, last_active_at, created_at, updated_at`

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) Create(ctx context.Context, u *User) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO users (id, email, username, full_name, password_hash, avatar_url, timezone, locale, is_bot, is_active)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		u.ID, u.Email, u.Username, u.FullName, u.PasswordHash, u.AvatarURL, u.Timezone, u.Locale, u.IsBot, u.IsActive,
	)
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

func (r *Repository) GetByID(ctx context.Context, id string) (*User, error) {
	return scanUser(r.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = $1`, id))
}

func (r *Repository) GetByEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(r.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE email = $1`, email))
}

func (r *Repository) GetByUsername(ctx context.Context, username string) (*User, error) {
	return scanUser(r.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE username = $1`, username))
}

// Update writes the profile fields. updated_at is maintained by the BEFORE
// UPDATE trigger added in migration 009.
func (r *Repository) Update(ctx context.Context, u *User) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE users SET full_name = $2, avatar_url = $3, timezone = $4, locale = $5
		 WHERE id = $1`,
		u.ID, u.FullName, u.AvatarURL, u.Timezone, u.Locale,
	)
	if err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	return nil
}

// UpdateStatus sets the custom status line. It lived as a raw pool.Exec inside
// the handler, which is why the columns were never added to userColumns.
func (r *Repository) UpdateStatus(ctx context.Context, id, text, emoji string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE users SET status_text = $2, status_emoji = $3 WHERE id = $1`,
		id, text, emoji,
	)
	if err != nil {
		return fmt.Errorf("update user status: %w", err)
	}
	return nil
}

// TouchLastActive stamps users.last_active_at, throttled so an active client
// costs one write per interval rather than one per request. Nothing called the
// previous unconditional version, so the column was always NULL.
func (r *Repository) TouchLastActive(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE users SET last_active_at = NOW()
		  WHERE id = $1
		    AND (last_active_at IS NULL OR last_active_at < NOW() - INTERVAL '5 minutes')`, id)
	if err != nil {
		return fmt.Errorf("touch last active: %w", err)
	}
	return nil
}

// SearchInSharedWorkspaces finds users the caller may legitimately see: active,
// not blocked in either direction, and sharing at least one workspace with the
// caller.
//
// The previous query searched the entire users table and matched on email, so
// any authenticated caller could confirm the existence of an account in an
// unrelated tenant from an email fragment. Matching is restricted to username
// and full_name, which migration 009 backed with trigram indexes.
func (r *Repository) SearchInSharedWorkspaces(ctx context.Context, callerID, query string, limit int) ([]*User, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+userColumns+`
		 FROM users u
		 WHERE u.is_active = TRUE
		   AND u.id <> $3
		   AND (u.username ILIKE $1 OR u.full_name ILIKE $1)
		   AND EXISTS (
		         SELECT 1 FROM workspace_members caller
		         JOIN workspace_members target ON target.workspace_id = caller.workspace_id
		         WHERE caller.user_id = $3 AND target.user_id = u.id)
		   AND NOT EXISTS (
		         SELECT 1 FROM user_blocks b
		         WHERE (b.blocker_id = $3 AND b.blocked_id = u.id)
		            OR (b.blocked_id = $3 AND b.blocker_id = u.id))
		 ORDER BY u.username
		 LIMIT $2`,
		"%"+query+"%", limit, callerID,
	)
	if err != nil {
		return nil, fmt.Errorf("search users: %w", err)
	}
	defer rows.Close()

	users := []*User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// SharesWorkspace reports whether two users have any workspace in common.
//
// authz.Checker.SharesWorkspace is deliberately not used here: it additionally
// requires the actor to be an owner/admin, which is the right test for
// /api/v1/admin/* but would deny an ordinary member looking up a colleague.
func (r *Repository) SharesWorkspace(ctx context.Context, a, b string) (bool, error) {
	if a == b {
		return true, nil
	}
	var ok bool
	err := r.pool.QueryRow(ctx,
		`SELECT EXISTS(
		    SELECT 1
		      FROM workspace_members x
		      JOIN workspace_members y ON y.workspace_id = x.workspace_id
		     WHERE x.user_id = $1 AND y.user_id = $2
		 )`, a, b,
	).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check shared workspace: %w", err)
	}
	return ok, nil
}

// scanner is satisfied by both pgx.Row and pgx.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanUser(row scanner) (*User, error) {
	u := &User{}
	err := row.Scan(&u.ID, &u.Email, &u.Username, &u.FullName, &u.PasswordHash, &u.AvatarURL, &u.StatusText, &u.StatusEmoji, &u.Timezone, &u.Locale, &u.IsBot, &u.IsActive, &u.LastActiveAt, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan user: %w", err)
	}
	return u, nil
}
