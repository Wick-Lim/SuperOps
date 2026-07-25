package message

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
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

// contentTypeMarkdown is the only content_type a client may post. 'system' and
// 'file' are also accepted by the column's CHECK constraint, but they identify
// messages produced by the server (webhooks, join notices); letting a caller
// pick one lets any user impersonate them.
const contentTypeMarkdown = "markdown"

// Limits mirrored from the CHECK constraints added in migration 009, so an
// oversized body is a 400 from the handler instead of a 500 from Postgres.
const (
	maxContentRunes = 40000
	maxEmojiRunes   = 64
)

type Handler struct {
	repo *Repository
	az   *authz.Checker
	nats *natspkg.Client
	log  *slog.Logger
}

func NewHandler(repo *Repository, az *authz.Checker, nats *natspkg.Client, log *slog.Logger) *Handler {
	return &Handler{repo: repo, az: az, nats: nats, log: log}
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

// publishTimeout bounds the JetStream storage ack so a sick NATS cannot stall
// an HTTP handler that has already committed its write.
const publishTimeout = 3 * time.Second

// publish emits a domain event durably. Every event this package produces
// drives realtime delivery, search indexing and notifications, so losing one
// silently corrupts all three with no reconciliation path — it goes through
// JetStream (at-least-once, deduplicated on dedupeID) rather than fire-and-
// forget core NATS, and a failure is logged instead of discarded.
//
// The publish deliberately outlives the request context: the row is already
// committed by the time we get here, so a client that hung up must not also
// cost the workspace its event.
func (h *Handler) publish(ctx context.Context, workspaceID, eventType, dedupeID string, data any) {
	if h.nats == nil || workspaceID == "" {
		return
	}
	subject := "superops." + workspaceID + "." + subjectSuffix(eventType)

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout)
	defer cancel()

	if err := h.nats.PublishDurable(ctx, subject, dedupeID, natspkg.Event{Type: eventType, Data: data}); err != nil {
		h.log.Error("publish message event", "subject", subject, "event", eventType, "error", err)
	}
}

// eventKey identifies one logical event for JetStream's duplicate window, so a
// retried publish of the same fact collapses instead of notifying twice.
func eventKey(eventType string, parts ...string) string {
	return eventType + ":" + strings.Join(parts, ":")
}

// messageVersionKey keys an update event on the row version rather than the row
// id: two successive edits are two distinct events and must not be deduplicated
// into one. The updated_at trigger (migration 009) supplies the version.
func messageVersionKey(m *Message) string {
	return eventKey(evtMessageUpdated, m.ID, m.UpdatedAt.Format(time.RFC3339Nano))
}

// --- authorization helpers ---

// requireChannelMember resolves the channel named in the URL and verifies the
// caller is a member of it. Use it only for channel-addressed requests.
func (h *Handler) requireChannelMember(w http.ResponseWriter, r *http.Request, channelID string) (*authz.ChannelInfo, bool) {
	if _, err := uuid.Parse(channelID); err != nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "channel not found")
		return nil, false
	}
	ch, err := h.az.Channel(r.Context(), channelID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return nil, false
	}
	if ch == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "channel not found")
		return nil, false
	}
	return ch, h.requireMembership(w, r, ch)
}

// requireMessageChannel resolves the channel a message actually lives in and
// verifies membership of THAT channel. Every message-addressed operation must
// go through it: authorizing against the channel id in the URL while mutating
// by message id let any member of any channel react to, pin, unpin or leak a
// message from anywhere in the deployment.
func (h *Handler) requireMessageChannel(w http.ResponseWriter, r *http.Request, messageID string) (*authz.ChannelInfo, bool) {
	if _, err := uuid.Parse(messageID); err != nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "message not found")
		return nil, false
	}
	ch, err := h.az.MessageChannel(r.Context(), messageID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return nil, false
	}
	if ch == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "message not found")
		return nil, false
	}
	return ch, h.requireMembership(w, r, ch)
}

func (h *Handler) requireMembership(w http.ResponseWriter, r *http.Request, ch *authz.ChannelInfo) bool {
	member, err := h.az.IsChannelMember(r.Context(), ch.ID, authctx.UserID(r.Context()))
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return false
	}
	if !member {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "not a member of this channel")
		return false
	}
	return true
}

