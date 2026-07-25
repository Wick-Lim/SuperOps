package channel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
)

// ErrSlugTaken reports a collision on UNIQUE (workspace_id, slug). Previously
// there was no check at all, so a duplicate slug was a 500.
var ErrSlugTaken = errors.New("channel slug already taken")

// ErrNotAMember reports that the (channel, user) row the caller asked to
// mutate does not exist. Callers map it to 404.
var ErrNotAMember = errors.New("not a channel member")

// ErrChannelGone reports that a channel disappeared between the authorization
// check and the write.
var ErrChannelGone = errors.New("channel not found")

// channelColumns is the column list every Channel scan uses. UnreadCount is
// not part of it — it is appended by the queries that compute it.
const channelColumns = `id, workspace_id, name, slug, description, type, topic, is_archived, creator_id, last_message_at, created_at, updated_at`

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// isUniqueViolation reports whether err is a Postgres unique-violation (23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// CreateWithOwner inserts a named channel and its creator's admin membership
// atomically. Doing this in two statements left channels with zero members on a
// partial failure: invisible in List and permanently un-updatable and
// un-archivable, since both require an admin channel_members row.
func (r *Repository) CreateWithOwner(ctx context.Context, c *Channel, ownerID string) error {
	return database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`INSERT INTO channels (id, workspace_id, name, slug, description, type, topic, creator_id)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			 RETURNING `+channelColumns,
			c.ID, c.WorkspaceID, c.Name, c.Slug, c.Description, c.Type, c.Topic, c.CreatorID,
		).Scan(&c.ID, &c.WorkspaceID, &c.Name, &c.Slug, &c.Description, &c.Type, &c.Topic, &c.IsArchived, &c.CreatorID, &c.LastMessageAt, &c.CreatedAt, &c.UpdatedAt)
		// The only unique constraint reachable here is UNIQUE (workspace_id, slug):
		// the id is a freshly minted uuid and dm_key is left NULL.
		if isUniqueViolation(err) {
			return ErrSlugTaken
		}
		if err != nil {
			return fmt.Errorf("create channel: %w", err)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO channel_members (channel_id, user_id, role) VALUES ($1, $2, $3)`,
			c.ID, ownerID, RoleAdmin,
		); err != nil {
			return fmt.Errorf("add channel owner: %w", err)
		}
		return nil
	})
}

// EnsureDM creates a DM channel with its full participant roster in one
// transaction, and is idempotent for 1:1 DMs.
//
// dmKey must be non-nil for a 1:1 DM and nil for a group DM (group DMs are
// deliberately never deduped). For 1:1 DMs the partial unique index
// idx_channels_dm_key turns the previous read-then-write race — where two
// concurrent requests both missed the lookup and both inserted — into an
// ON CONFLICT that adopts the winner's channel.
//
// Participants are inserted as channel admins: every participant of a DM is a
// peer, and the Update/Archive gates require the admin role, so creating them
// as plain members made DMs permanently un-renameable and un-archivable.
//
// The returned bool reports whether this call created the channel.
func (r *Repository) EnsureDM(ctx context.Context, c *Channel, dmKey *string, participants []string) (bool, error) {
	created := false
	err := database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		insert := `INSERT INTO channels (id, workspace_id, type, creator_id, dm_key)
			 VALUES ($1, $2, $3, $4, $5)
			 RETURNING ` + channelColumns
		if dmKey != nil {
			insert = `INSERT INTO channels (id, workspace_id, type, creator_id, dm_key)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (workspace_id, dm_key) WHERE dm_key IS NOT NULL DO NOTHING
			 RETURNING ` + channelColumns
		}

		err := tx.QueryRow(ctx, insert,
			c.ID, c.WorkspaceID, c.Type, c.CreatorID, dmKey,
		).Scan(&c.ID, &c.WorkspaceID, &c.Name, &c.Slug, &c.Description, &c.Type, &c.Topic, &c.IsArchived, &c.CreatorID, &c.LastMessageAt, &c.CreatedAt, &c.UpdatedAt)

		// DO NOTHING suppressed the insert: somebody else already owns this
		// pair. Adopt their channel instead of minting a duplicate.
		if dmKey != nil && errors.Is(err, pgx.ErrNoRows) {
			return tx.QueryRow(ctx,
				`SELECT `+channelColumns+` FROM channels WHERE workspace_id = $1 AND dm_key = $2`,
				c.WorkspaceID, *dmKey,
			).Scan(&c.ID, &c.WorkspaceID, &c.Name, &c.Slug, &c.Description, &c.Type, &c.Topic, &c.IsArchived, &c.CreatorID, &c.LastMessageAt, &c.CreatedAt, &c.UpdatedAt)
		}
		if err != nil {
			return fmt.Errorf("create dm channel: %w", err)
		}

		for _, uid := range participants {
			if _, err := tx.Exec(ctx,
				`INSERT INTO channel_members (channel_id, user_id, role) VALUES ($1, $2, $3)
				 ON CONFLICT (channel_id, user_id) DO NOTHING`,
				c.ID, uid, RoleAdmin,
			); err != nil {
				return fmt.Errorf("add dm participant: %w", err)
			}
		}
		created = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

func (r *Repository) GetByID(ctx context.Context, id string) (*Channel, error) {
	c := &Channel{}
	err := r.pool.QueryRow(ctx,
		`SELECT `+channelColumns+` FROM channels WHERE id = $1`, id,
	).Scan(&c.ID, &c.WorkspaceID, &c.Name, &c.Slug, &c.Description, &c.Type, &c.Topic, &c.IsArchived, &c.CreatorID, &c.LastMessageAt, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get channel: %w", err)
	}
	return c, nil
}

// ListByWorkspaceAndUser returns the caller's channels together with a per
// channel unread count: non-deleted, non-scheduled top-level messages posted by
// somebody else after the caller's last_read_at. The predicate matches
// idx_messages_channel_live and the channel timeline the client renders, so
// thread replies do not inflate the badge.
func (r *Repository) ListByWorkspaceAndUser(ctx context.Context, workspaceID, userID string) ([]*Channel, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT c.id, c.workspace_id, c.name, c.slug, c.description, c.type, c.topic, c.is_archived,
		        c.creator_id, c.last_message_at, c.created_at, c.updated_at,
		        (SELECT COUNT(*) FROM messages m
		          WHERE m.channel_id = c.id
		            AND m.parent_id IS NULL
		            AND m.is_deleted = FALSE
		            AND m.is_scheduled = FALSE
		            AND m.user_id <> $2
		            AND m.created_at > cm.last_read_at) AS unread_count
		 FROM channels c
		 JOIN channel_members cm ON c.id = cm.channel_id AND cm.user_id = $2
		 WHERE c.workspace_id = $1 AND c.is_archived = FALSE
		 ORDER BY c.last_message_at DESC NULLS LAST, c.id`, workspaceID, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	defer rows.Close()

	channels := []*Channel{}
	for rows.Next() {
		c := &Channel{}
		if err := rows.Scan(&c.ID, &c.WorkspaceID, &c.Name, &c.Slug, &c.Description, &c.Type, &c.Topic, &c.IsArchived, &c.CreatorID, &c.LastMessageAt, &c.CreatedAt, &c.UpdatedAt, &c.UnreadCount); err != nil {
			return nil, fmt.Errorf("scan channel: %w", err)
		}
		channels = append(channels, c)
	}
	return channels, rows.Err()
}

