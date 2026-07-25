package user

import (
	"net/http"
	"unicode/utf8"

	"github.com/Wick-Lim/SuperOps/backend/internal/push"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

const searchLimit = 20

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

// RegisterDeviceRoutes mounts push-token registration.
//
// It is separate from RegisterRoutes because app.New only calls it when
// PUSH_ENABLED is on. Storing device tokens that nothing will ever send to is
// collecting an identifier for no purpose, and the client treats the resulting
// 404 as "this deployment has no push" — see app/src/lib/push.ts.
func (h *Handler) RegisterDeviceRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("POST /api/v1/users/me/devices", authMw(http.HandlerFunc(h.RegisterDevice)))
	mux.Handle("DELETE /api/v1/users/me/devices/{token}", authMw(http.HandlerFunc(h.DeleteDevice)))
}

// RegisterDevice records the caller's push token.
//
// Registering a token that is already on file for somebody else moves it to the
// caller (see Repository.RegisterDevice). That is required for correctness on a
// shared handset and is not an authorization hole worth closing here: the token
// is issued to the app instance by the OS, so whoever can present it is on the
// device it addresses.
func (h *Handler) RegisterDevice(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())

	var input struct {
		Token    string `json:"token"`
		Platform string `json:"platform"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if len(input.Token) > MaxDeviceTokenLen {
		httputil.JSONError(w, http.StatusBadRequest, "INVALID_PUSH_TOKEN", "push token is too long")
		return
	}
	// Rejected rather than stored: every unusable token is re-sent and
	// re-rejected on every notification for this user until something deletes
	// it, and nothing would, because the push service never answers
	// DeviceNotRegistered for a value it cannot even parse.
	if !push.IsExpoToken(input.Token) {
		httputil.JSONError(w, http.StatusBadRequest, "INVALID_PUSH_TOKEN", "push token is not a valid Expo push token")
		return
	}

	if err := h.repo.RegisterDevice(r.Context(), userID, input.Token, input.Platform); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusCreated, map[string]string{
		"token":    input.Token,
		"platform": NormalizePlatform(input.Platform),
	})
}

// DeleteDevice deregisters one of the caller's push tokens. The client calls it
// on logout, without which the next person to sign in on that handset keeps
// receiving the previous user's notifications until they happen to re-register.
func (h *Handler) DeleteDevice(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())

	// PathValue returns the segment already percent-decoded, which matters:
	// an Expo token is `ExponentPushToken[...]` and the brackets reach us as
	// %5B/%5D.
	token := r.PathValue("token")
	if token == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "token is required")
		return
	}

	ok, err := h.repo.DeleteDevice(r.Context(), userID, token)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !ok {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "device token not found")
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]string{"message": "device deregistered"})
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
	// Migration 009 constrains both columns; without this an over-long status
	// would be a CHECK violation rendered as a 500.
	if utf8.RuneCountInString(input.StatusText) > MaxStatusTextLen {
		httputil.JSONError(w, http.StatusBadRequest, "STATUS_TOO_LONG", "status_text is too long")
		return
	}
	if utf8.RuneCountInString(input.StatusEmoji) > MaxStatusEmojiLen {
		httputil.JSONError(w, http.StatusBadRequest, "STATUS_TOO_LONG", "status_emoji is too long")
		return
	}

	if err := h.repo.UpdateStatus(r.Context(), userID, input.StatusText, input.StatusEmoji); err != nil {
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

	// The client calls this on load and on reconnect, which makes it the
	// natural liveness heartbeat. The write is throttled inside the repository
	// and best-effort: a failure here must not fail the read.
	_ = h.repo.TouchLastActive(r.Context(), userID)

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
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if u == nil {
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

// GetUser resolves another user's public profile. It used to be a global
// lookup by id, which turned any leaked uuid into a directory entry across
// tenant boundaries; a shared workspace is now required. A stranger gets 404,
// not 403, so the endpoint cannot be used to test id existence.
func (h *Handler) GetUser(w http.ResponseWriter, r *http.Request) {
	callerID := authctx.UserID(r.Context())
	id := r.PathValue("user_id")

	shared, err := h.repo.SharesWorkspace(r.Context(), callerID, id)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !shared {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "user not found")
		return
	}

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
	callerID := authctx.UserID(r.Context())
	q := r.URL.Query().Get("q")
	if q == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "query parameter 'q' is required")
		return
	}

	users, err := h.repo.SearchInSharedWorkspaces(r.Context(), callerID, q, searchLimit)
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