// canModerate reports whether the caller may act on someone else's message in
// this channel: channel admins and workspace admins/owners may delete, nobody
// but the author may edit.
func (h *Handler) canModerate(r *http.Request, ch *authz.ChannelInfo) (bool, error) {
	userID := authctx.UserID(r.Context())
	chanAdmin, err := h.az.IsChannelAdmin(r.Context(), ch.ID, userID)
	if err != nil || chanAdmin {
		return chanAdmin, err
	}
	return h.az.IsWorkspaceAdmin(r.Context(), ch.WorkspaceID, userID)
}

// --- input validation ---

func badRequest(code, message string) *httputil.AppError {
	return &httputil.AppError{Status: http.StatusBadRequest, Code: code, Message: message}
}

// validateContent enforces the messages_content_len CHECK constraint up front.
func validateContent(content string, fileIDs []string) error {
	if content == "" && len(fileIDs) == 0 {
		return badRequest("BAD_REQUEST", "content or file_ids is required")
	}
	if utf8.RuneCountInString(content) > maxContentRunes {
		return badRequest("CONTENT_TOO_LONG", fmt.Sprintf("content must be at most %d characters", maxContentRunes))
	}
	return nil
}

// normalizeContentType accepts only what a client is allowed to author.
func normalizeContentType(contentType string) (string, error) {
	if contentType == "" || contentType == contentTypeMarkdown {
		return contentTypeMarkdown, nil
	}
	return "", badRequest("INVALID_CONTENT_TYPE", `content_type must be "markdown"`)
}

// validateEmoji enforces the reactions_emoji_len CHECK constraint.
func validateEmoji(emoji string) error {
	if emoji == "" {
		return badRequest("BAD_REQUEST", "emoji is required")
	}
	if n := utf8.RuneCountInString(emoji); n > maxEmojiRunes {
		return badRequest("EMOJI_TOO_LONG", fmt.Sprintf("emoji must be at most %d characters", maxEmojiRunes))
	}
	return nil
}

// validateFileIDs rejects non-UUID ids before they reach Postgres, where they
// would surface as a 500 rather than the client error they are.
func validateFileIDs(fileIDs []string) error {
	for _, id := range fileIDs {
		if _, err := uuid.Parse(id); err != nil {
			return badRequest("INVALID_FILE_IDS", "file_ids must be valid file identifiers")
		}
	}
	return nil
}

// writeWriteError maps the repository's sentinel errors onto client errors;
// anything else is genuinely ours and stays a 500 with no detail leaked.
func writeWriteError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidParent):
		httputil.HandleError(w, badRequest("INVALID_PARENT",
			"parent_id must reference a live top-level message in this channel"))
	case errors.Is(err, ErrFilesUnavailable):
		httputil.HandleError(w, badRequest("INVALID_FILE_IDS",
			"file_ids must reference your own uploads that are not attached to another message"))
	default:
		httputil.HandleError(w, httputil.NewInternal(err))
	}
}

// nextCursor renders the keyset position of the last row of a page. at selects
// the timestamp the list is ordered by, which is not always created_at.
func nextCursor(messages []*Message, at func(*Message) *time.Time) string {
	if len(messages) == 0 {
		return ""
	}
	last := messages[len(messages)-1]
	t := at(last)
	if t == nil {
		return ""
	}
	return httputil.EncodeCursor(*t, last.ID)
}

func createdAt(m *Message) *time.Time   { return &m.CreatedAt }
func pinnedAt(m *Message) *time.Time    { return m.PinnedAt }
func scheduledAt(m *Message) *time.Time { return m.ScheduledAt }

// --- handlers ---

