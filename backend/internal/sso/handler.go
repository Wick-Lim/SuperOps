package sso

import (
	"errors"
	"net/http"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

type Handler struct {
	service *Service
	az      *authz.Checker
}

func NewHandler(service *Service, az *authz.Checker) *Handler {
	return &Handler{service: service, az: az}
}

// ssoErrorResponses maps this package's caller-fault sentinels onto responses.
// Anything absent from the table is an internal fault and is masked by
// writeError — a provider's OAuth error text, a Postgres constraint or a
// decryption failure must never reach an unauthenticated caller.
var ssoErrorResponses = []struct {
	err     error
	status  int
	code    string
	message string // defaults to err.Error()
}{
	{ErrProviderNotFound, http.StatusNotFound, "SSO_NOT_CONFIGURED", ""},
	{ErrProviderDisabled, http.StatusNotFound, "SSO_NOT_CONFIGURED", ""},
	{ErrNotConfigured, http.StatusNotImplemented, "SSO_UNAVAILABLE", ""},

	{ErrStateInvalid, http.StatusBadRequest, "SSO_STATE_INVALID", ""},
	// 401 and not 500: the assertion is a credential and it did not check out.
	// The reason it did not is in the log, never in the body.
	{ErrAssertionInvalid, http.StatusUnauthorized, "SSO_ASSERTION_INVALID", ""},
	{ErrExchangeFailed, http.StatusUnauthorized, "SSO_EXCHANGE_FAILED", ""},
	{ErrPendingInvalid, http.StatusBadRequest, "SSO_CHALLENGE_INVALID", ""},

	{ErrEmailMissing, http.StatusBadRequest, "SSO_EMAIL_MISSING", ""},
	{ErrEmailNotVerified, http.StatusForbidden, "SSO_EMAIL_UNVERIFIED", ""},
	{ErrEmailAmbiguous, http.StatusConflict, "SSO_EMAIL_AMBIGUOUS", ""},

	{ErrLinkNotAllowed, http.StatusForbidden, "SSO_LINK_NOT_ALLOWED", ""},
	{ErrJITNotAllowed, http.StatusForbidden, "SSO_PROVISIONING_DISABLED", ""},
	{ErrIdentityTaken, http.StatusConflict, "SSO_IDENTITY_TAKEN", ""},

	// Deliberately the same shape as a failed password login: an SSO callback
	// must not be usable to discover that an address belongs to a disabled
	// account.
	{ErrAccountInactive, http.StatusUnauthorized, "UNAUTHORIZED", "invalid credentials"},
	{ErrInvalidLocalCredential, http.StatusUnauthorized, "INVALID_PASSWORD", ""},
	{ErrSecondFactorRequired, http.StatusUnauthorized, "TOTP_REQUIRED", ""},
	{ErrSecondFactorInvalid, http.StatusUnauthorized, "INVALID_TOTP_CODE", ""},

	{ErrEnforceNeedsIdentity, http.StatusConflict, "SSO_ENFORCE_NEEDS_IDENTITY", ""},
}

func writeError(w http.ResponseWriter, err error) {
	for _, m := range ssoErrorResponses {
		if errors.Is(err, m.err) {
			msg := m.message
			if msg == "" {
				msg = m.err.Error()
			}
			httputil.JSONError(w, m.status, m.code, msg)
			return
		}
	}
	// httputil.AppError carries its own status: the configuration validators
	// return one so an administrator sees which field is wrong.
	var appErr *httputil.AppError
	if errors.As(err, &appErr) {
		httputil.HandleError(w, appErr)
		return
	}
	httputil.HandleError(w, httputil.NewInternal(err))
}

// RegisterRoutes registers the unauthenticated sign-in endpoints. limiter (may
// be nil) wraps all of them: every one either triggers an outbound request to
// a provider or accepts a guessable token.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, limiter func(http.Handler) http.Handler) {
	if limiter == nil {
		limiter = func(n http.Handler) http.Handler { return n }
	}
	mux.Handle("GET /api/v1/auth/sso/workspace/{workspace_slug}", limiter(http.HandlerFunc(h.Info)))
	mux.Handle("POST /api/v1/auth/sso/start", limiter(http.HandlerFunc(h.Start)))
	mux.Handle("POST /api/v1/auth/sso/callback", limiter(http.HandlerFunc(h.Callback)))
	mux.Handle("POST /api/v1/auth/sso/totp", limiter(http.HandlerFunc(h.CompleteTOTP)))
	mux.Handle("POST /api/v1/auth/sso/link", limiter(http.HandlerFunc(h.CompleteLink)))
}

// RegisterProtectedRoutes registers the per-workspace administration endpoints.
func (h *Handler) RegisterProtectedRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("GET /api/v1/workspaces/{workspace_id}/sso", authMw(http.HandlerFunc(h.Get)))
	mux.Handle("PUT /api/v1/workspaces/{workspace_id}/sso", authMw(http.HandlerFunc(h.Save)))
	mux.Handle("DELETE /api/v1/workspaces/{workspace_id}/sso", authMw(http.HandlerFunc(h.Delete)))
	mux.Handle("POST /api/v1/workspaces/{workspace_id}/sso/enforcement", authMw(http.HandlerFunc(h.SetEnforcement)))
	mux.Handle("POST /api/v1/workspaces/{workspace_id}/sso/verify", authMw(http.HandlerFunc(h.Verify)))
}

// ---------------------------------------------------------------------------
// Sign-in
// ---------------------------------------------------------------------------

