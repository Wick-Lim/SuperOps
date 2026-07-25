package admin

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/audit"
	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/crypto"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

const (
	invitationTTL   = 72 * time.Hour
	invitationBytes = 24
	uniqueViolation = "23505"
	listLimit       = 100
)

// invitableRoles mirrors the CHECK on invitations.role. 'owner' is deliberately
// absent: ownership transfer is not an invitation.
var invitableRoles = map[string]bool{
	authz.RoleAdmin:  true,
	authz.RoleMember: true,
	authz.RoleGuest:  true,
}

type Handler struct {
	pool  *pgxpool.Pool
	audit *audit.Service
	authz *authz.Checker
	mail  MailDeps
}

// NewHandler wires the admin endpoints. A zero MailDeps disables outbound
// email: invitations are still created and still return a usable URL, they are
// simply not delivered.
func NewHandler(pool *pgxpool.Pool, auditSvc *audit.Service, az *authz.Checker, mailDeps MailDeps) *Handler {
	return &Handler{pool: pool, audit: auditSvc, authz: az, mail: mailDeps}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("GET /api/v1/admin/users", authMw(http.HandlerFunc(h.ListUsers)))
	mux.Handle("PATCH /api/v1/admin/users/{user_id}", authMw(http.HandlerFunc(h.UpdateUser)))
	mux.Handle("GET /api/v1/admin/stats", authMw(http.HandlerFunc(h.Stats)))
	mux.Handle("GET /api/v1/admin/audit-logs", authMw(http.HandlerFunc(h.AuditLogs)))
	mux.Handle("POST /api/v1/admin/invitations", authMw(http.HandlerFunc(h.CreateInvitation)))
	mux.Handle("GET /api/v1/admin/invitations", authMw(http.HandlerFunc(h.ListInvitations)))
}

// RegisterMailRoutes is separate from RegisterRoutes because this one route
// needs a stricter middleware chain than the rest of /admin/*: it triggers
// outbound mail, so the composition root wraps it in its own rate limiter.
func (h *Handler) RegisterMailRoutes(mux *http.ServeMux, mw func(http.Handler) http.Handler) {
	mux.Handle("POST /api/v1/admin/mail/test", mw(http.HandlerFunc(h.SendTestMail)))
}

// scope resolves the workspaces the caller administers. Every /admin/* query is
// constrained to it: "is an owner/admin of some workspace" — which is all the
// route middleware proves — is not authority over the whole instance.
func (h *Handler) scope(w http.ResponseWriter, r *http.Request) ([]string, string, bool) {
	actor := authctx.UserID(r.Context())
	ids, err := h.authz.AdminWorkspaceIDs(r.Context(), actor)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return nil, "", false
	}
	if len(ids) == 0 {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "workspace admin privileges required")
		return nil, "", false
	}
	return ids, actor, true
}

// --- invitations -------------------------------------------------------------

