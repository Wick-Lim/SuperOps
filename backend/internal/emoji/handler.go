package emoji

import (
	"net/http"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

// nameRe enforces lowercase alphanumeric plus underscore/hyphen, non-empty.
var nameRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

type Handler struct {
	repo *Repository
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{repo: NewRepository(pool)}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("GET /api/v1/workspaces/{workspace_id}/emojis", authMw(http.HandlerFunc(h.List)))
	mux.Handle("POST /api/v1/workspaces/{workspace_id}/emojis", authMw(http.HandlerFunc(h.Create)))
	mux.Handle("DELETE /api/v1/workspaces/{workspace_id}/emojis/{emoji_id}", authMw(http.HandlerFunc(h.Delete)))
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	wsID := r.PathValue("workspace_id")

	member, err := h.repo.IsWorkspaceMember(r.Context(), wsID, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !member {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "workspace membership required")
		return
	}

	emojis, err := h.repo.ListByWorkspace(r.Context(), wsID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if emojis == nil {
		emojis = []*Emoji{}
	}
	httputil.JSON(w, http.StatusOK, emojis)
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	wsID := r.PathValue("workspace_id")

	member, err := h.repo.IsWorkspaceMember(r.Context(), wsID, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !member {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "workspace membership required")
		return
	}

	var input struct {
		Name     string `json:"name"`
		ImageURL string `json:"image_url"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if input.Name == "" || input.ImageURL == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "name and image_url are required")
		return
	}
	if !nameRe.MatchString(input.Name) {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "name must be lowercase alphanumeric, underscore, or hyphen")
		return
	}

	exists, err := h.repo.ExistsByName(r.Context(), wsID, input.Name)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if exists {
		httputil.JSONError(w, http.StatusConflict, "CONFLICT", "emoji name already exists in this workspace")
		return
	}

	e := &Emoji{
		ID:          uuid.NewString(),
		WorkspaceID: wsID,
		Name:        input.Name,
		ImageURL:    input.ImageURL,
		CreatedBy:   userID,
	}
	if err := h.repo.Create(r.Context(), e); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	created, err := h.repo.GetByID(r.Context(), e.ID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if created == nil {
		created = e
	}
	httputil.JSON(w, http.StatusCreated, created)
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	wsID := r.PathValue("workspace_id")
	emojiID := r.PathValue("emoji_id")

	e, err := h.repo.GetByID(r.Context(), emojiID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if e == nil || e.WorkspaceID != wsID {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "emoji not found")
		return
	}

	if e.CreatedBy != userID {
		admin, err := h.repo.IsWorkspaceAdmin(r.Context(), wsID, userID)
		if err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		if !admin {
			httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "only the creator or a workspace owner/admin can delete this emoji")
			return
		}
	}

	if err := h.repo.Delete(r.Context(), emojiID); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]string{"message": "deleted"})
}
