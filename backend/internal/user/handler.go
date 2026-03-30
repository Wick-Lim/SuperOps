package user

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
	mux.Handle("GET /api/v1/users/me", authMw(http.HandlerFunc(h.GetMe)))
	mux.Handle("PATCH /api/v1/users/me", authMw(http.HandlerFunc(h.UpdateMe)))
	mux.Handle("PUT /api/v1/users/me/status", authMw(http.HandlerFunc(h.UpdateStatus)))
	mux.Handle("GET /api/v1/users/{user_id}", authMw(http.HandlerFunc(h.GetUser)))
	mux.Handle("GET /api/v1/users/search", authMw(http.HandlerFunc(h.SearchUsers)))
}

func (h *Handler) UpdateStatus(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	var input struct {
		StatusText  string `json:"status_text"`
		StatusEmoji string `json:"status_emoji"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	_, err := h.repo.Pool().Exec(r.Context(),
		`UPDATE users SET status_text = $2, status_emoji = $3 WHERE id = $1`,
		userID, input.StatusText, input.StatusEmoji,
	)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]string{"status_text": input.StatusText, "status_emoji": input.StatusEmoji})
}

func (h *Handler) GetMe(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	u, err := h.repo.GetByID(r.Context(), userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if u == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "user not found")
		return
	}
	httputil.JSON(w, http.StatusOK, u)
}

func (h *Handler) UpdateMe(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())

	var input struct {
		FullName  *string `json:"full_name"`
		AvatarURL *string `json:"avatar_url"`
		Timezone  *string `json:"timezone"`
		Locale    *string `json:"locale"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	u, err := h.repo.GetByID(r.Context(), userID)
	if err != nil || u == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "user not found")
		return
	}

	if input.FullName != nil {
		u.FullName = *input.FullName
	}
	if input.AvatarURL != nil {
		u.AvatarURL = *input.AvatarURL
	}
	if input.Timezone != nil {
		u.Timezone = *input.Timezone
	}
	if input.Locale != nil {
		u.Locale = *input.Locale
	}

	if err := h.repo.Update(r.Context(), u); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusOK, u)
}

func (h *Handler) GetUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("user_id")
	u, err := h.repo.GetByID(r.Context(), id)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if u == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "user not found")
		return
	}
	httputil.JSON(w, http.StatusOK, u.ToPublic())
}

func (h *Handler) SearchUsers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "query parameter 'q' is required")
		return
	}

	users, err := h.repo.Search(r.Context(), q, 20)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	publicUsers := make([]PublicUser, len(users))
	for i, u := range users {
		publicUsers[i] = u.ToPublic()
	}
	httputil.JSON(w, http.StatusOK, publicUsers)
}
