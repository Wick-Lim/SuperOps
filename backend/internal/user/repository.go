package user

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) Pool() *pgxpool.Pool {
	return r.pool
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
	return r.scanUser(r.pool.QueryRow(ctx,
		`SELECT id, email, username, full_name, COALESCE(password_hash,''), avatar_url, timezone, locale, is_bot, is_active, last_active_at, created_at, updated_at
		 FROM users WHERE id = $1`, id))
}

func (r *Repository) GetByEmail(ctx context.Context, email string) (*User, error) {
	return r.scanUser(r.pool.QueryRow(ctx,
		`SELECT id, email, username, full_name, COALESCE(password_hash,''), avatar_url, timezone, locale, is_bot, is_active, last_active_at, created_at, updated_at
		 FROM users WHERE email = $1`, email))
}

func (r *Repository) GetByUsername(ctx context.Context, username string) (*User, error) {
	return r.scanUser(r.pool.QueryRow(ctx,
		`SELECT id, email, username, full_name, COALESCE(password_hash,''), avatar_url, timezone, locale, is_bot, is_active, last_active_at, created_at, updated_at
		 FROM users WHERE username = $1`, username))
}

func (r *Repository) Update(ctx context.Context, u *User) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE users SET full_name = $2, avatar_url = $3, timezone = $4, locale = $5, updated_at = NOW()
		 WHERE id = $1`,
		u.ID, u.FullName, u.AvatarURL, u.Timezone, u.Locale,
	)
	if err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	return nil
}

func (r *Repository) UpdateLastActive(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx, `UPDATE users SET last_active_at = NOW() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("update last active: %w", err)
	}
	return nil
}

func (r *Repository) Search(ctx context.Context, query string, limit int) ([]*User, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, email, username, full_name, COALESCE(password_hash,''), avatar_url, timezone, locale, is_bot, is_active, last_active_at, created_at, updated_at
		 FROM users
		 WHERE is_active = TRUE AND (username ILIKE $1 OR full_name ILIKE $1 OR email ILIKE $1)
		 ORDER BY username
		 LIMIT $2`,
		"%"+query+"%", limit,
	)
	if err != nil {
		return nil, fmt.Errorf("search users: %w", err)
	}
	defer rows.Close()

	var users []*User
	for rows.Next() {
		u, err := r.scanUserFromRows(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, nil
}

func (r *Repository) scanUser(row pgx.Row) (*User, error) {
	u := &User{}
	err := row.Scan(&u.ID, &u.Email, &u.Username, &u.FullName, &u.PasswordHash, &u.AvatarURL, &u.Timezone, &u.Locale, &u.IsBot, &u.IsActive, &u.LastActiveAt, &u.CreatedAt, &u.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan user: %w", err)
	}
	return u, nil
}

func (r *Repository) scanUserFromRows(rows pgx.Rows) (*User, error) {
	u := &User{}
	err := rows.Scan(&u.ID, &u.Email, &u.Username, &u.FullName, &u.PasswordHash, &u.AvatarURL, &u.Timezone, &u.Locale, &u.IsBot, &u.IsActive, &u.LastActiveAt, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("scan user row: %w", err)
	}
	return u, nil
}
