package emoji

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

// nameRe enforces lowercase alphanumeric plus underscore/hyphen, non-empty.
var nameRe = regexp.MustCompile(`^[a-z0-9_-]+$`)

const (
	// maxNameLen mirrors custom_emojis_name_len (migration 009).
	maxNameLen = 64
	// maxImageURLLen mirrors custom_emojis_url_len (migration 009).
	maxImageURLLen = 2048
)

// uniqueViolation is the Postgres SQLSTATE for a unique-constraint breach.
const uniqueViolation = "23505"

type Handler struct {
	repo  *Repository
	authz *authz.Checker
}

func NewHandler(pool *pgxpool.Pool, az *authz.Checker) *Handler {
	return &Handler{repo: NewRepository(pool), authz: az}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("GET /api/v1/workspaces/{workspace_id}/emojis", authMw(http.HandlerFunc(h.List)))
	mux.Handle("POST /api/v1/workspaces/{workspace_id}/emojis", authMw(http.HandlerFunc(h.Create)))
	mux.Handle("DELETE /api/v1/workspaces/{workspace_id}/emojis/{emoji_id}", authMw(http.HandlerFunc(h.Delete)))
}

// validateImageURL rejects anything that is not a plain absolute http(s) URL.
// The value is stored verbatim and every member's client loads it, so an
// unvalidated `javascript:` or `data:` URL is script execution in whatever
// renders the emoji.
func validateImageURL(raw string) error {
	if raw == "" {
		return errors.New("image_url is required")
	}
	if len(raw) > maxImageURLLen {
		return errors.New("image_url is too long")
	}
	if strings.ContainsAny(raw, "\r\n\t") || strings.ContainsRune(raw, 0) {
		return errors.New("image_url must not contain control characters")
	}

	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("image_url must be a valid URL")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return errors.New("image_url must be an http or https URL")
	}
	if u.Host == "" {
		return errors.New("image_url must include a host")
	}
	return nil
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	wsID := r.PathValue("workspace_id")

	member, err := h.authz.Can(r.Context(), authz.UserSubject(userID), authz.WorkspaceObject(wsID), authz.CapRead)
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

	member, err := h.authz.Can(r.Context(), authz.UserSubject(userID), authz.WorkspaceObject(wsID), authz.CapRead)
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
	if input.Name == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "name is required")
		return
	}
	if !nameRe.MatchString(input.Name) {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "name must be lowercase alphanumeric, underscore, or hyphen")
		return
	}
	if len(input.Name) > maxNameLen {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "name must be at most 64 characters")
		return
	}
	if err := validateImageURL(input.ImageURL); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}

	e := &Emoji{
		ID:          uuid.NewString(),
		WorkspaceID: wsID,
		Name:        input.Name,
		ImageURL:    input.ImageURL,
		CreatedBy:   userID,
	}
	// The UNIQUE (workspace_id, name) constraint is the authority. A prior
	// ExistsByName probe only narrowed the race window and turned the loser of
	// it into a 500 instead of the 409 the non-racing path returns.
	if err := h.repo.Create(r.Context(), e); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			httputil.JSONError(w, http.StatusConflict, "CONFLICT", "emoji name already exists in this workspace")
			return
		}
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
		admin, err := h.authz.Can(r.Context(), authz.UserSubject(userID), authz.WorkspaceObject(wsID), authz.CapAdmin)
		if err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		if !admin {
			httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "only the creator or a workspace owner/admin can delete this emoji")
			return
		}
	}

	deleted, err := h.repo.Delete(r.Context(), emojiID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !deleted {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "emoji not found")
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]string{"message": "deleted"})
}