func (h *Handler) Send(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	ch, ok := h.requireChannelMember(w, r, r.PathValue("channel_id"))
	if !ok {
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

	if err := validateContent(input.Content, input.FileIDs); err != nil {
		httputil.HandleError(w, err)
		return
	}
	if err := validateFileIDs(input.FileIDs); err != nil {
		httputil.HandleError(w, err)
		return
	}
	contentType, err := normalizeContentType(input.ContentType)
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	if input.ParentID != nil {
		if _, err := uuid.Parse(*input.ParentID); err != nil {
			httputil.HandleError(w, badRequest("INVALID_PARENT", "parent_id must be a valid message identifier"))
			return
		}
	}

	msg := &Message{
		ID:          uuid.NewString(),
		ChannelID:   ch.ID,
		UserID:      userID,
		ParentID:    input.ParentID,
		Content:     input.Content,
		ContentType: contentType,
	}

	// Scheduled send: persist as pending; the worker promotes & broadcasts it later.
	if input.ScheduledAt != nil && input.ScheduledAt.After(time.Now()) {
		if err := h.repo.CreateScheduled(r.Context(), msg, *input.ScheduledAt, input.FileIDs); err != nil {
			writeWriteError(w, err)
			return
		}
		h.respondWithMessage(w, r, http.StatusCreated, msg.ID, userID, "")
		return
	}

	if err := h.repo.Create(r.Context(), msg, input.FileIDs); err != nil {
		writeWriteError(w, err)
		return
	}
	h.respondWithMessage(w, r, http.StatusCreated, msg.ID, userID, ch.WorkspaceID)
}

// respondWithMessage re-reads a just-written message (to pick up DB defaults),
// hydrates it, optionally publishes it as a new message, and writes it out.
func (h *Handler) respondWithMessage(w http.ResponseWriter, r *http.Request, status int, messageID, userID, publishWorkspaceID string) {
	msg, err := h.repo.GetForUser(r.Context(), messageID, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if msg == nil {
		httputil.HandleError(w, httputil.NewInternal(fmt.Errorf("message %s vanished after write", messageID)))
		return
	}
	if err := h.repo.Hydrate(r.Context(), []*Message{msg}); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	if publishWorkspaceID != "" {
		h.publish(r.Context(), publishWorkspaceID, evtMessageNew, eventKey(evtMessageNew, msg.ID), msg)
	}
	httputil.JSON(w, status, msg)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	ch, ok := h.requireChannelMember(w, r, r.PathValue("channel_id"))
	if !ok {
		return
	}
	params, err := httputil.ParsePagination(r)
	if err != nil {
		httputil.HandleError(w, err)
		return
	}

	messages, err := h.repo.ListByChannel(r.Context(), ch.ID, params.Cursor, params.Limit+1)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	hasMore := len(messages) > params.Limit
	if hasMore {
		messages = messages[:params.Limit]
	}
	if err := h.repo.Hydrate(r.Context(), messages); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSONList(w, http.StatusOK, messages, nextCursor(messages, createdAt), hasMore)
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	msgID := r.PathValue("message_id")
	if _, ok := h.requireMessageChannel(w, r, msgID); !ok {
		return
	}
	msg, err := h.repo.GetByID(r.Context(), msgID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if msg == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "message not found")
		return
	}
	if err := h.repo.Hydrate(r.Context(), []*Message{msg}); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusOK, msg)
}

func (h *Handler) Edit(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	msgID := r.PathValue("message_id")

	// Membership is checked against the message's own channel: ownership alone
	// let a user who had been removed from a channel keep editing there.
	ch, ok := h.requireMessageChannel(w, r, msgID)
	if !ok {
		return
	}

	msg, err := h.repo.GetForUser(r.Context(), msgID, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if msg == nil {
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
	if err := validateContent(input.Content, nil); err != nil {
		httputil.HandleError(w, err)
		return
	}

	updatedRows, err := h.repo.Update(r.Context(), msgID, userID, input.Content)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !updatedRows {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "message not found")
		return
	}

	updated, err := h.repo.GetForUser(r.Context(), msgID, userID)
	if err != nil || updated == nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if err := h.repo.Hydrate(r.Context(), []*Message{updated}); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	// A message that has not been sent yet must not be announced as an edit.
	if !updated.IsScheduled {
		h.publish(r.Context(), ch.WorkspaceID, evtMessageUpdated, messageVersionKey(updated), updated)
	}
	httputil.JSON(w, http.StatusOK, updated)
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	msgID := r.PathValue("message_id")

	ch, ok := h.requireMessageChannel(w, r, msgID)
	if !ok {
		return
	}

	// Live messages only: a pending scheduled message is cancelled, not
	// deleted, or the worker would promote the emptied row.
	msg, err := h.repo.GetByID(r.Context(), msgID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if msg == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "message not found")
		return
	}
	if msg.UserID != userID {
		moderator, err := h.canModerate(r, ch)
		if err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		if !moderator {
			httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "can only delete your own messages")
			return
		}
	}

	deleted, err := h.repo.SoftDelete(r.Context(), msgID, ch.ID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !deleted {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "message not found")
		return
	}

	h.publish(r.Context(), ch.WorkspaceID, evtMessageDeleted, eventKey(evtMessageDeleted, msgID), map[string]string{
		"id":         msgID,
		"channel_id": ch.ID,
	})

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "deleted"})
}

