package message

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/Wick-Lim/SuperOps/backend/internal/channel"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"

	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

type Handler struct {
	repo    *Repository
	chanRepo *channel.Repository
	nats    *natspkg.Client
}

func NewHandler(repo *Repository, chanRepo *channel.Repository, nats *natspkg.Client) *Handler {
	return &Handler{repo: repo, chanRepo: chanRepo, nats: nats}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("POST /api/v1/channels/{channel_id}/messages", authMw(http.HandlerFunc(h.Send)))
	mux.Handle("GET /api/v1/channels/{channel_id}/messages", authMw(http.HandlerFunc(h.List)))
	mux.Handle("GET /api/v1/channels/{channel_id}/messages/{message_id}", authMw(http.HandlerFunc(h.Get)))
	mux.Handle("PATCH /api/v1/channels/{channel_id}/messages/{message_id}", authMw(http.HandlerFunc(h.Edit)))
	mux.Handle("DELETE /api/v1/channels/{channel_id}/messages/{message_id}", authMw(http.HandlerFunc(h.Delete)))
	mux.Handle("POST /api/v1/channels/{channel_id}/messages/{message_id}/reactions", authMw(http.HandlerFunc(h.AddReaction)))
	mux.Handle("DELETE /api/v1/channels/{channel_id}/messages/{message_id}/reactions/{emoji}", authMw(http.HandlerFunc(h.RemoveReaction)))
	mux.Handle("GET /api/v1/messages/{message_id}/thread", authMw(http.HandlerFunc(h.ListThread)))
	mux.Handle("POST /api/v1/messages/{message_id}/thread", authMw(http.HandlerFunc(h.ReplyThread)))
	mux.Handle("POST /api/v1/channels/{channel_id}/messages/{message_id}/pin", authMw(http.HandlerFunc(h.Pin)))
	mux.Handle("DELETE /api/v1/channels/{channel_id}/messages/{message_id}/pin", authMw(http.HandlerFunc(h.Unpin)))
	mux.Handle("POST /api/v1/messages/{message_id}/bookmark", authMw(http.HandlerFunc(h.Bookmark)))
	mux.Handle("DELETE /api/v1/messages/{message_id}/bookmark", authMw(http.HandlerFunc(h.RemoveBookmark)))
	mux.Handle("GET /api/v1/bookmarks", authMw(http.HandlerFunc(h.ListBookmarks)))
	mux.Handle("POST /api/v1/channels/{channel_id}/messages/{message_id}/forward", authMw(http.HandlerFunc(h.Forward)))
}

func (h *Handler) Pin(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	msgID := r.PathValue("message_id")
	h.repo.pool.Exec(r.Context(), `UPDATE messages SET is_pinned = TRUE, pinned_by = $2, pinned_at = NOW() WHERE id = $1`, msgID, userID)
	httputil.JSON(w, http.StatusOK, map[string]string{"message": "pinned"})
}

func (h *Handler) Unpin(w http.ResponseWriter, r *http.Request) {
	msgID := r.PathValue("message_id")
	h.repo.pool.Exec(r.Context(), `UPDATE messages SET is_pinned = FALSE, pinned_by = NULL, pinned_at = NULL WHERE id = $1`, msgID)
	httputil.JSON(w, http.StatusOK, map[string]string{"message": "unpinned"})
}

func (h *Handler) Bookmark(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	msgID := r.PathValue("message_id")
	h.repo.pool.Exec(r.Context(), `INSERT INTO bookmarks (user_id, message_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, userID, msgID)
	httputil.JSON(w, http.StatusCreated, map[string]string{"message": "bookmarked"})
}

func (h *Handler) RemoveBookmark(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	msgID := r.PathValue("message_id")
	h.repo.pool.Exec(r.Context(), `DELETE FROM bookmarks WHERE user_id = $1 AND message_id = $2`, userID, msgID)
	httputil.JSON(w, http.StatusOK, map[string]string{"message": "removed"})
}

func (h *Handler) ListBookmarks(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	rows, err := h.repo.pool.Query(r.Context(),
		`SELECT m.id, m.channel_id, m.user_id, m.content, m.created_at
		 FROM bookmarks b JOIN messages m ON b.message_id = m.id
		 WHERE b.user_id = $1 ORDER BY b.created_at DESC LIMIT 50`, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	defer rows.Close()

	var items []map[string]interface{}
	for rows.Next() {
		var id, chID, uid, content string
		var createdAt interface{}
		rows.Scan(&id, &chID, &uid, &content, &createdAt)
		items = append(items, map[string]interface{}{"id": id, "channel_id": chID, "user_id": uid, "content": content, "created_at": createdAt})
	}
	if items == nil { items = []map[string]interface{}{} }
	httputil.JSON(w, http.StatusOK, items)
}

func (h *Handler) Forward(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	msgID := r.PathValue("message_id")

	var input struct {
		TargetChannelID string `json:"target_channel_id"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil || input.TargetChannelID == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "target_channel_id is required")
		return
	}

	// Get original message
	msg, err := h.repo.GetByID(r.Context(), msgID)
	if err != nil || msg == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "message not found")
		return
	}

	// Create forwarded message in target channel
	fwd := &Message{
		ID:          uuid.NewString(),
		ChannelID:   input.TargetChannelID,
		UserID:      userID,
		Content:     "[Forwarded] " + msg.Content,
		ContentType: "markdown",
	}
	h.repo.Create(r.Context(), fwd)
	h.chanRepo.UpdateLastMessage(r.Context(), input.TargetChannelID, time.Now())

	httputil.JSON(w, http.StatusCreated, fwd)
}

