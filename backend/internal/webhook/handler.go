package webhook

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/crypto"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

type Handler struct {
	pool *pgxpool.Pool
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{pool: pool}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("POST /api/v1/webhooks", authMw(http.HandlerFunc(h.Create)))
	mux.Handle("GET /api/v1/webhooks", authMw(http.HandlerFunc(h.List)))
	mux.Handle("DELETE /api/v1/webhooks/{webhook_id}", authMw(http.HandlerFunc(h.Delete)))
	mux.HandleFunc("POST /api/v1/webhooks/incoming/{token}", h.IncomingWebhook)
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())

	var input struct {
		Name      string `json:"name"`
		ChannelID string `json:"channel_id"`
		Type      string `json:"type"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil || input.Name == "" || input.ChannelID == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "name and channel_id are required")
		return
	}
	if input.Type == "" {
		input.Type = "incoming"
	}

	token, _ := crypto.GenerateRandomToken(24)

	// Get workspace from channel
	var wsID string
	h.pool.QueryRow(r.Context(), `SELECT workspace_id FROM channels WHERE id = $1`, input.ChannelID).Scan(&wsID)

	whID := uuid.NewString()
	_, err := h.pool.Exec(r.Context(),
		`INSERT INTO webhooks (id, workspace_id, channel_id, name, type, token, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		whID, wsID, input.ChannelID, input.Name, input.Type, token, userID,
	)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusCreated, map[string]string{
		"id": whID, "token": token, "webhook_url": "/api/v1/webhooks/incoming/" + token,
	})
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	rows, err := h.pool.Query(r.Context(),
		`SELECT id, name, type, token, channel_id, is_active, created_at FROM webhooks ORDER BY created_at DESC LIMIT 50`)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	defer rows.Close()

	var hooks []map[string]interface{}
	for rows.Next() {
		var id, name, typ, token string
		var chID interface{}
		var isActive bool
		var createdAt interface{}
		rows.Scan(&id, &name, &typ, &token, &chID, &isActive, &createdAt)
		hooks = append(hooks, map[string]interface{}{
			"id": id, "name": name, "type": typ, "token": token, "channel_id": chID, "is_active": isActive, "created_at": createdAt,
		})
	}
	if hooks == nil {
		hooks = []map[string]interface{}{}
	}
	httputil.JSON(w, http.StatusOK, hooks)
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	whID := r.PathValue("webhook_id")
	h.pool.Exec(r.Context(), `DELETE FROM webhooks WHERE id = $1`, whID)
	httputil.JSON(w, http.StatusOK, map[string]string{"message": "deleted"})
}

// IncomingWebhook receives external messages via webhook token
func (h *Handler) IncomingWebhook(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")

	var chID string
	var isActive bool
	err := h.pool.QueryRow(r.Context(),
		`SELECT channel_id, is_active FROM webhooks WHERE token = $1 AND type = 'incoming'`, token,
	).Scan(&chID, &isActive)
	if err != nil || !isActive {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "webhook not found or inactive")
		return
	}

	var input struct {
		Text     string `json:"text"`
		Username string `json:"username"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil || input.Text == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "text is required")
		return
	}
	if input.Username == "" {
		input.Username = "webhook"
	}

	// Insert message as system/bot message
	msgID := uuid.NewString()
	h.pool.Exec(r.Context(),
		`INSERT INTO messages (id, channel_id, user_id, content, content_type)
		 VALUES ($1, $2, (SELECT id FROM users WHERE username = 'admin' LIMIT 1), $3, 'system')`,
		msgID, chID, "["+input.Username+"] "+input.Text,
	)
	h.pool.Exec(r.Context(), `UPDATE channels SET last_message_at = $2 WHERE id = $1`, chID, time.Now())

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "posted"})
}
