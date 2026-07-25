package webhook

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/crypto"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
)

// typeIncoming is the only webhook type that does anything. 'outgoing' is in
// the schema's CHECK constraint but nothing in the codebase ever dispatches
// one, so accepting it would persist a hook that silently never fires.
const typeIncoming = "incoming"

// tokenBytes is the entropy of an incoming-webhook credential. Only its
// crypto.HashToken is persisted (migration 009 replaced webhooks.token with
// token_hash), so the plaintext is returned exactly once — at create and at
// rotate — and is unrecoverable afterwards.
const tokenBytes = 24

type Handler struct {
	pool  *pgxpool.Pool
	authz *authz.Checker
	nats  *natspkg.Client
}

func NewHandler(pool *pgxpool.Pool, az *authz.Checker, nats *natspkg.Client) *Handler {
	return &Handler{pool: pool, authz: az, nats: nats}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("POST /api/v1/webhooks", authMw(http.HandlerFunc(h.Create)))
	mux.Handle("GET /api/v1/webhooks", authMw(http.HandlerFunc(h.List)))
	mux.Handle("PATCH /api/v1/webhooks/{webhook_id}", authMw(http.HandlerFunc(h.Update)))
	// Rotation is PUT-on-the-token, not POST-.../rotate, because
	// `POST /api/v1/webhooks/{webhook_id}/rotate` is ambiguous against the
	// legacy delivery route below: "/api/v1/webhooks/incoming/rotate" matches
	// both and neither pattern is more specific, which makes ServeMux panic at
	// registration. Differing methods cannot conflict, and replacing the token
	// resource is a fair reading of PUT anyway.
	mux.Handle("PUT /api/v1/webhooks/{webhook_id}/token", authMw(http.HandlerFunc(h.Rotate)))
	mux.Handle("DELETE /api/v1/webhooks/{webhook_id}", authMw(http.HandlerFunc(h.Delete)))

	// Preferred delivery endpoint: the credential travels in the Authorization
	// header, which no access log records.
	mux.HandleFunc("POST /api/v1/webhooks/incoming", h.IncomingWebhook)
	// Legacy delivery endpoint. The token is a path segment, and every request
	// path is written to the application log and to nginx's access log, so this
	// form leaks the credential on every single delivery. Kept only so already
	// deployed hooks keep working; see the redaction requirement in the wiring
	// notes and prefer the header form for anything new.
	mux.HandleFunc("POST /api/v1/webhooks/incoming/{token}", h.IncomingWebhookLegacy)
}

// requireAdminOfWebhook resolves a webhook and verifies the caller administers
// its workspace.
func (h *Handler) requireAdminOfWebhook(w http.ResponseWriter, r *http.Request, whID string) (workspaceID, channelID string, ok bool) {
	userID := authctx.UserID(r.Context())

	err := h.pool.QueryRow(r.Context(),
		`SELECT workspace_id, channel_id FROM webhooks WHERE id = $1`, whID,
	).Scan(&workspaceID, &channelID)
	if errors.Is(err, pgx.ErrNoRows) {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "webhook not found")
		return "", "", false
	}
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return "", "", false
	}

	admin, err := h.authz.IsWorkspaceAdmin(r.Context(), workspaceID, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return "", "", false
	}
	if !admin {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "workspace admin privileges required")
		return "", "", false
	}
	return workspaceID, channelID, true
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := authctx.UserID(ctx)

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
		input.Type = typeIncoming
	}
	if input.Type != typeIncoming {
		httputil.JSONError(w, http.StatusBadRequest, "UNSUPPORTED_WEBHOOK_TYPE",
			"only incoming webhooks are supported")
		return
	}

	ch, err := h.authz.Channel(ctx, input.ChannelID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if ch == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "channel not found")
		return
	}
	admin, err := h.authz.IsWorkspaceAdmin(ctx, ch.WorkspaceID, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !admin {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "workspace admin privileges required")
		return
	}

	token, err := crypto.GenerateRandomToken(tokenBytes)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	whID := uuid.NewString()
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO webhooks (id, workspace_id, channel_id, name, type, token_hash, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		whID, ch.WorkspaceID, ch.ID, input.Name, typeIncoming, crypto.HashToken(token), userID,
	); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusCreated, map[string]string{
		"id":          whID,
		"token":       token,
		"webhook_url": "/api/v1/webhooks/incoming",
		"usage":       "POST the payload to webhook_url with header: Authorization: Bearer <token>",
	})
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())

	adminWorkspaces, err := h.authz.AdminWorkspaceIDs(r.Context(), userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	hooks := []map[string]interface{}{}
	if len(adminWorkspaces) == 0 {
		httputil.JSON(w, http.StatusOK, hooks)
		return
	}

	// The token is never returned: only its hash is stored, and re-issuing a
	// credential is what Rotate is for.
	rows, err := h.pool.Query(r.Context(),
		`SELECT id, workspace_id, name, type, channel_id, is_active, created_at FROM webhooks
		 WHERE workspace_id = ANY($1)
		 ORDER BY created_at DESC LIMIT 50`, adminWorkspaces)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	defer rows.Close()

	for rows.Next() {
		var id, wsID, name, typ, chID string
		var isActive bool
		var createdAt time.Time
		if err := rows.Scan(&id, &wsID, &name, &typ, &chID, &isActive, &createdAt); err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		hooks = append(hooks, map[string]interface{}{
			"id": id, "workspace_id": wsID, "name": name, "type": typ,
			"channel_id": chID, "is_active": isActive, "created_at": createdAt,
		})
	}
	if err := rows.Err(); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusOK, hooks)
}