// Info tells a sign-in page whether to offer the SSO button and whether to
// still offer a password field.
func (h *Handler) Info(w http.ResponseWriter, r *http.Request) {
	info, err := h.service.PublicInfo(r.Context(), r.PathValue("workspace_slug"))
	if err != nil {
		writeError(w, err)
		return
	}
	httputil.JSON(w, http.StatusOK, info)
}

func (h *Handler) Start(w http.ResponseWriter, r *http.Request) {
	var input struct {
		WorkspaceSlug string `json:"workspace_slug"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if input.WorkspaceSlug == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "workspace_slug is required")
		return
	}

	out, err := h.service.Start(r.Context(), input.WorkspaceSlug)
	if err != nil {
		writeError(w, err)
		return
	}
	httputil.JSON(w, http.StatusOK, out)
}

func (h *Handler) Callback(w http.ResponseWriter, r *http.Request) {
	var input struct {
		State string `json:"state"`
		Code  string `json:"code"`
		// Error/ErrorDescription are what the provider sends back when the user
		// declines or the request was rejected. They are accepted so the body
		// still decodes (DecodeJSON rejects unknown fields) and are never
		// echoed.
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if input.Error != "" {
		httputil.JSONError(w, http.StatusUnauthorized, "SSO_DENIED", "the identity provider did not complete the sign-in")
		return
	}

	result, err := h.service.Callback(r.Context(), input.State, input.Code, r.UserAgent(), r.RemoteAddr)
	if err != nil {
		writeError(w, err)
		return
	}
	writeLoginResult(w, result)
}

func (h *Handler) CompleteTOTP(w http.ResponseWriter, r *http.Request) {
	var input struct {
		PendingToken string `json:"pending_token"`
		Code         string `json:"code"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if input.PendingToken == "" || input.Code == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "pending_token and code are required")
		return
	}

	result, err := h.service.CompleteSecondFactor(r.Context(), input.PendingToken, input.Code, r.UserAgent(), r.RemoteAddr)
	if err != nil {
		writeError(w, err)
		return
	}
	writeLoginResult(w, result)
}

func (h *Handler) CompleteLink(w http.ResponseWriter, r *http.Request) {
	var input struct {
		PendingToken string `json:"pending_token"`
		Password     string `json:"password"`
		TOTPCode     string `json:"totp_code"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if input.PendingToken == "" || input.Password == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "pending_token and password are required")
		return
	}

	result, err := h.service.CompleteLink(r.Context(), input.PendingToken, input.Password, input.TOTPCode, r.UserAgent(), r.RemoteAddr)
	if err != nil {
		writeError(w, err)
		return
	}
	writeLoginResult(w, result)
}

// writeLoginResult renders the three outcomes.
//
// A challenge is a 200 carrying a step, not an error: the assertion succeeded
// and the client has a well-defined next call to make. An error status would
// tell a client that the sign-in failed, which it did not.
func writeLoginResult(w http.ResponseWriter, result *LoginResult) {
	if result.Tokens != nil {
		httputil.JSON(w, http.StatusOK, result.Tokens)
		return
	}
	httputil.JSON(w, http.StatusOK, result)
}

// ---------------------------------------------------------------------------
// Administration
// ---------------------------------------------------------------------------

// requireAdmin gates every configuration endpoint. A non-admin member gets 404
// rather than 403 so that provider configuration is not probeable by anyone who
// happens to be in the workspace.
func (h *Handler) requireAdmin(w http.ResponseWriter, r *http.Request) (string, bool) {
	workspaceID := r.PathValue("workspace_id")
	ok, err := h.az.IsWorkspaceAdmin(r.Context(), workspaceID, authctx.UserID(r.Context()))
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return "", false
	}
	if !ok {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "workspace not found")
		return "", false
	}
	return workspaceID, true
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.requireAdmin(w, r)
	if !ok {
		return
	}
	p, err := h.service.GetProvider(r.Context(), workspaceID)
	if err != nil {
		writeError(w, err)
		return
	}
	httputil.JSON(w, http.StatusOK, p.View())
}

func (h *Handler) Save(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.requireAdmin(w, r)
	if !ok {
		return
	}

	var input SaveInput
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	p, err := h.service.SaveProvider(r.Context(), workspaceID, authctx.UserID(r.Context()), input, r.RemoteAddr)
	if err != nil {
		writeError(w, err)
		return
	}
	httputil.JSON(w, http.StatusOK, p.View())
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.requireAdmin(w, r)
	if !ok {
		return
	}
	if err := h.service.DeleteProvider(r.Context(), workspaceID, authctx.UserID(r.Context()), r.RemoteAddr); err != nil {
		writeError(w, err)
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]string{"message": "single sign-on removed"})
}

func (h *Handler) SetEnforcement(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.requireAdmin(w, r)
	if !ok {
		return
	}

	var input struct {
		Enforced bool `json:"enforced"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	p, err := h.service.SetEnforced(r.Context(), workspaceID, authctx.UserID(r.Context()), input.Enforced, r.RemoteAddr)
	if err != nil {
		writeError(w, err)
		return
	}
	httputil.JSON(w, http.StatusOK, p.View())
}

func (h *Handler) Verify(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := h.requireAdmin(w, r)
	if !ok {
		return
	}
	out, err := h.service.Verify(r.Context(), workspaceID)
	if err != nil {
		writeError(w, err)
		return
	}
	httputil.JSON(w, http.StatusOK, out)
}
