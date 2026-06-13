package admin

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/audit"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/crypto"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

type Handler struct {
	pool  *pgxpool.Pool
	audit *audit.Service
}

func NewHandler(pool *pgxpool.Pool, auditSvc *audit.Service) *Handler {
	return &Handler{pool: pool, audit: auditSvc}
}

// workspaceOf returns the caller's first workspace id (best-effort, for audit scoping).
func (h *Handler) workspaceOf(r *http.Request, userID string) string {
	var wsID string
	_ = h.pool.QueryRow(r.Context(), `SELECT workspace_id FROM workspace_members WHERE user_id = $1 LIMIT 1`, userID).Scan(&wsID)
	return wsID
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("GET /api/v1/admin/users", authMw(http.HandlerFunc(h.ListUsers)))
	mux.Handle("PATCH /api/v1/admin/users/{user_id}", authMw(http.HandlerFunc(h.UpdateUser)))
	mux.Handle("GET /api/v1/admin/stats", authMw(http.HandlerFunc(h.Stats)))
	mux.Handle("GET /api/v1/admin/audit-logs", authMw(http.HandlerFunc(h.AuditLogs)))
	mux.Handle("POST /api/v1/admin/invitations", authMw(http.HandlerFunc(h.CreateInvitation)))
	mux.Handle("GET /api/v1/admin/invitations", authMw(http.HandlerFunc(h.ListInvitations)))
}

func (h *Handler) CreateInvitation(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())

	var input struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil || input.Email == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "email is required")
		return
	}
	if input.Role == "" {
		input.Role = "member"
	}

	// Get user's first workspace
	var wsID string
	err := h.pool.QueryRow(r.Context(),
		`SELECT workspace_id FROM workspace_members WHERE user_id = $1 LIMIT 1`, userID,
	).Scan(&wsID)
	if err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "no workspace found")
		return
	}

	token, err := crypto.GenerateRandomToken(24)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	_, err = h.pool.Exec(r.Context(),
		`INSERT INTO invitations (email, workspace_id, role, token, invited_by, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		input.Email, wsID, input.Role, token, userID, time.Now().Add(72*time.Hour),
	)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	if h.audit != nil {
		_ = h.audit.Log(r.Context(), wsID, userID, "invitation.created", "invitation", "",
			map[string]interface{}{"email": input.Email, "role": input.Role})
	}

	httputil.JSON(w, http.StatusCreated, map[string]string{
		"token":      token,
		"invite_url": "/invite/" + token,
		"email":      input.Email,
	})
}

func (h *Handler) ListInvitations(w http.ResponseWriter, r *http.Request) {
	rows, err := h.pool.Query(r.Context(),
		`SELECT i.id, i.email, i.role, i.token, i.status, i.expires_at, i.created_at, COALESCE(u.full_name, u.username)
		 FROM invitations i JOIN users u ON i.invited_by = u.id
		 ORDER BY i.created_at DESC LIMIT 100`)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	defer rows.Close()

	var invites []map[string]interface{}
	for rows.Next() {
		var id, email, role, token, status, inviter string
		var expiresAt, createdAt interface{}
		if err := rows.Scan(&id, &email, &role, &token, &status, &expiresAt, &createdAt, &inviter); err != nil {
			continue
		}
		invites = append(invites, map[string]interface{}{
			"id": id, "email": email, "role": role, "token": token,
			"status": status, "expires_at": expiresAt, "created_at": createdAt, "invited_by": inviter,
		})
	}
	if invites == nil {
		invites = []map[string]interface{}{}
	}
	httputil.JSON(w, http.StatusOK, invites)
}

func (h *Handler) ListUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := h.pool.Query(r.Context(),
		`SELECT id, email, username, full_name, is_active, is_bot, created_at
		 FROM users ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	defer rows.Close()

	var users []map[string]interface{}
	for rows.Next() {
		var id, email, username, fullName string
		var isActive, isBot bool
		var createdAt interface{}
		if err := rows.Scan(&id, &email, &username, &fullName, &isActive, &isBot, &createdAt); err != nil {
			continue
		}
		users = append(users, map[string]interface{}{
			"id": id, "email": email, "username": username, "full_name": fullName,
			"is_active": isActive, "is_bot": isBot, "created_at": createdAt,
		})
	}
	if users == nil {
		users = []map[string]interface{}{}
	}
	httputil.JSON(w, http.StatusOK, users)
}

func (h *Handler) UpdateUser(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("user_id")

	var input struct {
		IsActive *bool   `json:"is_active"`
		Role     *string `json:"role"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	if input.IsActive != nil {
		if _, err := h.pool.Exec(r.Context(), `UPDATE users SET is_active = $2 WHERE id = $1`, uid, *input.IsActive); err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		if h.audit != nil {
			actor := authctx.UserID(r.Context())
			_ = h.audit.Log(r.Context(), h.workspaceOf(r, actor), actor, "user.updated", "user", uid,
				map[string]interface{}{"is_active": *input.IsActive})
		}
	}

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "user updated"})
}

func (h *Handler) Stats(w http.ResponseWriter, r *http.Request) {
	var userCount, wsCount, channelCount, messageCount int

	h.pool.QueryRow(r.Context(), `SELECT COUNT(*) FROM users`).Scan(&userCount)
	h.pool.QueryRow(r.Context(), `SELECT COUNT(*) FROM workspaces`).Scan(&wsCount)
	h.pool.QueryRow(r.Context(), `SELECT COUNT(*) FROM channels`).Scan(&channelCount)
	h.pool.QueryRow(r.Context(), `SELECT COUNT(*) FROM messages`).Scan(&messageCount)

	httputil.JSON(w, http.StatusOK, map[string]int{
		"users": userCount, "workspaces": wsCount,
		"channels": channelCount, "messages": messageCount,
	})
}

func (h *Handler) AuditLogs(w http.ResponseWriter, r *http.Request) {
	params := httputil.ParsePagination(r)
	actor := authctx.UserID(r.Context())

	// Only expose audit logs for workspaces the caller administers.
	rows, err := h.pool.Query(r.Context(),
		`SELECT id, workspace_id, actor_id, action, resource_type, resource_id, metadata::text, COALESCE(host(ip_address),''), created_at
		 FROM audit_logs
		 WHERE workspace_id IN (SELECT workspace_id FROM workspace_members WHERE user_id = $1 AND role IN ('owner','admin'))
		 ORDER BY created_at DESC LIMIT $2`, actor, params.Limit)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	defer rows.Close()

	var logs []map[string]interface{}
	for rows.Next() {
		var id, action, resourceType, metadata, ip string
		var wsID, actorID, resourceID interface{}
		var createdAt interface{}
		if err := rows.Scan(&id, &wsID, &actorID, &action, &resourceType, &resourceID, &metadata, &ip, &createdAt); err != nil {
			continue
		}
		logs = append(logs, map[string]interface{}{
			"id": id, "workspace_id": wsID, "actor_id": actorID,
			"action": action, "resource_type": resourceType, "resource_id": resourceID,
			"metadata": metadata, "ip_address": ip, "created_at": createdAt,
		})
	}
	if logs == nil {
		logs = []map[string]interface{}{}
	}
	httputil.JSON(w, http.StatusOK, logs)
}
