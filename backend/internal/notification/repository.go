package notification

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) Create(ctx context.Context, n *Notification) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO notifications (id, user_id, type, title, body, data) VALUES ($1, $2, $3, $4, $5, $6::jsonb)`,
		n.ID, n.UserID, n.Type, n.Title, n.Body, n.Data,
	)
	if err != nil {
		return fmt.Errorf("create notification: %w", err)
	}
	return nil
}

// ListByUser pages by the total (created_at, id) key. Ordering on created_at
// alone dropped rows at a page boundary whenever two notifications shared a
// timestamp, which a single fan-out routinely produces.
func (r *Repository) ListByUser(ctx context.Context, userID string, cursor httputil.Cursor, limit int) ([]*Notification, error) {
	const cols = `SELECT id, user_id, type, title, body, data::text, is_read, created_at
		 FROM notifications WHERE user_id = $1`

	var rows pgx.Rows
	var err error
	if cursor.IsZero() {
		rows, err = r.pool.Query(ctx,
			cols+` ORDER BY created_at DESC, id DESC LIMIT $2`,
			userID, limit)
	} else {
		rows, err = r.pool.Query(ctx,
			cols+` AND (created_at, id) < ($2, $3) ORDER BY created_at DESC, id DESC LIMIT $4`,
			userID, cursor.CreatedAt, cursor.ID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("list notifications: %w", err)
	}
	defer rows.Close()

	var notifications []*Notification
	for rows.Next() {
		n := &Notification{}
		if err := rows.Scan(&n.ID, &n.UserID, &n.Type, &n.Title, &n.Body, &n.Data, &n.IsRead, &n.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan notification: %w", err)
		}
		notifications = append(notifications, n)
	}
	return notifications, rows.Err()
}

// MarkRead reports whether a notification owned by the caller was updated, so a
// no-op is distinguishable from a success.
func (r *Repository) MarkRead(ctx context.Context, id, userID string) (bool, error) {
	tag, err := r.pool.Exec(ctx,
		`UPDATE notifications SET is_read = TRUE WHERE id = $1 AND user_id = $2 AND is_read = FALSE`, id, userID)
	if err != nil {
		return false, fmt.Errorf("mark read: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return true, nil
	}
	// Already read is still a success; only a missing (or someone else's) row is not.
	var exists bool
	if err := r.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM notifications WHERE id = $1 AND user_id = $2)`, id, userID,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("mark read: %w", err)
	}
	return exists, nil
}

func (r *Repository) MarkAllRead(ctx context.Context, userID string) (int64, error) {
	tag, err := r.pool.Exec(ctx, `UPDATE notifications SET is_read = TRUE WHERE user_id = $1 AND is_read = FALSE`, userID)
	if err != nil {
		return 0, fmt.Errorf("mark all read: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (r *Repository) UnreadCount(ctx context.Context, userID string) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM notifications WHERE user_id = $1 AND is_read = FALSE`, userID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("unread count: %w", err)
	}
	return count, nil
}
