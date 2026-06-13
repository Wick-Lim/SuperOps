package message

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
)

// messageColumns is the canonical column list (and scan order) for a message row.
const messageColumns = `id, channel_id, user_id, parent_id, content, content_type,
	is_edited, is_deleted, reply_count, is_pinned, pinned_by, pinned_at,
	scheduled_at, is_scheduled, created_at, updated_at`

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// Create inserts a message and, atomically in the same transaction, bumps the
// parent reply_count (for thread replies) and the channel's last_message_at.
func (r *Repository) Create(ctx context.Context, m *Message) error {
	return database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO messages (id, channel_id, user_id, parent_id, content, content_type)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			m.ID, m.ChannelID, m.UserID, m.ParentID, m.Content, m.ContentType,
		); err != nil {
			return fmt.Errorf("create message: %w", err)
		}

		if m.ParentID != nil {
			if _, err := tx.Exec(ctx,
				`UPDATE messages SET reply_count = reply_count + 1 WHERE id = $1`, *m.ParentID,
			); err != nil {
				return fmt.Errorf("increment reply count: %w", err)
			}
		}

		if _, err := tx.Exec(ctx,
			`UPDATE channels SET last_message_at = NOW() WHERE id = $1`, m.ChannelID,
		); err != nil {
			return fmt.Errorf("update last message: %w", err)
		}
		return nil
	})
}

// CreateScheduled inserts a message in the scheduled (not-yet-sent) state. It
// does not bump channel last_message_at or reply_count — the worker does that
// when the message becomes due.
func (r *Repository) CreateScheduled(ctx context.Context, m *Message, scheduledAt time.Time) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO messages (id, channel_id, user_id, parent_id, content, content_type, is_scheduled, scheduled_at)
		 VALUES ($1, $2, $3, $4, $5, $6, TRUE, $7)`,
		m.ID, m.ChannelID, m.UserID, m.ParentID, m.Content, m.ContentType, scheduledAt,
	)
	if err != nil {
		return fmt.Errorf("create scheduled message: %w", err)
	}
	return nil
}

// ListScheduled returns a user's pending scheduled messages in a channel.
func (r *Repository) ListScheduled(ctx context.Context, channelID, userID string) ([]*Message, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+messageColumns+`
		 FROM messages
		 WHERE channel_id = $1 AND user_id = $2 AND is_scheduled = TRUE AND is_deleted = FALSE
		 ORDER BY scheduled_at ASC`, channelID, userID)
	if err != nil {
		return nil, fmt.Errorf("list scheduled: %w", err)
	}
	defer rows.Close()
	return r.scanMessages(rows)
}

// CancelScheduled deletes a not-yet-sent scheduled message owned by the user.
func (r *Repository) CancelScheduled(ctx context.Context, messageID, userID string) (bool, error) {
	tag, err := r.pool.Exec(ctx,
		`DELETE FROM messages WHERE id = $1 AND user_id = $2 AND is_scheduled = TRUE`, messageID, userID)
	if err != nil {
		return false, fmt.Errorf("cancel scheduled: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// LinkFiles attaches the caller's previously-uploaded files to a message.
func (r *Repository) LinkFiles(ctx context.Context, messageID, userID string, fileIDs []string) error {
	if len(fileIDs) == 0 {
		return nil
	}
	_, err := r.pool.Exec(ctx,
		`UPDATE files SET message_id = $1 WHERE id = ANY($2) AND user_id = $3 AND message_id IS NULL`,
		messageID, fileIDs, userID)
	if err != nil {
		return fmt.Errorf("link files: %w", err)
	}
	return nil
}

// hydrateFiles attaches file metadata to messages with a single query.
func (r *Repository) hydrateFiles(ctx context.Context, messages []*Message) error {
	if len(messages) == 0 {
		return nil
	}
	ids := make([]string, len(messages))
	byID := make(map[string]*Message, len(messages))
	for i, m := range messages {
		ids[i] = m.ID
		byID[m.ID] = m
	}
	rows, err := r.pool.Query(ctx,
		`SELECT id, message_id, name, content_type, size_bytes FROM files WHERE message_id = ANY($1)`, ids)
	if err != nil {
		return fmt.Errorf("hydrate files: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var msgID string
		f := &FileRef{}
		if err := rows.Scan(&f.ID, &msgID, &f.Name, &f.ContentType, &f.SizeBytes); err != nil {
			return fmt.Errorf("scan file: %w", err)
		}
		if m, ok := byID[msgID]; ok {
			m.Files = append(m.Files, f)
		}
	}
	return rows.Err()
}

// Hydrate attaches both reactions and files to the given messages.
func (r *Repository) Hydrate(ctx context.Context, messages []*Message) error {
	if err := r.hydrateReactions(ctx, messages); err != nil {
		return err
	}
	return r.hydrateFiles(ctx, messages)
}

func (r *Repository) GetByID(ctx context.Context, id string) (*Message, error) {
	m := &Message{}
	err := r.pool.QueryRow(ctx,
		`SELECT `+messageColumns+` FROM messages WHERE id = $1`, id,
	).Scan(scanArgs(m)...)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get message: %w", err)
	}
	return m, nil
}

func (r *Repository) ListByChannel(ctx context.Context, channelID string, before time.Time, limit int) ([]*Message, error) {
	query := `SELECT ` + messageColumns + `
		 FROM messages
		 WHERE channel_id = $1 AND parent_id IS NULL AND is_deleted = FALSE AND is_scheduled = FALSE`

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
		`SELECT `+messageColumns+`
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

