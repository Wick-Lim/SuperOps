package admin

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

type Handler struct {
	pool *pgxpool.Pool
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{pool: pool}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("GET /api/v1/admin/users", authMw(http.HandlerFunc(h.ListUsers)))
	mux.Handle("PATCH /api/v1/admin/users/{user_id}", authMw(http.HandlerFunc(h.UpdateUser)))
	mux.Handle("GET /api/v1/admin/stats", authMw(http.HandlerFunc(h.Stats)))
	mux.Handle("GET /api/v1/admin/audit-logs", authMw(http.HandlerFunc(h.AuditLogs)))
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
		h.pool.Exec(r.Context(), `UPDATE users SET is_active = $2 WHERE id = $1`, uid, *input.IsActive)
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

	rows, err := h.pool.Query(r.Context(),
		`SELECT id, workspace_id, actor_id, action, resource_type, resource_id, metadata::text, COALESCE(host(ip_address),''), created_at
		 FROM audit_logs ORDER BY created_at DESC LIMIT $1`, params.Limit)
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
