package message

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/Wick-Lim/SuperOps/backend/internal/channel"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"

	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

// Wire/outbound event types (mirror internal/ws.Type* constants — these are the
// public wire protocol, decoupled to avoid an import cycle).
const (
	evtMessageNew      = "message.new"
	evtMessageUpdated  = "message.updated"
	evtMessageDeleted  = "message.deleted"
	evtReactionAdded   = "reaction.added"
	evtReactionRemoved = "reaction.removed"
)

type Handler struct {
	repo     *Repository
	chanRepo *channel.Repository
	nats     *natspkg.Client
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
	mux.Handle("GET /api/v1/messages/{message_id}/reactions", authMw(http.HandlerFunc(h.GetReactions)))
	mux.Handle("GET /api/v1/messages/{message_id}/thread", authMw(http.HandlerFunc(h.ListThread)))
	mux.Handle("POST /api/v1/messages/{message_id}/thread", authMw(http.HandlerFunc(h.ReplyThread)))
	mux.Handle("GET /api/v1/channels/{channel_id}/scheduled", authMw(http.HandlerFunc(h.ListScheduled)))
	mux.Handle("DELETE /api/v1/channels/{channel_id}/scheduled/{message_id}", authMw(http.HandlerFunc(h.CancelScheduled)))
	mux.Handle("GET /api/v1/channels/{channel_id}/pins", authMw(http.HandlerFunc(h.ListPins)))
	mux.Handle("POST /api/v1/channels/{channel_id}/messages/{message_id}/pin", authMw(http.HandlerFunc(h.Pin)))
	mux.Handle("DELETE /api/v1/channels/{channel_id}/messages/{message_id}/pin", authMw(http.HandlerFunc(h.Unpin)))
	mux.Handle("POST /api/v1/messages/{message_id}/bookmark", authMw(http.HandlerFunc(h.Bookmark)))
	mux.Handle("DELETE /api/v1/messages/{message_id}/bookmark", authMw(http.HandlerFunc(h.RemoveBookmark)))
	mux.Handle("GET /api/v1/bookmarks", authMw(http.HandlerFunc(h.ListBookmarks)))
	mux.Handle("POST /api/v1/channels/{channel_id}/messages/{message_id}/forward", authMw(http.HandlerFunc(h.Forward)))
}

// --- event publishing helpers ---

func subjectSuffix(eventType string) string {
	switch eventType {
	case evtMessageNew:
		return "message.created"
	case evtMessageUpdated:
		return "message.updated"
	case evtMessageDeleted:
		return "message.deleted"
	default:
		return eventType
	}
}

func (h *Handler) publishWS(workspaceID, eventType string, data interface{}) {
	if h.nats == nil || workspaceID == "" {
		return
	}
	_ = h.nats.Publish("superops."+workspaceID+"."+subjectSuffix(eventType), natspkg.Event{Type: eventType, Data: data})
}

func (h *Handler) publishCh(ctx context.Context, channelID, eventType string, data interface{}) {
	if h.nats == nil {
		return
	}
	ch, err := h.chanRepo.GetByID(ctx, channelID)
	if err != nil || ch == nil {
		return
	}
	h.publishWS(ch.WorkspaceID, eventType, data)
}

// requireMember returns the caller's channel membership or writes a 403 and
// returns false.
func (h *Handler) requireMember(w http.ResponseWriter, r *http.Request, channelID string) bool {
	userID := authctx.UserID(r.Context())
	member, err := h.chanRepo.GetMember(r.Context(), channelID, userID)
	if err != nil || member == nil {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "not a member of this channel")
		return false
	}
	return true
}

// requireMessageMember loads a message and verifies the caller is a member of
// its channel (the authoritative channel, not whatever is in the URL path).
func (h *Handler) requireMessageMember(w http.ResponseWriter, r *http.Request, messageID string) (*Message, bool) {
	msg, err := h.repo.GetByID(r.Context(), messageID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return nil, false
	}
	if msg == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "message not found")
		return nil, false
	}
	userID := authctx.UserID(r.Context())
	member, err := h.chanRepo.GetMember(r.Context(), msg.ChannelID, userID)
	if err != nil || member == nil {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "not a member of this channel")
		return nil, false
	}
	return msg, true
}

// --- handlers ---

