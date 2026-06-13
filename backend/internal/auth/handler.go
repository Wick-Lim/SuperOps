package auth

import (
	"net/http"

	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// RegisterRoutes registers the public auth endpoints. limiter (may be nil)
// wraps the brute-forceable endpoints (login, refresh, accept-invite).
func (h *Handler) RegisterRoutes(mux *http.ServeMux, limiter func(http.Handler) http.Handler) {
	if limiter == nil {
		limiter = func(n http.Handler) http.Handler { return n }
	}
	mux.Handle("POST /api/v1/auth/login", limiter(http.HandlerFunc(h.Login)))
	mux.Handle("POST /api/v1/auth/refresh", limiter(http.HandlerFunc(h.Refresh)))
	mux.Handle("POST /api/v1/auth/logout", http.HandlerFunc(h.Logout))
	mux.Handle("POST /api/v1/auth/accept-invite", limiter(http.HandlerFunc(h.AcceptInvite)))
	mux.Handle("GET /api/v1/auth/invite/{token}", limiter(http.HandlerFunc(h.GetInviteInfo)))
}

// RegisterProtectedRoutes registers auth endpoints that require an authenticated session.
func (h *Handler) RegisterProtectedRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("POST /api/v1/auth/change-password", authMw(http.HandlerFunc(h.ChangePassword)))
}

func (h *Handler) ChangePassword(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())

	var input struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if len(input.NewPassword) < 8 {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "new password must be at least 8 characters")
		return
	}

	if err := h.service.ChangePassword(r.Context(), userID, input.OldPassword, input.NewPassword); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]string{"message": "password changed"})
}

func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var input LoginInput
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	if input.Email == "" || input.Password == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "email and password are required")
		return
	}

	tokens, err := h.service.Login(r.Context(), input, r.UserAgent(), r.RemoteAddr)
	if err != nil {
		httputil.JSONError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
		return
	}

	httputil.JSON(w, http.StatusOK, tokens)
}

func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	var input struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	if input.RefreshToken == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "refresh_token is required")
		return
	}

	tokens, err := h.service.RefreshTokens(r.Context(), input.RefreshToken, r.UserAgent(), r.RemoteAddr)
	if err != nil {
		httputil.JSONError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
		return
	}

	httputil.JSON(w, http.StatusOK, tokens)
}

func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	var input struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	if err := h.service.Logout(r.Context(), input.RefreshToken); err != nil {
		httputil.JSONError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "logout failed")
		return
	}

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "logged out"})
}

func (h *Handler) AcceptInvite(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Token    string `json:"token"`
		Username string `json:"username"`
		Password string `json:"password"`
		FullName string `json:"full_name"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	if input.Token == "" || input.Username == "" || input.Password == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "token, username, and password are required")
		return
	}
	if len(input.Password) < 8 {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "password must be at least 8 characters")
		return
	}

	tokens, err := h.service.AcceptInvite(r.Context(), input.Token, input.Username, input.Password, input.FullName, r.UserAgent(), r.RemoteAddr)
	if err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}

	httputil.JSON(w, http.StatusCreated, tokens)
}

func (h *Handler) GetInviteInfo(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	info, err := h.service.GetInviteInfo(r.Context(), token)
	if err != nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	httputil.JSON(w, http.StatusOK, info)
}