func (h *Handler) Send(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	chID := r.PathValue("channel_id")

	// Check membership
	member, err := h.chanRepo.GetMember(r.Context(), chID, userID)
	if err != nil || member == nil {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "not a member of this channel")
		return
	}

	var input struct {
		Content     string  `json:"content"`
		ContentType string  `json:"content_type"`
		ParentID    *string `json:"parent_id"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	if input.Content == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "content is required")
		return
	}

	if input.ContentType == "" {
		input.ContentType = "markdown"
	}

	msg := &Message{
		ID:          uuid.NewString(),
		ChannelID:   chID,
		UserID:      userID,
		ParentID:    input.ParentID,
		Content:     input.Content,
		ContentType: input.ContentType,
	}

	if err := h.repo.Create(r.Context(), msg); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	// Update channel last_message_at
	now := time.Now()
	h.chanRepo.UpdateLastMessage(r.Context(), chID, now)

	// Fetch the full message to return with timestamps
	created, _ := h.repo.GetByID(r.Context(), msg.ID)
	if created != nil {
		msg = created
	}

	// Publish event via NATS
	ch, _ := h.chanRepo.GetByID(r.Context(), chID)
	if ch != nil && h.nats != nil {
		h.nats.Publish(
			"superops."+ch.WorkspaceID+".message.created",
			natspkg.Event{Type: "message.new", Data: msg},
		)
	}

	httputil.JSON(w, http.StatusCreated, msg)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	chID := r.PathValue("channel_id")
	params := httputil.ParsePagination(r)

	messages, err := h.repo.ListByChannel(r.Context(), chID, params.Cursor, params.Limit+1)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	hasMore := len(messages) > params.Limit
	if hasMore {
		messages = messages[:params.Limit]
	}

	if messages == nil {
		messages = []*Message{}
	}

	var cursor string
	if len(messages) > 0 {
		cursor = httputil.EncodeCursor(messages[len(messages)-1].CreatedAt)
	}

	httputil.JSONList(w, http.StatusOK, messages, cursor, hasMore)
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	msg, err := h.repo.GetByID(r.Context(), r.PathValue("message_id"))
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if msg == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "message not found")
		return
	}
	httputil.JSON(w, http.StatusOK, msg)
}

func (h *Handler) Edit(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	msgID := r.PathValue("message_id")

	msg, err := h.repo.GetByID(r.Context(), msgID)
	if err != nil || msg == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "message not found")
		return
	}
	if msg.UserID != userID {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "can only edit your own messages")
		return
	}

	var input struct {
		Content string `json:"content"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	if err := h.repo.Update(r.Context(), msgID, input.Content); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	updated, _ := h.repo.GetByID(r.Context(), msgID)
	httputil.JSON(w, http.StatusOK, updated)
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	msgID := r.PathValue("message_id")

	msg, err := h.repo.GetByID(r.Context(), msgID)
	if err != nil || msg == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "message not found")
		return
	}
	if msg.UserID != userID {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "can only delete your own messages")
		return
	}

	if err := h.repo.SoftDelete(r.Context(), msgID); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "deleted"})
}

func (h *Handler) AddReaction(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	msgID := r.PathValue("message_id")

	var input struct {
		Emoji string `json:"emoji"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil || input.Emoji == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "emoji is required")
		return
	}

	reaction := &Reaction{
		ID:        uuid.NewString(),
		MessageID: msgID,
		UserID:    userID,
		Emoji:     input.Emoji,
	}

	if err := h.repo.AddReaction(r.Context(), reaction); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusCreated, reaction)
}

func (h *Handler) RemoveReaction(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	msgID := r.PathValue("message_id")
	emoji := r.PathValue("emoji")

	if err := h.repo.RemoveReaction(r.Context(), msgID, userID, emoji); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "reaction removed"})
}

func (h *Handler) ListThread(w http.ResponseWriter, r *http.Request) {
	parentID := r.PathValue("message_id")
	messages, err := h.repo.ListThread(r.Context(), parentID, 100)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if messages == nil {
		messages = []*Message{}
	}
	httputil.JSON(w, http.StatusOK, messages)
}

func (h *Handler) ReplyThread(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	parentID := r.PathValue("message_id")

	parent, err := h.repo.GetByID(r.Context(), parentID)
	if err != nil || parent == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "parent message not found")
		return
	}

	var input struct {
		Content string `json:"content"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil || input.Content == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "content is required")
		return
	}

	msg := &Message{
		ID:          uuid.NewString(),
		ChannelID:   parent.ChannelID,
		UserID:      userID,
		ParentID:    &parentID,
		Content:     input.Content,
		ContentType: "markdown",
	}

	if err := h.repo.Create(r.Context(), msg); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	created, _ := h.repo.GetByID(r.Context(), msg.ID)
	if created != nil {
		msg = created
	}

	httputil.JSON(w, http.StatusCreated, msg)
}
