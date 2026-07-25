package block

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// ListByBlocker returns the users the given user has blocked.
func (r *Repository) ListByBlocker(ctx context.Context, blockerID string) ([]*Block, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT blocked_id, created_at FROM user_blocks WHERE blocker_id = $1 ORDER BY created_at DESC`,
		blockerID,
	)
	if err != nil {
		return nil, fmt.Errorf("list blocks: %w", err)
	}
	defer rows.Close()

	var blocks []*Block
	for rows.Next() {
		b := &Block{}
		if err := rows.Scan(&b.BlockedID, &b.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan block: %w", err)
		}
		blocks = append(blocks, b)
	}
	return blocks, rows.Err()
}

// Create inserts a block, ignoring duplicates.
func (r *Repository) Create(ctx context.Context, blockerID, blockedID string) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO user_blocks (blocker_id, blocked_id) VALUES ($1, $2)
		 ON CONFLICT (blocker_id, blocked_id) DO NOTHING`,
		blockerID, blockedID,
	)
	if err != nil {
		return fmt.Errorf("create block: %w", err)
	}
	return nil
}

// Delete removes a block and reports whether one existed, so unblocking
// somebody who was never blocked is not reported as a success.
func (r *Repository) Delete(ctx context.Context, blockerID, blockedID string) (bool, error) {
	tag, err := r.pool.Exec(ctx,
		`DELETE FROM user_blocks WHERE blocker_id = $1 AND blocked_id = $2`,
		blockerID, blockedID,
	)
	if err != nil {
		return false, fmt.Errorf("delete block: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// UserExists lets the handler answer 404 for an unknown target instead of
// letting the foreign-key violation become a 500.
func (r *Repository) UserExists(ctx context.Context, userID string) (bool, error) {
	var ok bool
	if err := r.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, userID,
	).Scan(&ok); err != nil {
		return false, fmt.Errorf("check user exists: %w", err)
	}
	return ok, nil
}