// Update toggles is_active. Without it a leaked token could only be revoked by
// deleting the hook outright.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	whID := r.PathValue("webhook_id")

	var input struct {
		IsActive *bool   `json:"is_active"`
		Name     *string `json:"name"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if input.IsActive == nil && input.Name == nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "is_active or name is required")
		return
	}
	if input.Name != nil && *input.Name == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "name must not be empty")
		return
	}

	if _, _, ok := h.requireAdminOfWebhook(w, r, whID); !ok {
		return
	}

	tag, err := h.pool.Exec(r.Context(),
		`UPDATE webhooks SET
		   is_active = COALESCE($2, is_active),
		   name      = COALESCE($3, name)
		 WHERE id = $1`, whID, input.IsActive, input.Name)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if tag.RowsAffected() == 0 {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "webhook not found")
		return
	}

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "updated"})
}

// Rotate issues a new credential and invalidates the old one immediately.
func (h *Handler) Rotate(w http.ResponseWriter, r *http.Request) {
	whID := r.PathValue("webhook_id")

	if _, _, ok := h.requireAdminOfWebhook(w, r, whID); !ok {
		return
	}

	token, err := crypto.GenerateRandomToken(tokenBytes)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	tag, err := h.pool.Exec(r.Context(),
		`UPDATE webhooks SET token_hash = $2 WHERE id = $1`, whID, crypto.HashToken(token))
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if tag.RowsAffected() == 0 {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "webhook not found")
		return
	}

	httputil.JSON(w, http.StatusOK, map[string]string{
		"id":          whID,
		"token":       token,
		"webhook_url": "/api/v1/webhooks/incoming",
	})
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	whID := r.PathValue("webhook_id")

	if _, _, ok := h.requireAdminOfWebhook(w, r, whID); !ok {
		return
	}

	tag, err := h.pool.Exec(r.Context(), `DELETE FROM webhooks WHERE id = $1`, whID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if tag.RowsAffected() == 0 {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "webhook not found")
		return
	}

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "deleted"})
}

// --- delivery ---------------------------------------------------------------

// IncomingWebhook receives an external message authenticated by
// `Authorization: Bearer <token>`.
func (h *Handler) IncomingWebhook(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" {
		httputil.JSONError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing webhook token")
		return
	}
	h.deliver(w, r, token)
}

// IncomingWebhookLegacy accepts the credential as a path segment.
func (h *Handler) IncomingWebhookLegacy(w http.ResponseWriter, r *http.Request) {
	h.deliver(w, r, r.PathValue("token"))
}

func bearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// webhookMessage mirrors the JSON shape of message.Message so the WebSocket
// relay, the search indexer and the notifier all understand a webhook post.
type webhookMessage struct {
	ID          string    `json:"id"`
	ChannelID   string    `json:"channel_id"`
	UserID      string    `json:"user_id"`
	Content     string    `json:"content"`
	ContentType string    `json:"content_type"`
	IsEdited    bool      `json:"is_edited"`
	IsDeleted   bool      `json:"is_deleted"`
	ReplyCount  int       `json:"reply_count"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (h *Handler) deliver(w http.ResponseWriter, r *http.Request, token string) {
	ctx := r.Context()
	if token == "" {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "webhook not found or inactive")
		return
	}

	var wsID, chID, createdBy string
	err := h.pool.QueryRow(ctx,
		`SELECT workspace_id, channel_id, created_by FROM webhooks
		  WHERE token_hash = $1 AND type = $2 AND is_active = TRUE`,
		crypto.HashToken(token), typeIncoming,
	).Scan(&wsID, &chID, &createdBy)
	if errors.Is(err, pgx.ErrNoRows) {
		// One response for unknown, wrong-type and disabled, so the endpoint is
		// not an oracle for valid token prefixes.
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "webhook not found or inactive")
		return
	}
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
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

	// Post as a system message authored by the webhook's creator (respects
	// workspace isolation instead of assuming a global 'admin' user). The
	// display username is sanitized to a single safe line.
	msg := webhookMessage{
		ID:          uuid.NewString(),
		ChannelID:   chID,
		UserID:      createdBy,
		Content:     "[" + sanitizeName(input.Username) + "] " + input.Text,
		ContentType: "system",
	}

	if err := database.WithTx(ctx, h.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO messages (id, channel_id, user_id, content, content_type)
			 VALUES ($1, $2, $3, $4, 'system')
			 RETURNING created_at, updated_at`,
			msg.ID, msg.ChannelID, msg.UserID, msg.Content,
		).Scan(&msg.CreatedAt, &msg.UpdatedAt); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`UPDATE channels SET last_message_at = $2 WHERE id = $1`, chID, msg.CreatedAt)
		return err
	}); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	// Publish the same event the REST send path publishes. Without this a
	// webhook message reached no connected client, was never indexed for search
	// and never notified anyone, even on an @mention.
	h.publishMessage(ctx, wsID, msg)

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "posted", "id": msg.ID})
}

func (h *Handler) publishMessage(_ context.Context, workspaceID string, msg webhookMessage) {
	if h.nats == nil || workspaceID == "" {
		return
	}
	_ = h.nats.Publish("superops."+workspaceID+".message.created",
		natspkg.Event{Type: "message.new", Data: msg})
}

// sanitizeName strips line breaks and bracket characters from a webhook display
// name and caps its length.
func sanitizeName(name string) string {
	name = strings.Map(func(rr rune) rune {
		if rr == '\n' || rr == '\r' || rr == '[' || rr == ']' || rr < 0x20 {
			return -1
		}
		return rr
	}, name)
	name = strings.TrimSpace(name)
	if name == "" {
		return "webhook"
	}
	// Cap by runes, not bytes: a byte slice would cut a multibyte name mid-rune
	// and the message INSERT would then fail on invalid UTF-8.
	runes := []rune(name)
	if len(runes) > 40 {
		name = string(runes[:40])
	}
	return name
}
