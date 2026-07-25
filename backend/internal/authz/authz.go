// Package authz is the canonical source of authorization decisions.
//
// Before this package the same `SELECT EXISTS(... workspace_members ...)` was
// copy-pasted into file, emoji, webhook, presence, admin and channel handlers
// with subtly different predicates, and several endpoints simply forgot it —
// which is how a caller could browse, join and post in a workspace they had no
// membership in, and full-text search another tenant's private channels.
//
// The question a handler asks is "may this SUBJECT do this much to this
// OBJECT": Checker.Can, with an ordered capability
// (admin > share > write > comment > read). checker.go is that model;
// docs/plans/00-permissions.md is why it is shaped that way. The fifteen
// membership methods that predated it are gone — step 5 of that plan — and
// what remains here is the layer they were built on: the role and membership
// lookups that Capability composes, plus the domain predicates (SSO
// enforcement, blocks) that were never object-shaped at all.
//
// Rules of use:
//   - Authentication stays in middleware; authorization stays in the handler.
//     Call Can as the first statement of the handler body.
//   - err is a 500 and !ok is a 403/404 — never collapse the two, or a database
//     outage becomes an authorization bypass (or a permanent lockout).
//   - Never authorize a message/file/webhook against an id taken from the URL
//     when the resource itself names its own parent. Ask about the object
//     itself: a file is an object, and Can resolves its own parentage.
//   - Do not call Can in a loop. A page of rows is KeysFor once, then
//     FilterReadable — the N+1 this whole design exists to prevent reads as
//     correct, which is why the batch call is the one with the convenient
//     signature.
package authz

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/audit"
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
//
// checker.go carries the model handlers use — Capability, Can, KeysFor,
// FilterReadable, Grant, Revoke, Move. This file carries the lookups it is
// composed from (WorkspaceRole, ChannelRole, Channel, MessageChannel) and the
// predicates that are not object-shaped and never will be: SSO enforcement and
// user blocks.
type Checker struct {
	pool *pgxpool.Pool

	// revoker is nil unless the composition root wired a live surface. See
	// Revoker in checker.go: a revoked grant has to reach connections that were
	// authorized before it was revoked.
	revoker Revoker

	// auditor is nil unless the composition root wired one. See WithAuditor.
	auditor Auditor
}

// Auditor records authorization changes. *audit.Service is the implementation.
//
// The hooks live INSIDE Grant/Revoke/Move rather than at their call sites, and
// that is the whole point: a pillar cannot forget an audit hook it never had to
// write. Nine pillars each remembering to log a grant is nine chances to miss
// one, and the one that gets missed is the one an auditor asks about.
type Auditor interface {
	Try(ctx context.Context, e audit.Entry)
}

// WithAuditor attaches the audit trail to the authorization mutations. Without
// it the mutations behave identically and record nothing, which is correct for
// the operational tools and tests that construct a bare New(pool).
func WithAuditor(a Auditor) Option {
	return func(c *Checker) { c.auditor = a }
}

// audit records one authorization change, if an auditor is wired.
//
// Try, not Record: the mutation has already committed by the time this is
// called, so losing the trail entry must be visible in the logs (and in
// superops_audit_write_failures_total) without turning a successful revocation
// into a 500. These entries are Tier 1 — synchronous, never coalesced, chained —
// because "who was given access to what" is exactly the category that must not
// be lost, and it is low-volume enough to afford it.
func (c *Checker) audit(ctx context.Context, actor SubjectRef, action string, obj ObjectRef, subject *SubjectRef, meta map[string]interface{}) {
	if c.auditor == nil {
		return
	}
	actorID := ""
	if actor.Type == SubjectUser {
		if id, ok := canonicalUUID(actor.ID); ok {
			actorID = id
		}
	}
	if meta == nil {
		meta = map[string]interface{}{}
	}
	if subject != nil {
		meta["subject_type"] = string(subject.Type)
		meta["subject_id"] = subject.ID
	}
	workspaceID := ""
	if obj.Type == TypeWorkspace {
		workspaceID = obj.ID
	} else if ws, err := c.objectWorkspace(ctx, obj); err == nil {
		workspaceID = ws
	}

	c.auditor.Try(ctx, audit.Entry{
		WorkspaceID:  workspaceID,
		ActorID:      actorID,
		Action:       action,
		ResourceType: string(obj.Type),
		ResourceID:   obj.ID,
		Metadata:     meta,
	})
}

// objectWorkspace resolves the tenant an object belongs to, for the audit row.
// acl_object.workspace_id is authoritative for tenancy, so this is the same
// answer every authorization decision uses.
func (c *Checker) objectWorkspace(ctx context.Context, obj ObjectRef) (string, error) {
	var ws string
	err := c.pool.QueryRow(ctx,
		`SELECT workspace_id FROM acl_object WHERE object_type = $1 AND object_id = $2`,
		obj.Type, obj.ID).Scan(&ws)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return ws, err
}

// Option configures a Checker. Variadic so a plain New(pool) — which every test
// and every operational tool uses — keeps working.
type Option func(*Checker)

// WithRevoker attaches the live surface that Revoke and Move must reach.
// Without it the mutations still rewrite acl_key correctly and a connection
// authorized before the revocation keeps its access until the socket's periodic
// recheck — which is the backstop, not the mechanism, and it is minutes wide.
func WithRevoker(r Revoker) Option {
	return func(c *Checker) { c.revoker = r }
}