func (h *Handler) CreateInvitation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := authctx.UserID(ctx)

	var input struct {
		WorkspaceID string `json:"workspace_id"`
		Email       string `json:"email"`
		Role        string `json:"role"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	email := strings.ToLower(strings.TrimSpace(input.Email))
	if email == "" || !strings.Contains(email, "@") {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "a valid email is required")
		return
	}
	// The target workspace is explicit. It used to be whichever row Postgres
	// happened to return from an unordered `LIMIT 1`, which could be a workspace
	// the caller is only a plain member of.
	if _, err := uuid.Parse(input.WorkspaceID); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "workspace_id is required")
		return
	}
	if input.Role == "" {
		input.Role = authz.RoleMember
	}
	if !invitableRoles[input.Role] {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "role must be admin, member or guest")
		return
	}

	admin, err := h.authz.IsWorkspaceAdmin(ctx, input.WorkspaceID, actor)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !admin {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "workspace admin privileges required")
		return
	}

	token, err := crypto.GenerateRandomToken(invitationBytes)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	expiresAt := time.Now().Add(invitationTTL)
	var inviteID string
	err = h.pool.QueryRow(ctx,
		`INSERT INTO invitations (email, workspace_id, role, token_hash, invited_by, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		email, input.WorkspaceID, input.Role, crypto.HashToken(token), actor, expiresAt,
	).Scan(&inviteID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			httputil.JSONError(w, http.StatusConflict, "CONFLICT", "a pending invitation for this email already exists")
			return
		}
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	if h.audit != nil {
		_ = h.audit.Log(ctx, input.WorkspaceID, actor, "invitation.created", "invitation", inviteID,
			map[string]interface{}{"email": email, "role": input.Role})
	}

	// Delivery is best effort and deliberately after the commit. The invitation
	// row is the source of truth: a mail queue that is down must not destroy an
	// invitation that is otherwise perfectly usable.
	acceptPath := "/invite/" + token
	queued := h.queueInvitationMail(ctx, invitationMail{
		InvitationID: inviteID,
		WorkspaceID:  input.WorkspaceID,
		InviterID:    actor,
		Email:        email,
		Role:         input.Role,
		AcceptPath:   acceptPath,
		ExpiresAt:    expiresAt,
	})

	// The token is shown exactly once: only its hash is stored. invite_url stays
	// in the response even when the mail was queued — an admin still needs it
	// when mail is disabled, when the address bounces, or when the recipient
	// never finds the message.
	httputil.JSON(w, http.StatusCreated, map[string]interface{}{
		"id":              inviteID,
		"invite_url":      acceptPath,
		"email":           email,
		"role":            input.Role,
		"workspace_id":    input.WorkspaceID,
		"expires_at":      expiresAt,
		"email_queued":    queued,
		"email_transport": h.mail.transportName(),
	})
}

func (h *Handler) ListInvitations(w http.ResponseWriter, r *http.Request) {
	workspaces, _, ok := h.scope(w, r)
	if !ok {
		return
	}

	// No token, ever: it is stored only as a hash, and returning it instance-wide
	// let an admin of any throwaway workspace redeem another org's pending invite.
	rows, err := h.pool.Query(r.Context(),
		`SELECT i.id, i.workspace_id, i.email, i.role, i.status, i.expires_at, i.created_at,
		        COALESCE(u.full_name, u.username)
		   FROM invitations i JOIN users u ON i.invited_by = u.id
		  WHERE i.workspace_id = ANY($1)
		  ORDER BY i.created_at DESC LIMIT $2`, workspaces, listLimit)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	defer rows.Close()

	invites := []map[string]interface{}{}
	for rows.Next() {
		var id, wsID, email, role, status, inviter string
		var expiresAt, createdAt time.Time
		if err := rows.Scan(&id, &wsID, &email, &role, &status, &expiresAt, &createdAt, &inviter); err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		invites = append(invites, map[string]interface{}{
			"id": id, "workspace_id": wsID, "email": email, "role": role,
			"status": status, "expires_at": expiresAt, "created_at": createdAt, "invited_by": inviter,
		})
	}
	if err := rows.Err(); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusOK, invites)
}

// --- users -------------------------------------------------------------------