func (r *Repository) ListPublicByWorkspace(ctx context.Context, workspaceID string) ([]*Channel, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+channelColumns+`
		 FROM channels
		 WHERE workspace_id = $1 AND type = 'public' AND is_archived = FALSE
		 ORDER BY name`, workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list public channels: %w", err)
	}
	defer rows.Close()

	return scanChannels(rows)
}

// UnreadCount counts what ListByWorkspaceAndUser computes, for a single
// channel. It returns ErrNotAMember when the caller has no membership row,
// since "unread" is only defined relative to one.
func (r *Repository) UnreadCount(ctx context.Context, channelID, userID string) (*Unread, error) {
	u := &Unread{ChannelID: channelID}
	err := r.pool.QueryRow(ctx,
		`SELECT cm.last_read_at,
		        (SELECT COUNT(*) FROM messages m
		          WHERE m.channel_id = cm.channel_id
		            AND m.parent_id IS NULL
		            AND m.is_deleted = FALSE
		            AND m.is_scheduled = FALSE
		            AND m.user_id <> cm.user_id
		            AND m.created_at > cm.last_read_at)
		 FROM channel_members cm
		 WHERE cm.channel_id = $1 AND cm.user_id = $2`,
		channelID, userID,
	).Scan(&u.LastReadAt, &u.Count)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotAMember
	}
	if err != nil {
		return nil, fmt.Errorf("count unread: %w", err)
	}
	return u, nil
}

// Update writes the mutable presentation fields. updated_at is maintained by
// the BEFORE UPDATE trigger added in migration 009.
func (r *Repository) Update(ctx context.Context, c *Channel) error {
	err := r.pool.QueryRow(ctx,
		`UPDATE channels SET name = $2, description = $3, topic = $4
		 WHERE id = $1
		 RETURNING `+channelColumns,
		c.ID, c.Name, c.Description, c.Topic,
	).Scan(&c.ID, &c.WorkspaceID, &c.Name, &c.Slug, &c.Description, &c.Type, &c.Topic, &c.IsArchived, &c.CreatorID, &c.LastMessageAt, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrChannelGone
	}
	if err != nil {
		return fmt.Errorf("update channel: %w", err)
	}
	return nil
}

