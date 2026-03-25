package message

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) Create(ctx context.Context, m *Message) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO messages (id, channel_id, user_id, parent_id, content, content_type)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		m.ID, m.ChannelID, m.UserID, m.ParentID, m.Content, m.ContentType,
	)
	if err != nil {
		return fmt.Errorf("create message: %w", err)
	}

	// Increment parent reply count if this is a thread reply
	if m.ParentID != nil {
		_, err = r.pool.Exec(ctx,
			`UPDATE messages SET reply_count = reply_count + 1 WHERE id = $1`, *m.ParentID,
		)
		if err != nil {
			return fmt.Errorf("increment reply count: %w", err)
		}
	}

	return nil
}

func (r *Repository) GetByID(ctx context.Context, id string) (*Message, error) {
	m := &Message{}
	err := r.pool.QueryRow(ctx,
		`SELECT id, channel_id, user_id, parent_id, content, content_type, is_edited, is_deleted, reply_count, created_at, updated_at
		 FROM messages WHERE id = $1`, id,
	).Scan(&m.ID, &m.ChannelID, &m.UserID, &m.ParentID, &m.Content, &m.ContentType, &m.IsEdited, &m.IsDeleted, &m.ReplyCount, &m.CreatedAt, &m.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get message: %w", err)
	}
	return m, nil
}

func (r *Repository) ListByChannel(ctx context.Context, channelID string, before time.Time, limit int) ([]*Message, error) {
	query := `SELECT id, channel_id, user_id, parent_id, content, content_type, is_edited, is_deleted, reply_count, created_at, updated_at
		 FROM messages
		 WHERE channel_id = $1 AND parent_id IS NULL AND is_deleted = FALSE`

	var rows pgx.Rows
	var err error

	if before.IsZero() {
		query += ` ORDER BY created_at DESC LIMIT $2`
		rows, err = r.pool.Query(ctx, query, channelID, limit)
	} else {
		query += ` AND created_at < $2 ORDER BY created_at DESC LIMIT $3`
		rows, err = r.pool.Query(ctx, query, channelID, before, limit)
	}

	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	return r.scanMessages(rows)
}

func (r *Repository) ListThread(ctx context.Context, parentID string, limit int) ([]*Message, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, channel_id, user_id, parent_id, content, content_type, is_edited, is_deleted, reply_count, created_at, updated_at
		 FROM messages
		 WHERE parent_id = $1 AND is_deleted = FALSE
		 ORDER BY created_at ASC
		 LIMIT $2`, parentID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list thread: %w", err)
	}
	defer rows.Close()

	return r.scanMessages(rows)
}

func (r *Repository) Update(ctx context.Context, id, content string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE messages SET content = $2, is_edited = TRUE, updated_at = NOW() WHERE id = $1`,
		id, content,
	)
	if err != nil {
		return fmt.Errorf("update message: %w", err)
	}
	return nil
}

func (r *Repository) SoftDelete(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE messages SET is_deleted = TRUE, content = '', updated_at = NOW() WHERE id = $1`, id,
	)
	if err != nil {
		return fmt.Errorf("soft delete message: %w", err)
	}
	return nil
}

// Reactions

func (r *Repository) AddReaction(ctx context.Context, reaction *Reaction) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO reactions (id, message_id, user_id, emoji) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (message_id, user_id, emoji) DO NOTHING`,
		reaction.ID, reaction.MessageID, reaction.UserID, reaction.Emoji,
	)
	if err != nil {
		return fmt.Errorf("add reaction: %w", err)
	}
	return nil
}

func (r *Repository) RemoveReaction(ctx context.Context, messageID, userID, emoji string) error {
	_, err := r.pool.Exec(ctx,
		`DELETE FROM reactions WHERE message_id = $1 AND user_id = $2 AND emoji = $3`,
		messageID, userID, emoji,
	)
	if err != nil {
		return fmt.Errorf("remove reaction: %w", err)
	}
	return nil
}

func (r *Repository) ListReactions(ctx context.Context, messageID string) ([]*Reaction, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, message_id, user_id, emoji, created_at FROM reactions WHERE message_id = $1 ORDER BY created_at`, messageID,
	)
	if err != nil {
		return nil, fmt.Errorf("list reactions: %w", err)
	}
	defer rows.Close()

	var reactions []*Reaction
	for rows.Next() {
		re := &Reaction{}
		if err := rows.Scan(&re.ID, &re.MessageID, &re.UserID, &re.Emoji, &re.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan reaction: %w", err)
		}
		reactions = append(reactions, re)
	}
	return reactions, nil
}

func (r *Repository) scanMessages(rows pgx.Rows) ([]*Message, error) {
	var messages []*Message
	for rows.Next() {
		m := &Message{}
		if err := rows.Scan(&m.ID, &m.ChannelID, &m.UserID, &m.ParentID, &m.Content, &m.ContentType, &m.IsEdited, &m.IsDeleted, &m.ReplyCount, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		messages = append(messages, m)
	}
	return messages, nil
}
