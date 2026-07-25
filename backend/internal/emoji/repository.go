package emoji

import (
	"context"
	"errors"
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

func (r *Repository) ListByWorkspace(ctx context.Context, workspaceID string) ([]*Emoji, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, workspace_id, name, image_url, created_by, created_at
		 FROM custom_emojis WHERE workspace_id = $1 ORDER BY name`, workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list emojis: %w", err)
	}
	defer rows.Close()

	emojis := []*Emoji{}
	for rows.Next() {
		e := &Emoji{}
		if err := rows.Scan(&e.ID, &e.WorkspaceID, &e.Name, &e.ImageURL, &e.CreatedBy, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan emoji: %w", err)
		}
		emojis = append(emojis, e)
	}
	return emojis, rows.Err()
}

// Create inserts an emoji. A unique-constraint violation is returned to the
// caller unwrapped enough for errors.As(*pgconn.PgError) to see it, so the
// handler can answer 409 instead of 500.
func (r *Repository) Create(ctx context.Context, e *Emoji) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO custom_emojis (id, workspace_id, name, image_url, created_by)
		 VALUES ($1, $2, $3, $4, $5)`,
		e.ID, e.WorkspaceID, e.Name, e.ImageURL, e.CreatedBy,
	)
	if err != nil {
		return fmt.Errorf("create emoji: %w", err)
	}
	return nil
}

// GetByID returns the emoji or (nil, nil) when it does not exist.
func (r *Repository) GetByID(ctx context.Context, id string) (*Emoji, error) {
	e := &Emoji{}
	err := r.pool.QueryRow(ctx,
		`SELECT id, workspace_id, name, image_url, created_by, created_at
		 FROM custom_emojis WHERE id = $1`, id,
	).Scan(&e.ID, &e.WorkspaceID, &e.Name, &e.ImageURL, &e.CreatedBy, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get emoji: %w", err)
	}
	return e, nil
}

// Delete reports whether a row was actually removed.
func (r *Repository) Delete(ctx context.Context, id string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `DELETE FROM custom_emojis WHERE id = $1`, id)
	if err != nil {
		return false, fmt.Errorf("delete emoji: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