func (h *Handler) AddReaction(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	msgID := r.PathValue("message_id")

	ch, ok := h.requireMessageChannel(w, r, msgID)
	if !ok {
		return
	}
	if !h.matchesPathChannel(w, r, ch) {
		return
	}

	var input struct {
		Emoji string `json:"emoji"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if err := validateEmoji(input.Emoji); err != nil {
		httputil.HandleError(w, err)
		return
	}

	reaction, inserted, err := h.repo.AddReaction(r.Context(), &Reaction{
		ID:        uuid.NewString(),
		MessageID: msgID,
		UserID:    userID,
		Emoji:     input.Emoji,
	}, ch.ID)
	if errors.Is(err, ErrMessageNotFound) {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "message not found")
		return
	}
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	if inserted {
		h.publish(r.Context(), ch.WorkspaceID, evtReactionAdded, eventKey(evtReactionAdded, msgID, userID, input.Emoji), map[string]string{
			"channel_id": ch.ID,
			"message_id": msgID,
			"user_id":    userID,
			"emoji":      input.Emoji,
		})
	}

	httputil.JSON(w, http.StatusCreated, reaction)
}

func (h *Handler) RemoveReaction(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	msgID := r.PathValue("message_id")
	emoji := r.PathValue("emoji")

	ch, ok := h.requireMessageChannel(w, r, msgID)
	if !ok {
		return
	}
	if !h.matchesPathChannel(w, r, ch) {
		return
	}

	removed, err := h.repo.RemoveReaction(r.Context(), msgID, ch.ID, userID, emoji)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !removed {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "reaction not found")
		return
	}

	h.publish(r.Context(), ch.WorkspaceID, evtReactionRemoved, eventKey(evtReactionRemoved, msgID, userID, emoji), map[string]string{
		"channel_id": ch.ID,
		"message_id": msgID,
		"user_id":    userID,
		"emoji":      emoji,
	})

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "reaction removed"})
}

// matchesPathChannel rejects a request whose {channel_id} is not the channel
// the message actually lives in. Authorization already runs against the
// message's own channel; this keeps the URL from lying about what was touched.
func (h *Handler) matchesPathChannel(w http.ResponseWriter, r *http.Request, ch *authz.ChannelInfo) bool {
	if pathCh := r.PathValue("channel_id"); pathCh != "" && pathCh != ch.ID {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "message not found in this channel")
		return false
	}
	return true
}

func (h *Handler) GetReactions(w http.ResponseWriter, r *http.Request) {
	msgID := r.PathValue("message_id")
	ch, ok := h.requireMessageChannel(w, r, msgID)
	if !ok {
		return
	}
	reactions, err := h.repo.ListReactions(r.Context(), msgID, ch.ID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusOK, reactions)
}

func (h *Handler) ListThread(w http.ResponseWriter, r *http.Request) {
	parentID := r.PathValue("message_id")
	if _, ok := h.requireMessageChannel(w, r, parentID); !ok {
		return
	}
	params, err := httputil.ParsePagination(r)
	if err != nil {
		httputil.HandleError(w, err)
		return
	}

	messages, err := h.repo.ListThread(r.Context(), parentID, params.Cursor, params.Limit+1)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	hasMore := len(messages) > params.Limit
	if hasMore {
		messages = messages[:params.Limit]
	}
	if err := h.repo.Hydrate(r.Context(), messages); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSONList(w, http.StatusOK, messages, nextCursor(messages, createdAt), hasMore)
}

func (h *Handler) ReplyThread(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	parentID := r.PathValue("message_id")

	ch, ok := h.requireMessageChannel(w, r, parentID)
	if !ok {
		return
	}

	var input struct {
		Content string   `json:"content"`
		FileIDs []string `json:"file_ids"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if err := validateContent(input.Content, input.FileIDs); err != nil {
		httputil.HandleError(w, err)
		return
	}
	if err := validateFileIDs(input.FileIDs); err != nil {
		httputil.HandleError(w, err)
		return
	}

	msg := &Message{
		ID:          uuid.NewString(),
		ChannelID:   ch.ID,
		UserID:      userID,
		ParentID:    &parentID,
		Content:     input.Content,
		ContentType: contentTypeMarkdown,
	}
	// Create validates that the parent is a live top-level message of this
	// channel, so a reply cannot target another reply or a foreign thread.
	if err := h.repo.Create(r.Context(), msg, input.FileIDs); err != nil {
		writeWriteError(w, err)
		return
	}

	// Broadcast the parent too — its reply_count changed.
	if parent, err := h.repo.GetByID(r.Context(), parentID); err == nil && parent != nil {
		if err := h.repo.Hydrate(r.Context(), []*Message{parent}); err == nil {
			h.publish(r.Context(), ch.WorkspaceID, evtMessageUpdated, messageVersionKey(parent), parent)
		}
	}

	h.respondWithMessage(w, r, http.StatusCreated, msg.ID, userID, ch.WorkspaceID)
}

func (h *Handler) ListScheduled(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	ch, ok := h.requireChannelMember(w, r, r.PathValue("channel_id"))
	if !ok {
		return
	}
	params, err := httputil.ParsePagination(r)
	if err != nil {
		httputil.HandleError(w, err)
		return
	}

	messages, err := h.repo.ListScheduled(r.Context(), ch.ID, userID, params.Cursor, params.Limit+1)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	hasMore := len(messages) > params.Limit
	if hasMore {
		messages = messages[:params.Limit]
	}
	if err := h.repo.Hydrate(r.Context(), messages); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSONList(w, http.StatusOK, messages, nextCursor(messages, scheduledAt), hasMore)
}

func (h *Handler) CancelScheduled(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	// The route's {channel_id} was previously ignored entirely: cancelling was
	// scoped to (user, is_scheduled) alone, with no membership check at all.
	ch, ok := h.requireChannelMember(w, r, r.PathValue("channel_id"))
	if !ok {
		return
	}
	msgID := r.PathValue("message_id")
	if _, err := uuid.Parse(msgID); err != nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "scheduled message not found")
		return
	}

	canceled, err := h.repo.CancelScheduled(r.Context(), msgID, ch.ID, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !canceled {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "scheduled message not found")
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]string{"message": "canceled"})
}