// ListPinned returns the non-deleted pinned messages of a channel.
func (r *Repository) ListPinned(ctx context.Context, channelID string) ([]*Message, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+messageColumns+`
		 FROM messages
		 WHERE channel_id = $1 AND is_pinned = TRUE AND is_deleted = FALSE
		 ORDER BY pinned_at DESC`, channelID,
	)
	if err != nil {
		return nil, fmt.Errorf("list pinned: %w", err)
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

func (r *Repository) SetPinned(ctx context.Context, id, pinnedBy string, pinned bool) error {
	if pinned {
		_, err := r.pool.Exec(ctx,
			`UPDATE messages SET is_pinned = TRUE, pinned_by = $2, pinned_at = NOW() WHERE id = $1`, id, pinnedBy)
		return err
	}
	_, err := r.pool.Exec(ctx,
		`UPDATE messages SET is_pinned = FALSE, pinned_by = NULL, pinned_at = NULL WHERE id = $1`, id)
	return err
}

// Reactions

// AddReaction inserts a reaction, returning true if a new row was created
// (false if the user had already reacted with the same emoji).
func (r *Repository) AddReaction(ctx context.Context, reaction *Reaction) (bool, error) {
	tag, err := r.pool.Exec(ctx,
		`INSERT INTO reactions (id, message_id, user_id, emoji) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (message_id, user_id, emoji) DO NOTHING`,
		reaction.ID, reaction.MessageID, reaction.UserID, reaction.Emoji,
	)
	if err != nil {
		return false, fmt.Errorf("add reaction: %w", err)
	}
	return tag.RowsAffected() > 0, nil
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

// hydrateReactions attaches reactions to the given messages with a single query
// (avoids N+1). Messages with no reactions get an empty slice.
func (r *Repository) hydrateReactions(ctx context.Context, messages []*Message) error {
	if len(messages) == 0 {
		return nil
	}
	ids := make([]string, len(messages))
	byID := make(map[string]*Message, len(messages))
	for i, m := range messages {
		ids[i] = m.ID
		byID[m.ID] = m
	}

	rows, err := r.pool.Query(ctx,
		`SELECT id, message_id, user_id, emoji, created_at FROM reactions WHERE message_id = ANY($1) ORDER BY created_at`, ids)
	if err != nil {
		return fmt.Errorf("hydrate reactions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		re := &Reaction{}
		if err := rows.Scan(&re.ID, &re.MessageID, &re.UserID, &re.Emoji, &re.CreatedAt); err != nil {
			return fmt.Errorf("scan reaction: %w", err)
		}
		if m, ok := byID[re.MessageID]; ok {
			m.Reactions = append(m.Reactions, re)
		}
	}
	return rows.Err()
}

// HydrateReactions is the exported wrapper used by handlers.
func (r *Repository) HydrateReactions(ctx context.Context, messages []*Message) error {
	return r.hydrateReactions(ctx, messages)
}

func scanArgs(m *Message) []any {
	return []any{
		&m.ID, &m.ChannelID, &m.UserID, &m.ParentID, &m.Content, &m.ContentType,
		&m.IsEdited, &m.IsDeleted, &m.ReplyCount, &m.IsPinned, &m.PinnedBy, &m.PinnedAt,
		&m.ScheduledAt, &m.IsScheduled, &m.CreatedAt, &m.UpdatedAt,
	}
}

func (r *Repository) scanMessages(rows pgx.Rows) ([]*Message, error) {
	var messages []*Message
	for rows.Next() {
		m := &Message{}
		if err := rows.Scan(scanArgs(m)...); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}
