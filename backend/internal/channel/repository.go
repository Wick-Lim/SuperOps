package channel

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

func (r *Repository) Create(ctx context.Context, c *Channel) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO channels (id, workspace_id, name, slug, description, type, topic, creator_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		c.ID, c.WorkspaceID, c.Name, c.Slug, c.Description, c.Type, c.Topic, c.CreatorID,
	)
	if err != nil {
		return fmt.Errorf("create channel: %w", err)
	}
	return nil
}

func (r *Repository) GetByID(ctx context.Context, id string) (*Channel, error) {
	c := &Channel{}
	err := r.pool.QueryRow(ctx,
		`SELECT id, workspace_id, name, slug, description, type, topic, is_archived, creator_id, last_message_at, created_at, updated_at
		 FROM channels WHERE id = $1`, id,
	).Scan(&c.ID, &c.WorkspaceID, &c.Name, &c.Slug, &c.Description, &c.Type, &c.Topic, &c.IsArchived, &c.CreatorID, &c.LastMessageAt, &c.CreatedAt, &c.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get channel: %w", err)
	}
	return c, nil
}

func (r *Repository) ListByWorkspaceAndUser(ctx context.Context, workspaceID, userID string) ([]*Channel, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT c.id, c.workspace_id, c.name, c.slug, c.description, c.type, c.topic, c.is_archived, c.creator_id, c.last_message_at, c.created_at, c.updated_at
		 FROM channels c
		 JOIN channel_members cm ON c.id = cm.channel_id
		 WHERE c.workspace_id = $1 AND cm.user_id = $2 AND c.is_archived = FALSE
		 ORDER BY c.last_message_at DESC NULLS LAST`, workspaceID, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	defer rows.Close()

	return r.scanChannels(rows)
}

func (r *Repository) ListPublicByWorkspace(ctx context.Context, workspaceID string) ([]*Channel, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, workspace_id, name, slug, description, type, topic, is_archived, creator_id, last_message_at, created_at, updated_at
		 FROM channels
		 WHERE workspace_id = $1 AND type = 'public' AND is_archived = FALSE
		 ORDER BY name`, workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list public channels: %w", err)
	}
	defer rows.Close()

	return r.scanChannels(rows)
}

func (r *Repository) Update(ctx context.Context, c *Channel) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE channels SET name = $2, description = $3, topic = $4, is_archived = $5, updated_at = NOW()
		 WHERE id = $1`,
		c.ID, c.Name, c.Description, c.Topic, c.IsArchived,
	)
	if err != nil {
		return fmt.Errorf("update channel: %w", err)
	}
	return nil
}

func (r *Repository) UpdateLastMessage(ctx context.Context, channelID string, t time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE channels SET last_message_at = $2 WHERE id = $1`, channelID, t)
	if err != nil {
		return fmt.Errorf("update last message: %w", err)
	}
	return nil
}

// Members

func (r *Repository) AddMember(ctx context.Context, m *ChannelMember) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO channel_members (channel_id, user_id, role) VALUES ($1, $2, $3)
		 ON CONFLICT (channel_id, user_id) DO NOTHING`,
		m.ChannelID, m.UserID, m.Role,
	)
	if err != nil {
		return fmt.Errorf("add channel member: %w", err)
	}
	return nil
}

func (r *Repository) GetMember(ctx context.Context, channelID, userID string) (*ChannelMember, error) {
	m := &ChannelMember{}
	err := r.pool.QueryRow(ctx,
		`SELECT channel_id, user_id, role, last_read_at, muted, notification_pref, joined_at
		 FROM channel_members WHERE channel_id = $1 AND user_id = $2`,
		channelID, userID,
	).Scan(&m.ChannelID, &m.UserID, &m.Role, &m.LastReadAt, &m.Muted, &m.NotificationPref, &m.JoinedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get channel member: %w", err)
	}
	return m, nil
}

func (r *Repository) ListMembers(ctx context.Context, channelID string) ([]*ChannelMember, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT channel_id, user_id, role, last_read_at, muted, notification_pref, joined_at
		 FROM channel_members WHERE channel_id = $1 ORDER BY joined_at`, channelID,
	)
	if err != nil {
		return nil, fmt.Errorf("list channel members: %w", err)
	}
	defer rows.Close()

	var members []*ChannelMember
	for rows.Next() {
		m := &ChannelMember{}
		if err := rows.Scan(&m.ChannelID, &m.UserID, &m.Role, &m.LastReadAt, &m.Muted, &m.NotificationPref, &m.JoinedAt); err != nil {
			return nil, fmt.Errorf("scan channel member: %w", err)
		}
		members = append(members, m)
	}
	return members, nil
}

func (r *Repository) RemoveMember(ctx context.Context, channelID, userID string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM channel_members WHERE channel_id = $1 AND user_id = $2`, channelID, userID)
	if err != nil {
		return fmt.Errorf("remove channel member: %w", err)
	}
	return nil
}

func (r *Repository) UpdateReadAt(ctx context.Context, channelID, userID string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE channel_members SET last_read_at = NOW() WHERE channel_id = $1 AND user_id = $2`,
		channelID, userID,
	)
	if err != nil {
		return fmt.Errorf("update read at: %w", err)
	}
	return nil
}

func (r *Repository) scanChannels(rows pgx.Rows) ([]*Channel, error) {
	var channels []*Channel
	for rows.Next() {
		c := &Channel{}
		if err := rows.Scan(&c.ID, &c.WorkspaceID, &c.Name, &c.Slug, &c.Description, &c.Type, &c.Topic, &c.IsArchived, &c.CreatorID, &c.LastMessageAt, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan channel: %w", err)
		}
		channels = append(channels, c)
	}
	return channels, nil
}