func (h *Handler) Send(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	chID := r.PathValue("channel_id")

	member, err := h.chanRepo.GetMember(r.Context(), chID, userID)
	if err != nil || member == nil {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "not a member of this channel")
		return
	}

	var input struct {
		Content     string     `json:"content"`
		ContentType string     `json:"content_type"`
		ParentID    *string    `json:"parent_id"`
		FileIDs     []string   `json:"file_ids"`
		ScheduledAt *time.Time `json:"scheduled_at"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	// A message must carry text or at least one attachment.
	if input.Content == "" && len(input.FileIDs) == 0 {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "content or file_ids is required")
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

	// Scheduled send: persist as pending; the worker promotes & broadcasts it later.
	if input.ScheduledAt != nil && input.ScheduledAt.After(time.Now()) {
		if err := h.repo.CreateScheduled(r.Context(), msg, *input.ScheduledAt); err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		_ = h.repo.LinkFiles(r.Context(), msg.ID, userID, input.FileIDs)
		if created, _ := h.repo.GetByID(r.Context(), msg.ID); created != nil {
			msg = created
		}
		httputil.JSON(w, http.StatusCreated, msg)
		return
	}

	if err := h.repo.Create(r.Context(), msg); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	_ = h.repo.LinkFiles(r.Context(), msg.ID, userID, input.FileIDs)

	if created, _ := h.repo.GetByID(r.Context(), msg.ID); created != nil {
		msg = created
	}
	_ = h.repo.Hydrate(r.Context(), []*Message{msg})

	h.publishCh(r.Context(), chID, evtMessageNew, msg)

	httputil.JSON(w, http.StatusCreated, msg)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	chID := r.PathValue("channel_id")
	if !h.requireMember(w, r, chID) {
		return
	}
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

	if err := h.repo.Hydrate(r.Context(), messages); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	var cursor string
	if len(messages) > 0 {
		cursor = httputil.EncodeCursor(messages[len(messages)-1].CreatedAt)
	}

	httputil.JSONList(w, http.StatusOK, messages, cursor, hasMore)
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	msg, ok := h.requireMessageMember(w, r, r.PathValue("message_id"))
	if !ok {
		return
	}
	_ = h.repo.Hydrate(r.Context(), []*Message{msg})
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
	if err := httputil.DecodeJSON(r, &input); err != nil || input.Content == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "content is required")
		return
	}

	if err := h.repo.Update(r.Context(), msgID, input.Content); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	updated, err := h.repo.GetByID(r.Context(), msgID)
	if err != nil || updated == nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	_ = h.repo.Hydrate(r.Context(), []*Message{updated})

	h.publishCh(r.Context(), updated.ChannelID, evtMessageUpdated, updated)

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

	h.publishCh(r.Context(), msg.ChannelID, evtMessageDeleted, map[string]string{
		"id":         msgID,
		"channel_id": msg.ChannelID,
	})

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "deleted"})
}

func (h *Handler) AddReaction(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	chID := r.PathValue("channel_id")
	msgID := r.PathValue("message_id")
	if !h.requireMember(w, r, chID) {
		return
	}

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

	inserted, err := h.repo.AddReaction(r.Context(), reaction)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	if inserted {
		h.publishCh(r.Context(), chID, evtReactionAdded, map[string]string{
			"channel_id": chID,
			"message_id": msgID,
			"user_id":    userID,
			"emoji":      input.Emoji,
		})
	}

	httputil.JSON(w, http.StatusCreated, reaction)
}

func (h *Handler) RemoveReaction(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	chID := r.PathValue("channel_id")
	msgID := r.PathValue("message_id")
	emoji := r.PathValue("emoji")
	if !h.requireMember(w, r, chID) {
		return
	}

	if err := h.repo.RemoveReaction(r.Context(), msgID, userID, emoji); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	h.publishCh(r.Context(), chID, evtReactionRemoved, map[string]string{
		"channel_id": chID,
		"message_id": msgID,
		"user_id":    userID,
		"emoji":      emoji,
	})

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "reaction removed"})
}

func (h *Handler) GetReactions(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireMessageMember(w, r, r.PathValue("message_id")); !ok {
		return
	}
	reactions, err := h.repo.ListReactions(r.Context(), r.PathValue("message_id"))
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if reactions == nil {
		reactions = []*Reaction{}
	}
	httputil.JSON(w, http.StatusOK, reactions)
}

func (h *Handler) ListThread(w http.ResponseWriter, r *http.Request) {
	parentID := r.PathValue("message_id")
	if _, ok := h.requireMessageMember(w, r, parentID); !ok {
		return
	}
	messages, err := h.repo.ListThread(r.Context(), parentID, 100)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if messages == nil {
		messages = []*Message{}
	}
	_ = h.repo.Hydrate(r.Context(), messages)
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

	if member, err := h.chanRepo.GetMember(r.Context(), parent.ChannelID, userID); err != nil || member == nil {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "not a member of this channel")
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

	if created, _ := h.repo.GetByID(r.Context(), msg.ID); created != nil {
		msg = created
	}

	// Broadcast the new reply, then the parent (its reply_count changed).
	h.publishCh(r.Context(), msg.ChannelID, evtMessageNew, msg)
	if updatedParent, _ := h.repo.GetByID(r.Context(), parentID); updatedParent != nil {
		h.publishCh(r.Context(), msg.ChannelID, evtMessageUpdated, updatedParent)
	}

	httputil.JSON(w, http.StatusCreated, msg)
}

func (h *Handler) ListScheduled(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	chID := r.PathValue("channel_id")
	if !h.requireMember(w, r, chID) {
		return
	}
	messages, err := h.repo.ListScheduled(r.Context(), chID, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if messages == nil {
		messages = []*Message{}
	}
	_ = h.repo.Hydrate(r.Context(), messages)
	httputil.JSON(w, http.StatusOK, messages)
}

func (h *Handler) CancelScheduled(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	msgID := r.PathValue("message_id")
	ok, err := h.repo.CancelScheduled(r.Context(), msgID, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !ok {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "scheduled message not found")
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]string{"message": "canceled"})
}

func (h *Handler) ListPins(w http.ResponseWriter, r *http.Request) {
	chID := r.PathValue("channel_id")
	if !h.requireMember(w, r, chID) {
		return
	}
	messages, err := h.repo.ListPinned(r.Context(), chID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if messages == nil {
		messages = []*Message{}
	}
	_ = h.repo.Hydrate(r.Context(), messages)
	httputil.JSON(w, http.StatusOK, messages)
}

func (h *Handler) Pin(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	chID := r.PathValue("channel_id")
	msgID := r.PathValue("message_id")
	if !h.requireMember(w, r, chID) {
		return
	}
	if err := h.repo.SetPinned(r.Context(), msgID, userID, true); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if updated, _ := h.repo.GetByID(r.Context(), msgID); updated != nil {
		h.publishCh(r.Context(), chID, evtMessageUpdated, updated)
	}
	httputil.JSON(w, http.StatusOK, map[string]string{"message": "pinned"})
}

func (h *Handler) Unpin(w http.ResponseWriter, r *http.Request) {
	chID := r.PathValue("channel_id")
	msgID := r.PathValue("message_id")
	if !h.requireMember(w, r, chID) {
		return
	}
	if err := h.repo.SetPinned(r.Context(), msgID, "", false); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if updated, _ := h.repo.GetByID(r.Context(), msgID); updated != nil {
		h.publishCh(r.Context(), chID, evtMessageUpdated, updated)
	}
	httputil.JSON(w, http.StatusOK, map[string]string{"message": "unpinned"})
}

func (h *Handler) Bookmark(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	msgID := r.PathValue("message_id")
	if _, ok := h.requireMessageMember(w, r, msgID); !ok {
		return
	}
	if _, err := h.repo.pool.Exec(r.Context(),
		`INSERT INTO bookmarks (user_id, message_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, userID, msgID); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusCreated, map[string]string{"message": "bookmarked"})
}