// SetArchived flips is_archived and returns the refreshed row. A zero-row
// result means the channel disappeared between the authorization check and the
// write.
func (r *Repository) SetArchived(ctx context.Context, channelID string, archived bool) (*Channel, error) {
	c := &Channel{}
	err := r.pool.QueryRow(ctx,
		`UPDATE channels SET is_archived = $2 WHERE id = $1 RETURNING `+channelColumns,
		channelID, archived,
	).Scan(&c.ID, &c.WorkspaceID, &c.Name, &c.Slug, &c.Description, &c.Type, &c.Topic, &c.IsArchived, &c.CreatorID, &c.LastMessageAt, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("set archived: %w", err)
	}
	return c, nil
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
	if errors.Is(err, pgx.ErrNoRows) {
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
		 FROM channel_members WHERE channel_id = $1 ORDER BY joined_at, user_id`, channelID,
	)
	if err != nil {
		return nil, fmt.Errorf("list channel members: %w", err)
	}
	defer rows.Close()

	members := []*ChannelMember{}
	for rows.Next() {
		m := &ChannelMember{}
		if err := rows.Scan(&m.ChannelID, &m.UserID, &m.Role, &m.LastReadAt, &m.Muted, &m.NotificationPref, &m.JoinedAt); err != nil {
			return nil, fmt.Errorf("scan channel member: %w", err)
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

// MemberCounts returns the channel's total member count and its admin count.
// Leave and RemoveMember use it to refuse the departure of the last admin,
// which would otherwise leave the channel permanently unmanageable.
func (r *Repository) MemberCounts(ctx context.Context, channelID string) (total, admins int, err error) {
	err = r.pool.QueryRow(ctx,
		`SELECT COUNT(*), COUNT(*) FILTER (WHERE role = 'admin')
		 FROM channel_members WHERE channel_id = $1`, channelID,
	).Scan(&total, &admins)
	if err != nil {
		return 0, 0, fmt.Errorf("count channel members: %w", err)
	}
	return total, admins, nil
}

// RemoveMember deletes a membership row. It returns ErrNotAMember when there
// was nothing to delete, so callers answer 404 rather than a misleading 200.
func (r *Repository) RemoveMember(ctx context.Context, channelID, userID string) error {
	tag, err := r.pool.Exec(ctx,
		`DELETE FROM channel_members WHERE channel_id = $1 AND user_id = $2`, channelID, userID)
	if err != nil {
		return fmt.Errorf("remove channel member: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotAMember
	}
	return nil
}

// UpdateReadAt advances the caller's read marker to at. The marker is monotonic
// (GREATEST) and clamped to NOW(), so neither an out-of-order request nor a
// client clock skewed into the future can mark unsent messages read.
func (r *Repository) UpdateReadAt(ctx context.Context, channelID, userID string, at time.Time) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE channel_members
		    SET last_read_at = GREATEST(last_read_at, LEAST($3::timestamptz, NOW()))
		  WHERE channel_id = $1 AND user_id = $2`,
		channelID, userID, at,
	)
	if err != nil {
		return fmt.Errorf("update read at: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotAMember
	}
	return nil
}

// MessageTimestamp resolves a message's created_at, scoped to the channel it is
// claimed to belong to. MarkRead uses it so a client can say "I have read up to
// this message" without trusting a client-supplied timestamp.
func (r *Repository) MessageTimestamp(ctx context.Context, channelID, messageID string) (*time.Time, error) {
	var t time.Time
	err := r.pool.QueryRow(ctx,
		`SELECT created_at FROM messages WHERE id = $1 AND channel_id = $2`, messageID, channelID,
	).Scan(&t)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get message timestamp: %w", err)
	}
	return &t, nil
}

// UpdateMemberPrefs applies the non-nil notification preferences and returns
// the refreshed membership row.
func (r *Repository) UpdateMemberPrefs(ctx context.Context, channelID, userID string, muted *bool, pref *string) (*ChannelMember, error) {
	m := &ChannelMember{}
	err := r.pool.QueryRow(ctx,
		`UPDATE channel_members
		    SET muted = COALESCE($3, muted),
		        notification_pref = COALESCE($4, notification_pref)
		  WHERE channel_id = $1 AND user_id = $2
		  RETURNING channel_id, user_id, role, last_read_at, muted, notification_pref, joined_at`,
		channelID, userID, muted, pref,
	).Scan(&m.ChannelID, &m.UserID, &m.Role, &m.LastReadAt, &m.Muted, &m.NotificationPref, &m.JoinedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotAMember
	}
	if err != nil {
		return nil, fmt.Errorf("update channel member prefs: %w", err)
	}
	return m, nil
}

func scanChannels(rows pgx.Rows) ([]*Channel, error) {
	channels := []*Channel{}
	for rows.Next() {
		c := &Channel{}
		if err := rows.Scan(&c.ID, &c.WorkspaceID, &c.Name, &c.Slug, &c.Description, &c.Type, &c.Topic, &c.IsArchived, &c.CreatorID, &c.LastMessageAt, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan channel: %w", err)
		}
		channels = append(channels, c)
	}
	return channels, rows.Err()
}
