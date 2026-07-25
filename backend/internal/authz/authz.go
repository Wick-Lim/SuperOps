// Package authz is the canonical source of membership and role decisions.
//
// Before this package the same `SELECT EXISTS(... workspace_members ...)` was
// copy-pasted into file, emoji, webhook, presence, admin and channel handlers
// with subtly different predicates, and several endpoints simply forgot it —
// which is how a caller could browse, join and post in a workspace they had no
// membership in, and full-text search another tenant's private channels.
//
// Rules of use:
//   - Authentication stays in middleware; authorization stays in the handler.
//     Call a Checker method as the first statement of the handler body.
//   - Methods return (bool, error). Treat err as 500 and !ok as 403/404 —
//     never collapse the two, or a database outage becomes an authorization
//     bypass (or a permanent lockout).
//   - Never authorize a message/file/webhook against an id taken from the URL
//     when the resource itself names its own parent. Resolve the parent from
//     the row and check that.
package authz

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Workspace roles, ordered by privilege. Mirrors the CHECK constraint on
// workspace_members.role (migrations/002).
const (
	RoleOwner  = "owner"
	RoleAdmin  = "admin"
	RoleMember = "member"
	RoleGuest  = "guest"
)

// Channel roles. Deliberately a smaller vocabulary than workspace roles —
// mirrors the CHECK constraint on channel_members.role (migrations/003).
const (
	ChannelRoleAdmin  = "admin"
	ChannelRoleMember = "member"
)

// Checker answers authorization questions against Postgres. It holds no
// per-request state and is safe for concurrent use.
type Checker struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Checker { return &Checker{pool: pool} }

// ---------------------------------------------------------------------------
// Workspace
// ---------------------------------------------------------------------------

// WorkspaceRole returns the caller's role in a workspace, or "" if they are not
// a member. A non-member is not an error.
func (c *Checker) WorkspaceRole(ctx context.Context, workspaceID, userID string) (string, error) {
	if workspaceID == "" || userID == "" {
		return "", nil
	}
	var role string
	err := c.pool.QueryRow(ctx,
		`SELECT role FROM workspace_members WHERE workspace_id = $1 AND user_id = $2`,
		workspaceID, userID,
	).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get workspace role: %w", err)
	}
	return role, nil
}

func (c *Checker) IsWorkspaceMember(ctx context.Context, workspaceID, userID string) (bool, error) {
	role, err := c.WorkspaceRole(ctx, workspaceID, userID)
	return role != "", err
}

// IsWorkspaceAdmin reports whether the caller is owner or admin of the given
// workspace. This is the gate for destructive or membership-changing actions.
func (c *Checker) IsWorkspaceAdmin(ctx context.Context, workspaceID, userID string) (bool, error) {
	role, err := c.WorkspaceRole(ctx, workspaceID, userID)
	if err != nil {
		return false, err
	}
	return role == RoleOwner || role == RoleAdmin, nil
}

func (c *Checker) IsWorkspaceOwner(ctx context.Context, workspaceID, userID string) (bool, error) {
	role, err := c.WorkspaceRole(ctx, workspaceID, userID)
	if err != nil {
		return false, err
	}
	return role == RoleOwner, nil
}

// AdminWorkspaceIDs lists every workspace the caller administers. Admin
// endpoints must scope their queries to this set instead of running globally —
// "is an admin somewhere" is not "is an admin here".
func (c *Checker) AdminWorkspaceIDs(ctx context.Context, userID string) ([]string, error) {
	rows, err := c.pool.Query(ctx,
		`SELECT workspace_id FROM workspace_members
		  WHERE user_id = $1 AND role IN ('owner','admin')
		  ORDER BY workspace_id`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list admin workspaces: %w", err)
	}
	defer rows.Close()

	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan admin workspace: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SharesWorkspace reports whether two users have any workspace in common. An
// admin acting on another user must be able to prove at least this much, or
// /api/v1/admin/* becomes instance-wide control for anyone who created a
// throwaway workspace.
func (c *Checker) SharesWorkspace(ctx context.Context, actorID, targetID string) (bool, error) {
	var ok bool
	err := c.pool.QueryRow(ctx,
		`SELECT EXISTS(
		    SELECT 1
		      FROM workspace_members a
		      JOIN workspace_members b ON b.workspace_id = a.workspace_id
		     WHERE a.user_id = $1 AND a.role IN ('owner','admin') AND b.user_id = $2
		 )`,
		actorID, targetID,
	).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check shared workspace: %w", err)
	}
	return ok, nil
}

// ---------------------------------------------------------------------------
// Channel
// ---------------------------------------------------------------------------

// ChannelInfo is the minimum a handler needs to authorize against a channel
// resolved by id, without re-querying.
type ChannelInfo struct {
	ID          string
	WorkspaceID string
	Type        string
	IsArchived  bool
}