func (h *Handler) ListPins(w http.ResponseWriter, r *http.Request) {
	ch, ok := h.requireChannelMember(w, r, r.PathValue("channel_id"))
	if !ok {
		return
	}
	params, err := httputil.ParsePagination(r)
	if err != nil {
		httputil.HandleError(w, err)
		return
	}

	messages, err := h.repo.ListPinned(r.Context(), ch.ID, params.Cursor, params.Limit+1)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	hasMore := len(messages) > params.Limit
	if hasMore {
		messages = messages[:params.Limit]
	}
	if err := h.repo.Hydrate(r.Context(), messages); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSONList(w, http.StatusOK, messages, nextCursor(messages, pinnedAt), hasMore)
}

func (h *Handler) Pin(w http.ResponseWriter, r *http.Request) {
	h.setPinned(w, r, true)
}

func (h *Handler) Unpin(w http.ResponseWriter, r *http.Request) {
	h.setPinned(w, r, false)
}

func (h *Handler) setPinned(w http.ResponseWriter, r *http.Request, pinned bool) {
	userID := authctx.UserID(r.Context())
	msgID := r.PathValue("message_id")

	// The message's own channel decides, not the one in the URL — pinning
	// broadcasts the message body, so authorizing against the path channel
	// leaked any message in the deployment into the caller's channel.
	ch, ok := h.requireMessageChannel(w, r, msgID)
	if !ok {
		return
	}
	if !h.matchesPathChannel(w, r, ch) {
		return
	}

	changed, err := h.repo.SetPinned(r.Context(), msgID, ch.ID, userID, pinned)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !changed {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "message not found")
		return
	}

	updated, err := h.repo.GetByID(r.Context(), msgID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if updated != nil {
		// Hydrate before publishing: an un-hydrated copy would wipe reactions
		// and attachments from every other client's view of the message.
		if err := h.repo.Hydrate(r.Context(), []*Message{updated}); err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		h.publish(r.Context(), ch.WorkspaceID, evtMessageUpdated, messageVersionKey(updated), updated)
	}

	if pinned {
		httputil.JSON(w, http.StatusOK, map[string]string{"message": "pinned"})
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]string{"message": "unpinned"})
}

