package inbox

import (
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"

	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

type Handler struct {
	repo *Repository
}

func NewHandler(repo *Repository) *Handler { return &Handler{repo: repo} }

// RegisterRoutes mounts the inbox surface.
//
// Authorization for an inbox item is ownership and nothing else: there is no
// sharing model, no admin read and no workspace-admin override, because an
// inbox item is a record of what one person has been told. Every mutation below
// carries `WHERE id = $1 AND user_id = $2` in its UPDATE rather than doing a
// read-then-check, so there is no window between the two and no second query to
// forget.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("GET /api/v1/inbox", authMw(http.HandlerFunc(h.List)))
	mux.Handle("GET /api/v1/inbox/count", authMw(http.HandlerFunc(h.Count)))
	mux.Handle("POST /api/v1/inbox/read-all", authMw(http.HandlerFunc(h.ReadAll)))
	mux.Handle("GET /api/v1/inbox/{item_id}/events", authMw(http.HandlerFunc(h.Events)))
	mux.Handle("PUT /api/v1/inbox/{item_id}/read", authMw(h.setState(StateRead)))
	mux.Handle("PUT /api/v1/inbox/{item_id}/unread", authMw(h.setState(StateUnread)))
	mux.Handle("PUT /api/v1/inbox/{item_id}/done", authMw(h.setState(StateDone)))
	mux.Handle("PUT /api/v1/inbox/{item_id}/undone", authMw(h.setState(StateRead)))

	mux.Handle("GET /api/v1/notification-prefs", authMw(http.HandlerFunc(h.ListPrefs)))
	mux.Handle("PUT /api/v1/notification-prefs", authMw(http.HandlerFunc(h.ReplacePrefs)))
}

// RegisterCompatRoutes mounts the four shipped /api/v1/notifications routes over
// the inbox. See compat.go for what changes meaning and why it is not versioned.
func (h *Handler) RegisterCompatRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("GET /api/v1/notifications", authMw(http.HandlerFunc(h.LegacyList)))
	mux.Handle("PUT /api/v1/notifications/{notification_id}/read", authMw(http.HandlerFunc(h.LegacyMarkRead)))
	mux.Handle("PUT /api/v1/notifications/read-all", authMw(http.HandlerFunc(h.LegacyMarkAllRead)))
	mux.Handle("GET /api/v1/notifications/unread-count", authMw(http.HandlerFunc(h.LegacyUnreadCount)))
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	items, cursor, hasMore, ok := h.list(w, r)
	if !ok {
		return
	}
	httputil.JSONList(w, http.StatusOK, items, cursor, hasMore)
}

// list is the shared body of List and LegacyList: same query, two projections.
func (h *Handler) list(w http.ResponseWriter, r *http.Request) ([]*Item, string, bool, bool) {
	userID := authctx.UserID(r.Context())
	params, err := httputil.ParsePagination(r)
	if err != nil {
		httputil.HandleError(w, err)
		return nil, "", false, false
	}

	f := ListFilter{
		UserID:      userID,
		State:       httputil.QueryParam(r, "state"),
		WorkspaceID: httputil.QueryParam(r, "workspace_id"),
		Kind:        httputil.QueryParam(r, "kind"),
	}
	if f.WorkspaceID != "" {
		if _, err := uuid.Parse(f.WorkspaceID); err != nil {
			httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "workspace_id must be a uuid")
			return nil, "", false, false
		}
	}
	if f.Kind != "" && !ValidKind(f.Kind) {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "kind must be '<resource>.<verb>'")
		return nil, "", false, false
	}

	items, err := h.repo.ListItems(r.Context(), f, params.Cursor, params.Limit+1)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return nil, "", false, false
	}

	hasMore := len(items) > params.Limit
	if hasMore {
		items = items[:params.Limit]
	}
	var cursor string
	if len(items) > 0 {
		last := items[len(items)-1]
		cursor = httputil.EncodeCursor(last.LastAt, last.ID)
	}
	return items, cursor, hasMore, true
}