// Channel loads the authorization-relevant fields of a channel. Returns
// (nil, nil) when the channel does not exist, matching the repository
// convention used throughout the codebase.
func (c *Checker) Channel(ctx context.Context, channelID string) (*ChannelInfo, error) {
	var info ChannelInfo
	err := c.pool.QueryRow(ctx,
		`SELECT id, workspace_id, type::text, is_archived FROM channels WHERE id = $1`,
		channelID,
	).Scan(&info.ID, &info.WorkspaceID, &info.Type, &info.IsArchived)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get channel: %w", err)
	}
	return &info, nil
}

// ChannelRole returns the caller's role in a channel, or "" if not a member.
func (c *Checker) ChannelRole(ctx context.Context, channelID, userID string) (string, error) {
	if channelID == "" || userID == "" {
		return "", nil
	}
	var role string
	err := c.pool.QueryRow(ctx,
		`SELECT role FROM channel_members WHERE channel_id = $1 AND user_id = $2`,
		channelID, userID,
	).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get channel role: %w", err)
	}
	return role, nil
}

func (c *Checker) IsChannelMember(ctx context.Context, channelID, userID string) (bool, error) {
	role, err := c.ChannelRole(ctx, channelID, userID)
	return role != "", err
}

func (c *Checker) IsChannelAdmin(ctx context.Context, channelID, userID string) (bool, error) {
	role, err := c.ChannelRole(ctx, channelID, userID)
	if err != nil {
		return false, err
	}
	return role == ChannelRoleAdmin, nil
}

// CanReadChannel reports whether the caller may read a channel's contents:
// membership always suffices, and a public channel is readable by any member
// of its workspace. Crucially, a public channel is NOT readable by an
// authenticated stranger — that gap is what allowed cross-tenant browsing.
func (c *Checker) CanReadChannel(ctx context.Context, ch *ChannelInfo, userID string) (bool, error) {
	if ch == nil {
		return false, nil
	}
	member, err := c.IsChannelMember(ctx, ch.ID, userID)
	if err != nil || member {
		return member, err
	}
	if ch.Type != "public" {
		return false, nil
	}
	return c.IsWorkspaceMember(ctx, ch.WorkspaceID, userID)
}

// MessageChannel resolves the channel a message belongs to. Authorization for
// anything addressed by message id must go through this rather than trusting
// the channel id in the URL — otherwise a member of any channel can react to,
// pin, or unpin a message in a private channel of another workspace.
func (c *Checker) MessageChannel(ctx context.Context, messageID string) (*ChannelInfo, error) {
	var info ChannelInfo
	err := c.pool.QueryRow(ctx,
		`SELECT ch.id, ch.workspace_id, ch.type::text, ch.is_archived
		   FROM messages m JOIN channels ch ON ch.id = m.channel_id
		  WHERE m.id = $1`,
		messageID,
	).Scan(&info.ID, &info.WorkspaceID, &info.Type, &info.IsArchived)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get message channel: %w", err)
	}
	return &info, nil
}

// ReadableChannelIDs returns every channel in a workspace whose contents the
// caller may read. Search uses this to constrain the Meilisearch filter; a
// non-member gets an empty slice, which must be rendered as "no results"
// rather than "no filter".
func (c *Checker) ReadableChannelIDs(ctx context.Context, workspaceID, userID string) ([]string, error) {
	member, err := c.IsWorkspaceMember(ctx, workspaceID, userID)
	if err != nil {
		return nil, err
	}
	if !member {
		return []string{}, nil
	}

	rows, err := c.pool.Query(ctx,
		`SELECT ch.id
		   FROM channels ch
		  WHERE ch.workspace_id = $1
		    AND (ch.type = 'public'
		         OR EXISTS (SELECT 1 FROM channel_members cm
		                     WHERE cm.channel_id = ch.id AND cm.user_id = $2))`,
		workspaceID, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list readable channels: %w", err)
	}
	defer rows.Close()

	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan readable channel: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ---------------------------------------------------------------------------
// Blocks
// ---------------------------------------------------------------------------

// IsBlocked reports whether either user has blocked the other. Blocking is
// symmetric in effect: neither party should be able to route content to the
// other, regardless of who initiated the block.
func (c *Checker) IsBlocked(ctx context.Context, a, b string) (bool, error) {
	if a == "" || b == "" || a == b {
		return false, nil
	}
	var blocked bool
	err := c.pool.QueryRow(ctx,
		`SELECT EXISTS(
		    SELECT 1 FROM user_blocks
		     WHERE (blocker_id = $1 AND blocked_id = $2)
		        OR (blocker_id = $2 AND blocked_id = $1)
		 )`,
		a, b,
	).Scan(&blocked)
	if err != nil {
		return false, fmt.Errorf("check block: %w", err)
	}
	return blocked, nil
}

// BlockedUserIDs returns everyone the caller has blocked or been blocked by.
// Notification fan-out filters against this set in one query instead of N.
func (c *Checker) BlockedUserIDs(ctx context.Context, userID string) (map[string]bool, error) {
	rows, err := c.pool.Query(ctx,
		`SELECT blocked_id FROM user_blocks WHERE blocker_id = $1
		 UNION
		 SELECT blocker_id FROM user_blocks WHERE blocked_id = $1`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list blocks: %w", err)
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan block: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}