func New(pool *pgxpool.Pool, opts ...Option) *Checker {
	c := &Checker{pool: pool}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

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

// isWorkspaceMember is the membership predicate Capability's workspace arm and
// KeysFor are built from. It is unexported: a handler asking "is this person in
// this workspace" is asking whether they hold CapRead on the workspace object,
// and Can is the way to ask it — one call site style for every authorization
// decision in the product, so a reviewer never has to work out which layer a
// given check went through.
func (c *Checker) isWorkspaceMember(ctx context.Context, workspaceID, userID string) (bool, error) {
	role, err := c.WorkspaceRole(ctx, workspaceID, userID)
	return role != "", err
}

// IsWorkspaceOwner is the one role question with no object-level equivalent, and
// it stays a role check for a reason rather than by omission: the capability
// ladder is ordered, owner and admin both map to CapAdmin, and there is no rung
// between them. Sole ownership — deleting the workspace, transferring itself —
// is not "more admin than admin", it is a distinguished single row, so it is
// asked as one.
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

// isChannelMember is the membership predicate. Membership maps to CapWrite on
// the ladder, not CapRead: a member may post, while a non-member of a PUBLIC
// channel may only read. Handlers ask Can for one or the other — see
// message.Handler.requireMembership.
func (c *Checker) isChannelMember(ctx context.Context, channelID, userID string) (bool, error) {
	role, err := c.ChannelRole(ctx, channelID, userID)
	return role != "", err
}

// canReadChannel is the readability predicate Capability's channel arm is built
// from: membership always suffices, and a public channel is readable by any
// member of its workspace. Crucially, a public channel is NOT readable by an
// authenticated stranger — that gap is what allowed cross-tenant browsing.
func (c *Checker) canReadChannel(ctx context.Context, ch *ChannelInfo, userID string) (bool, error) {
	if ch == nil {
		return false, nil
	}
	member, err := c.isChannelMember(ctx, ch.ID, userID)
	if err != nil || member {
		return member, err
	}
	if ch.Type != "public" {
		return false, nil
	}
	return c.isWorkspaceMember(ctx, ch.WorkspaceID, userID)
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

// readableChannelIDs returns every channel in a workspace whose contents the
// caller may read. It is the channel arm of KeysFor and therefore, one step
// further out, the thing every search filter is built from — a non-member gets
// an empty slice, which the caller must render as "no results" rather than as
// "no filter".
//
// Unexported: the only correct consumer is KeysFor, which turns it into c- keys.
// A caller that wants "the channels I can read" as ids is almost always about
// to write an N+1, and one that wants to filter a page should be intersecting
// key sets instead.
func (c *Checker) readableChannelIDs(ctx context.Context, workspaceID, userID string) ([]string, error) {
	member, err := c.isWorkspaceMember(ctx, workspaceID, userID)
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
// Single sign-on enforcement
// ---------------------------------------------------------------------------

// PasswordLoginAllowed reports whether a user may still authenticate with a
// password.
//
// It lives here, and not in internal/sso, because it is a membership-and-role
// decision: a session is global while enforcement is per-workspace, so "may
// this person use a password" is answered by looking at every workspace they
// belong to and at their role in each. Exactly the class of query this package
// exists to keep out of handlers.
//
// The role clause is the lockout escape hatch. sso_providers.
// allow_owner_password_login (default true) keeps a workspace owner able to
// sign in with a password even under enforcement, so an IdP that breaks after
// the switch was flipped still leaves somebody able to reach the switch.
//
// Fails closed by construction: the caller treats an error as a 500, so a
// database fault refuses the login rather than allowing it.
func (c *Checker) PasswordLoginAllowed(ctx context.Context, userID string) (bool, error) {
	if userID == "" {
		return false, nil
	}
	var blocked bool
	err := c.pool.QueryRow(ctx,
		`SELECT EXISTS(
		    SELECT 1
		      FROM workspace_members wm
		      JOIN sso_providers p ON p.workspace_id = wm.workspace_id
		     WHERE wm.user_id = $1
		       AND p.enabled
		       AND p.enforced
		       AND NOT (p.allow_owner_password_login AND wm.role = 'owner')
		 )`,
		userID,
	).Scan(&blocked)
	if err != nil {
		return false, fmt.Errorf("check sso enforcement: %w", err)
	}
	return !blocked, nil
}

// SSOEnforced reports whether a workspace requires single sign-on.
//
// Used by invitation acceptance: joining an enforced workspace by password
// would mint a session that PasswordLoginAllowed then refuses to refresh, so
// the invite is refused up front instead — before it is consumed.
func (c *Checker) SSOEnforced(ctx context.Context, workspaceID string) (bool, error) {
	if workspaceID == "" {
		return false, nil
	}
	var enforced bool
	err := c.pool.QueryRow(ctx,
		`SELECT EXISTS(
		    SELECT 1 FROM sso_providers
		     WHERE workspace_id = $1 AND enabled AND enforced
		 )`,
		workspaceID,
	).Scan(&enforced)
	if err != nil {
		return false, fmt.Errorf("check workspace sso enforcement: %w", err)
	}
	return enforced, nil
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