func (h *Handler) Bookmark(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	msgID := r.PathValue("message_id")
	if _, ok := h.requireMessageChannel(w, r, msgID); !ok {
		return
	}

	msg, err := h.repo.GetByID(r.Context(), msgID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if msg == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "message not found")
		return
	}

	if err := h.repo.AddBookmark(r.Context(), userID, msgID); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusCreated, map[string]string{"message": "bookmarked"})
}

func (h *Handler) RemoveBookmark(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	msgID := r.PathValue("message_id")
	if _, err := uuid.Parse(msgID); err != nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "bookmark not found")
		return
	}

	// Deleting is scoped to the caller's own bookmark row, which is the whole
	// authorization: the previous version deleted by message id for anyone.
	removed, err := h.repo.RemoveBookmark(r.Context(), userID, msgID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !removed {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "bookmark not found")
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]string{"message": "removed"})
}

func (h *Handler) ListBookmarks(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	params, err := httputil.ParsePagination(r)
	if err != nil {
		httputil.HandleError(w, err)
		return
	}

	bookmarks, err := h.repo.ListBookmarks(r.Context(), userID, params.Cursor, params.Limit+1)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	hasMore := len(bookmarks) > params.Limit
	if hasMore {
		bookmarks = bookmarks[:params.Limit]
	}

	messages := make([]*Message, 0, len(bookmarks))
	for _, b := range bookmarks {
		messages = append(messages, b.Message)
	}
	if err := h.repo.Hydrate(r.Context(), messages); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	var cursor string
	if len(bookmarks) > 0 {
		last := bookmarks[len(bookmarks)-1]
		cursor = httputil.EncodeCursor(last.BookmarkedAt, last.Message.ID)
	}
	httputil.JSONList(w, http.StatusOK, bookmarks, cursor, hasMore)
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
	source, ok := h.requireMessageChannel(w, r, msgID)
	if !ok {
		return
	}
	if !h.matchesPathChannel(w, r, source) {
		return
	}
	src, err := h.repo.GetByID(r.Context(), msgID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if src == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "message not found")
		return
	}

	// ...and of the TARGET channel (to post into it).
	target, ok := h.requireChannelMember(w, r, input.TargetChannelID)
	if !ok {
		return
	}

	// Attribution travels as structured metadata rather than a "[Forwarded] "
	// prefix, so the copy is distinguishable from something the forwarder
	// wrote and the original attachments stay reachable. Forwarding a forward
	// keeps pointing at the root message.
	origin := src.Metadata.ForwardedFrom
	if origin == nil {
		origin = &ForwardRef{
			MessageID: src.ID,
			ChannelID: src.ChannelID,
			UserID:    src.UserID,
			CreatedAt: src.CreatedAt,
		}
	}

	fwd := &Message{
		ID:          uuid.NewString(),
		ChannelID:   target.ID,
		UserID:      userID,
		Content:     src.Content,
		ContentType: contentTypeMarkdown,
		Metadata:    Metadata{ForwardedFrom: origin},
	}
	if err := h.repo.Create(r.Context(), fwd, nil); err != nil {
		writeWriteError(w, err)
		return
	}

	h.respondWithMessage(w, r, http.StatusCreated, fwd.ID, userID, target.WorkspaceID)
}