func (h *Handler) ListUsers(w http.ResponseWriter, r *http.Request) {
	workspaces, _, ok := h.scope(w, r)
	if !ok {
		return
	}

	rows, err := h.pool.Query(r.Context(),
		`SELECT DISTINCT u.id, u.email, u.username, u.full_name, u.is_active, u.is_bot, u.created_at
		   FROM users u JOIN workspace_members wm ON wm.user_id = u.id
		  WHERE wm.workspace_id = ANY($1)
		  ORDER BY u.created_at DESC LIMIT $2`, workspaces, listLimit)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	defer rows.Close()

	users := []map[string]interface{}{}
	for rows.Next() {
		var id, email, username, fullName string
		var isActive, isBot bool
		var createdAt time.Time
		if err := rows.Scan(&id, &email, &username, &fullName, &isActive, &isBot, &createdAt); err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		users = append(users, map[string]interface{}{
			"id": id, "email": email, "username": username, "full_name": fullName,
			"is_active": isActive, "is_bot": isBot, "created_at": createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusOK, users)
}

func (h *Handler) UpdateUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := authctx.UserID(ctx)
	uid := r.PathValue("user_id")

	if _, err := uuid.Parse(uid); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "user_id must be a user id")
		return
	}

	var input struct {
		IsActive *bool   `json:"is_active"`
		Role     *string `json:"role"`
		// WorkspaceID scopes a role change. A role has no meaning outside a
		// workspace, and the caller must administer the one they name.
		WorkspaceID *string `json:"workspace_id"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if input.IsActive == nil && input.Role == nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "is_active or role is required")
		return
	}

	// The caller must administer a workspace the target belongs to. Without this
	// any workspace admin could deactivate any account on the instance.
	shares, err := h.authz.SharesWorkspace(ctx, actor, uid)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !shares {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "user not found")
		return
	}

	workspaces, err := h.authz.AdminWorkspaceIDs(ctx, actor)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	if input.IsActive != nil {
		if err := h.setActive(w, r, actor, uid, workspaces, *input.IsActive); err != nil {
			return // response already written
		}
	}

	if input.Role != nil {
		if err := h.setRole(w, r, actor, uid, input.WorkspaceID, *input.Role); err != nil {
			return // response already written
		}
	}

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "user updated"})
}

// errWritten signals that the failure response has already been produced.
var errWritten = errors.New("response written")

func (h *Handler) setActive(w http.ResponseWriter, r *http.Request, actor, uid string, workspaces []string, active bool) error {
	ctx := r.Context()

	if !active {
		if uid == actor {
			httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "cannot deactivate yourself")
			return errWritten
		}
		// is_active is an instance-wide flag, so deactivating the owner of a
		// workspace the caller does not administer is a cross-tenant takedown.
		var foreignOwner bool
		if err := h.pool.QueryRow(ctx,
			`SELECT EXISTS(
			    SELECT 1 FROM workspace_members
			     WHERE user_id = $1 AND role = 'owner' AND NOT (workspace_id = ANY($2))
			 )`, uid, workspaces).Scan(&foreignOwner); err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return errWritten
		}
		if foreignOwner {
			httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN",
				"cannot deactivate the owner of a workspace you do not administer")
			return errWritten
		}
	}

	// Deactivation must revoke sessions in the same transaction. Login checks
	// is_active but the refresh path did not, so a ban left the user rotating a
	// 30-day refresh token indefinitely.
	err := database.WithTx(ctx, h.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE users SET is_active = $2 WHERE id = $1`, uid, active)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		if !active {
			if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, uid); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "user not found")
		return errWritten
	}
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return errWritten
	}

	if h.audit != nil {
		wsID := ""
		if len(workspaces) > 0 {
			wsID = workspaces[0]
		}
		_ = h.audit.Log(ctx, wsID, actor, "user.updated", "user", uid,
			map[string]interface{}{"is_active": active})
	}
	return nil
}

func (h *Handler) setRole(w http.ResponseWriter, r *http.Request, actor, uid string, workspaceID *string, role string) error {
	ctx := r.Context()

	if workspaceID == nil || *workspaceID == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "workspace_id is required to change a role")
		return errWritten
	}
	if !invitableRoles[role] {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "role must be admin, member or guest")
		return errWritten
	}

	admin, err := h.authz.IsWorkspaceAdmin(ctx, *workspaceID, actor)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return errWritten
	}
	if !admin {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "workspace admin privileges required")
		return errWritten
	}

	// Guard predicate rather than SELECT-then-UPDATE: the owner's role is not
	// demotable here, and a non-member simply matches no row.
	tag, err := h.pool.Exec(ctx,
		`UPDATE workspace_members SET role = $3
		  WHERE workspace_id = $1 AND user_id = $2 AND role <> 'owner'`,
		*workspaceID, uid, role)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return errWritten
	}
	if tag.RowsAffected() == 0 {
		httputil.JSONError(w, http.StatusConflict, "CONFLICT",
			"user is not a non-owner member of this workspace")
		return errWritten
	}

	if h.audit != nil {
		_ = h.audit.Log(ctx, *workspaceID, actor, "user.role_changed", "user", uid,
			map[string]interface{}{"role": role})
	}
	return nil
}

