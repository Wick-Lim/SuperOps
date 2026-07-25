package auth

import (
	"errors"
	"io"
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

// authErrorResponses maps the service's caller-fault sentinels onto client
// responses. An error that is not in this table is an internal fault: it is
// masked by writeAuthError rather than echoed, so Postgres constraint text
// never reaches an unauthenticated caller.
var authErrorResponses = []struct {
	err     error
	status  int
	code    string
	message string // defaults to err.Error()
}{
	{ErrTOTPRequired, http.StatusUnauthorized, "TOTP_REQUIRED", "two-factor code required"},
	{ErrInvalidCredentials, http.StatusUnauthorized, "UNAUTHORIZED", ""},
	{ErrInvalidTOTPCode, http.StatusUnauthorized, "INVALID_TOTP_CODE", ""},
	{ErrInvalidRefreshToken, http.StatusUnauthorized, "UNAUTHORIZED", ""},
	{ErrReauthRequired, http.StatusUnauthorized, "REAUTH_REQUIRED", ""},
	{ErrInviteWrongAccount, http.StatusForbidden, "INVITE_WRONG_ACCOUNT", ""},
	{ErrUsernameTaken, http.StatusConflict, "USERNAME_TAKEN", ""},
	{ErrCurrentPassword, http.StatusBadRequest, "INVALID_PASSWORD", ""},
	{ErrPasswordTooShort, http.StatusBadRequest, "BAD_REQUEST", ""},
	{ErrPasswordTooLong, http.StatusBadRequest, "BAD_REQUEST", ""},
	{ErrSignupFieldsMissing, http.StatusBadRequest, "BAD_REQUEST", ""},
	{ErrTOTPSetupMissing, http.StatusBadRequest, "TOTP_SETUP_REQUIRED", ""},
	{ErrInviteInvalid, http.StatusBadRequest, "INVITE_INVALID", ""},
	{ErrInviteExpired, http.StatusBadRequest, "INVITE_EXPIRED", ""},
}

func writeAuthError(w http.ResponseWriter, err error) {
	for _, m := range authErrorResponses {
		if errors.Is(err, m.err) {
			msg := m.message
			if msg == "" {
				msg = m.err.Error()
			}
			httputil.JSONError(w, m.status, m.code, msg)
			return
		}
	}
	httputil.HandleError(w, httputil.NewInternal(err))
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
	mux.Handle("GET /api/v1/auth/totp/status", authMw(http.HandlerFunc(h.TOTPStatus)))
	mux.Handle("POST /api/v1/auth/totp/setup", authMw(http.HandlerFunc(h.TOTPSetup)))
	mux.Handle("POST /api/v1/auth/totp/verify", authMw(http.HandlerFunc(h.TOTPVerify)))
	mux.Handle("POST /api/v1/auth/totp/disable", authMw(http.HandlerFunc(h.TOTPDisable)))
}

func (h *Handler) TOTPStatus(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	httputil.JSON(w, http.StatusOK, map[string]bool{"enabled": h.service.TOTPEnabled(r.Context(), userID)})
}

func (h *Handler) TOTPSetup(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())

	// Re-enrolling while 2FA is already on requires proof of possession of a
	// second factor or the password; first-time enrolment needs neither, so an
	// empty body stays valid.
	var input struct {
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil && !errors.Is(err, io.EOF) {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	setup, err := h.service.SetupTOTP(r.Context(), userID, input.Password, input.Code, r.RemoteAddr)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	httputil.JSON(w, http.StatusOK, setup)
}

func (h *Handler) TOTPVerify(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	var input struct {
		Code string `json:"code"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil || input.Code == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "code is required")
		return
	}
	codes, err := h.service.EnableTOTP(r.Context(), userID, input.Code, r.RemoteAddr)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]interface{}{"enabled": true, "backup_codes": codes})
}

func (h *Handler) TOTPDisable(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	var input struct {
		Code string `json:"code"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil || input.Code == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "code is required")
		return
	}
	if err := h.service.DisableTOTP(r.Context(), userID, input.Code, r.RemoteAddr); err != nil {
		writeAuthError(w, err)
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]bool{"enabled": false})
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

	if err := h.service.ChangePassword(r.Context(), userID, input.OldPassword, input.NewPassword, r.RemoteAddr); err != nil {
		writeAuthError(w, err)
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
		writeAuthError(w, err)
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
		writeAuthError(w, err)
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

	if err := h.service.Logout(r.Context(), input.RefreshToken, r.RemoteAddr); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
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

	if input.Token == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "token is required")
		return
	}

	// The route is public, but a signed-in client may still hit it: the bearer
	// token, when present, proves the caller owns an already-registered
	// invitee address without asking for the password again.
	tokens, err := h.service.AcceptInvite(r.Context(), AcceptInviteInput{
		Token:       input.Token,
		Username:    input.Username,
		Password:    input.Password,
		FullName:    input.FullName,
		BearerToken: bearerToken(r),
		UserAgent:   r.UserAgent(),
		IPAddress:   r.RemoteAddr,
	})
	if err != nil {
		writeAuthError(w, err)
		return
	}

	httputil.JSON(w, http.StatusCreated, tokens)
}

func (h *Handler) GetInviteInfo(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	info, err := h.service.GetInviteInfo(r.Context(), token)
	if err != nil {
		if errors.Is(err, ErrInviteInvalid) || errors.Is(err, ErrInviteExpired) {
			httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
			return
		}
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusOK, info)
}
