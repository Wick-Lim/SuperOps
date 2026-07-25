package notification

import (
	"net/http"

	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

type Handler struct {
	repo *Repository
}

func NewHandler(repo *Repository) *Handler {
	return &Handler{repo: repo}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("GET /api/v1/notifications", authMw(http.HandlerFunc(h.List)))
	mux.Handle("PUT /api/v1/notifications/{notification_id}/read", authMw(http.HandlerFunc(h.MarkRead)))
	mux.Handle("PUT /api/v1/notifications/read-all", authMw(http.HandlerFunc(h.MarkAllRead)))
	mux.Handle("GET /api/v1/notifications/unread-count", authMw(http.HandlerFunc(h.UnreadCount)))
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	params, err := httputil.ParsePagination(r)
	if err != nil {
		httputil.HandleError(w, err)
		return
	}

	notifications, err := h.repo.ListByUser(r.Context(), userID, params.Cursor, params.Limit+1)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	hasMore := len(notifications) > params.Limit
	if hasMore {
		notifications = notifications[:params.Limit]
	}
	if notifications == nil {
		notifications = []*Notification{}
	}

	var cursor string
	if len(notifications) > 0 {
		last := notifications[len(notifications)-1]
		cursor = httputil.EncodeCursor(last.CreatedAt, last.ID)
	}
	httputil.JSONList(w, http.StatusOK, notifications, cursor, hasMore)
}

func (h *Handler) MarkRead(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	nID := r.PathValue("notification_id")

	ok, err := h.repo.MarkRead(r.Context(), nID, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !ok {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "notification not found")
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]string{"message": "marked as read"})
}

func (h *Handler) MarkAllRead(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())

	updated, err := h.repo.MarkAllRead(r.Context(), userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]interface{}{
		"message": "all marked as read",
		"updated": updated,
	})
}

func (h *Handler) UnreadCount(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())

	count, err := h.repo.UnreadCount(r.Context(), userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]int{"count": count})
}