// Count is the badge, plus the per-workspace breakdown the shell's switcher
// needs. `unread` is COUNT(*) over unread ITEMS — the one definition, see the
// package comment.
func (h *Handler) Count(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())

	total, err := h.repo.UnreadItems(r.Context(), userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	byWorkspace, err := h.repo.WorkspaceUnread(r.Context(), userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	list := make([]map[string]any, 0, len(byWorkspace))
	for ws, n := range byWorkspace {
		list = append(list, map[string]any{"workspace_id": ws, "unread": n})
	}
	httputil.JSON(w, http.StatusOK, map[string]any{"unread": total, "by_workspace": list})
}

func (h *Handler) Events(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	params, err := httputil.ParsePagination(r)
	if err != nil {
		httputil.HandleError(w, err)
		return
	}

	// Two distinct failures that must not collapse: a database error is a 500,
	// a missing (or someone else's) item is a 404.
	item, err := h.repo.GetItem(r.Context(), r.PathValue("item_id"), userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if item == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "inbox item not found")
		return
	}

	events, err := h.repo.ListEvents(r.Context(), item, params.Cursor, params.Limit+1)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	hasMore := len(events) > params.Limit
	if hasMore {
		events = events[:params.Limit]
	}
	var cursor string
	if len(events) > 0 {
		last := events[len(events)-1]
		cursor = httputil.EncodeCursor(last.CreatedAt, last.ID)
	}
	httputil.JSONList(w, http.StatusOK, events, cursor, hasMore)
}

func (h *Handler) setState(state string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := authctx.UserID(r.Context())
		ok, err := h.repo.SetState(r.Context(), r.PathValue("item_id"), userID, state)
		if err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		if !ok {
			httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "inbox item not found")
			return
		}
		httputil.JSON(w, http.StatusOK, map[string]string{"state": state})
	})
}

func (h *Handler) ReadAll(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())

	var input struct {
		WorkspaceID string `json:"workspace_id"`
		Kind        string `json:"kind"`
	}
	// The body is optional: "mark everything read" is the common case.
	if err := httputil.DecodeJSON(r, &input); err != nil && !errors.Is(err, io.EOF) {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if input.WorkspaceID != "" {
		if _, err := uuid.Parse(input.WorkspaceID); err != nil {
			httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "workspace_id must be a uuid")
			return
		}
	}
	if input.Kind != "" && !ValidKind(input.Kind) {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "kind must be '<resource>.<verb>'")
		return
	}

	updated, err := h.repo.ReadAll(r.Context(), userID, input.WorkspaceID, input.Kind)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]any{"updated": updated})
}

// --- preferences -------------------------------------------------------------

func (h *Handler) ListPrefs(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	workspaceID := httputil.QueryParam(r, "workspace_id")
	if _, err := uuid.Parse(workspaceID); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "workspace_id is required")
		return
	}

	prefs, err := h.repo.ListPrefs(r.Context(), userID, workspaceID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]any{
		"workspace_id": workspaceID,
		"prefs":        prefs,
		// The ladder's last rung, sent so the prefs screen can show what "no row"
		// resolves to without hard-coding this package's default.
		"default": DefaultPref,
		// Turning off a kind does not retract items already filed under it.
		// Correct, and users read it as a bug unless the screen says so.
		"note": "changing a preference applies to future events only; items already in the inbox stay",
	})
}

func (h *Handler) ReplacePrefs(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())

	var input struct {
		WorkspaceID string    `json:"workspace_id"`
		Prefs       []PrefRow `json:"prefs"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if _, err := uuid.Parse(input.WorkspaceID); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "workspace_id is required")
		return
	}
	// Validated here, in Go, so the caller gets a 400 naming the bad value
	// rather than a 500 carrying a CHECK constraint name.
	for _, p := range input.Prefs {
		if !ValidPrefKind(p.Kind) {
			httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST",
				"kind must be '<resource>.<verb>', '<resource>.*' or '*': "+p.Kind)
			return
		}
		if !ValidEmailMode(p.Email) {
			httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST",
				"email must be never, digest or immediate: "+p.Email)
			return
		}
	}

	if err := h.repo.ReplacePrefs(r.Context(), userID, input.WorkspaceID, input.Prefs); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	prefs, err := h.repo.ListPrefs(r.Context(), userID, input.WorkspaceID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]any{"workspace_id": input.WorkspaceID, "prefs": prefs})
}

// --- compatibility -----------------------------------------------------------

func (h *Handler) LegacyList(w http.ResponseWriter, r *http.Request) {
	items, cursor, hasMore, ok := h.list(w, r)
	if !ok {
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, legacyProjection(it))
	}
	httputil.JSONList(w, http.StatusOK, out, cursor, hasMore)
}

func (h *Handler) LegacyMarkRead(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	ok, err := h.repo.SetState(r.Context(), r.PathValue("notification_id"), userID, StateRead)
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

func (h *Handler) LegacyMarkAllRead(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	updated, err := h.repo.ReadAll(r.Context(), userID, "", "")
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]any{
		"message": "all marked as read",
		"updated": updated,
	})
}

// LegacyUnreadCount is the one route whose MEANING changed: unread items, not
// unread events. See compat.go.
func (h *Handler) LegacyUnreadCount(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	count, err := h.repo.UnreadItems(r.Context(), userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]int{"count": count})
}
