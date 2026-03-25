package auth

import (
	"net/http"

	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/auth/register", h.Register)
	mux.HandleFunc("POST /api/v1/auth/login", h.Login)
	mux.HandleFunc("POST /api/v1/auth/refresh", h.Refresh)
	mux.HandleFunc("POST /api/v1/auth/logout", h.Logout)
}

func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var input RegisterInput
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	if input.Email == "" || input.Username == "" || input.Password == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "email, username, and password are required")
		return
	}

	if len(input.Password) < 8 {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "password must be at least 8 characters")
		return
	}

	u, err := h.service.Register(r.Context(), input)
	if err != nil {
		httputil.JSONError(w, http.StatusConflict, "CONFLICT", err.Error())
		return
	}

	httputil.JSON(w, http.StatusCreated, map[string]interface{}{
		"id":       u.ID,
		"email":    u.Email,
		"username": u.Username,
	})
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