func (h *Handler) RemoveBookmark(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	msgID := r.PathValue("message_id")
	if _, err := h.repo.pool.Exec(r.Context(),
		`DELETE FROM bookmarks WHERE user_id = $1 AND message_id = $2`, userID, msgID); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
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

	items := []map[string]interface{}{}
	for rows.Next() {
		var id, chID, uid, content string
		var createdAt interface{}
		if err := rows.Scan(&id, &chID, &uid, &content, &createdAt); err != nil {
			continue
		}
		items = append(items, map[string]interface{}{"id": id, "channel_id": chID, "user_id": uid, "content": content, "created_at": createdAt})
	}
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

	// Caller must be a member of the SOURCE channel (to read the message)...
	msg, ok := h.requireMessageMember(w, r, msgID)
	if !ok {
		return
	}
	// ...and of the TARGET channel (to post into it).
	if member, err := h.chanRepo.GetMember(r.Context(), input.TargetChannelID, userID); err != nil || member == nil {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "not a member of the target channel")
		return
	}

	fwd := &Message{
		ID:          uuid.NewString(),
		ChannelID:   input.TargetChannelID,
		UserID:      userID,
		Content:     "[Forwarded] " + msg.Content,
		ContentType: "markdown",
	}
	if err := h.repo.Create(r.Context(), fwd); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if created, _ := h.repo.GetByID(r.Context(), fwd.ID); created != nil {
		fwd = created
	}

	h.publishCh(r.Context(), input.TargetChannelID, evtMessageNew, fwd)

	httputil.JSON(w, http.StatusCreated, fwd)
}