// --- stats & audit -----------------------------------------------------------

func (h *Handler) Stats(w http.ResponseWriter, r *http.Request) {
	workspaces, _, ok := h.scope(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	var userCount, channelCount, messageCount int

	// Every Scan error used to be discarded, so a failing query silently
	// reported 0 rather than an error.
	if err := h.pool.QueryRow(ctx,
		`SELECT COUNT(DISTINCT user_id) FROM workspace_members WHERE workspace_id = ANY($1)`,
		workspaces).Scan(&userCount); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if err := h.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM channels WHERE workspace_id = ANY($1)`,
		workspaces).Scan(&channelCount); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if err := h.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM messages m JOIN channels c ON c.id = m.channel_id
		  WHERE c.workspace_id = ANY($1)`,
		workspaces).Scan(&messageCount); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusOK, map[string]int{
		"users": userCount, "workspaces": len(workspaces),
		"channels": channelCount, "messages": messageCount,
	})
}

func (h *Handler) AuditLogs(w http.ResponseWriter, r *http.Request) {
	params, err := httputil.ParsePagination(r)
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	workspaces, _, ok := h.scope(w, r)
	if !ok {
		return
	}

	const cols = `SELECT id, workspace_id, actor_id, action, resource_type, resource_id,
	                     metadata::text, COALESCE(host(ip_address),''), created_at
	                FROM audit_logs
	               WHERE workspace_id = ANY($1)`

	var rows pgx.Rows
	if params.Cursor.IsZero() {
		rows, err = h.pool.Query(r.Context(),
			cols+` ORDER BY created_at DESC, id DESC LIMIT $2`,
			workspaces, params.Limit+1)
	} else {
		rows, err = h.pool.Query(r.Context(),
			cols+` AND (created_at, id) < ($2, $3) ORDER BY created_at DESC, id DESC LIMIT $4`,
			workspaces, params.Cursor.CreatedAt, params.Cursor.ID, params.Limit+1)
	}
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	defer rows.Close()

	type entry struct {
		id        string
		createdAt time.Time
		body      map[string]interface{}
	}
	entries := []entry{}
	for rows.Next() {
		var id, action, resourceType, metadata, ip string
		var wsID, actorID, resourceID *string
		var createdAt time.Time
		if err := rows.Scan(&id, &wsID, &actorID, &action, &resourceType, &resourceID, &metadata, &ip, &createdAt); err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		entries = append(entries, entry{id: id, createdAt: createdAt, body: map[string]interface{}{
			"id": id, "workspace_id": wsID, "actor_id": actorID,
			"action": action, "resource_type": resourceType, "resource_id": resourceID,
			"metadata": metadata, "ip_address": ip, "created_at": createdAt,
		}})
	}
	if err := rows.Err(); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	hasMore := len(entries) > params.Limit
	if hasMore {
		entries = entries[:params.Limit]
	}

	logs := make([]map[string]interface{}, 0, len(entries))
	for _, e := range entries {
		logs = append(logs, e.body)
	}

	var cursor string
	if len(entries) > 0 {
		last := entries[len(entries)-1]
		cursor = httputil.EncodeCursor(last.createdAt, last.id)
	}

	httputil.JSONList(w, http.StatusOK, logs, cursor, hasMore)
}
