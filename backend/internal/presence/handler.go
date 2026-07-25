package presence

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

type Handler struct {
	service *Service
	authz   *authz.Checker
	pool    *pgxpool.Pool
}

func NewHandler(service *Service, az *authz.Checker, pool *pgxpool.Pool) *Handler {
	return &Handler{service: service, authz: az, pool: pool}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("GET /api/v1/workspaces/{workspace_id}/presence", authMw(http.HandlerFunc(h.WorkspacePresence)))
	mux.Handle("GET /api/v1/channels/{channel_id}/typing", authMw(http.HandlerFunc(h.ChannelTyping)))
}

// WorkspacePresence returns the presence status of every member of a workspace.
func (h *Handler) WorkspacePresence(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("workspace_id")
	if uuid.Validate(wsID) != nil {
		httputil.HandleError(w, httputil.NewBadRequest("workspace_id must be a UUID"))
		return
	}
	userID := authctx.UserID(r.Context())

	member, err := h.authz.IsWorkspaceMember(r.Context(), wsID, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !member {
		httputil.HandleError(w, httputil.NewForbidden("not a member of this workspace"))
		return
	}

	rows, err := h.pool.Query(r.Context(),
		`SELECT user_id FROM workspace_members WHERE workspace_id = $1`, wsID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	defer rows.Close()

	var userIDs []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		userIDs = append(userIDs, uid)
	}
	if err := rows.Err(); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	statuses := h.service.GetBulkStatus(r.Context(), userIDs)
	out := make(map[string]string, len(statuses))
	for uid, st := range statuses {
		out[uid] = string(st)
	}
	httputil.JSON(w, http.StatusOK, out)
}

// ChannelTyping returns the list of user IDs currently typing in a channel.
func (h *Handler) ChannelTyping(w http.ResponseWriter, r *http.Request) {
	chID := r.PathValue("channel_id")
	if uuid.Validate(chID) != nil {
		httputil.HandleError(w, httputil.NewBadRequest("channel_id must be a UUID"))
		return
	}
	userID := authctx.UserID(r.Context())

	ch, err := h.authz.Channel(r.Context(), chID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if ch == nil {
		httputil.HandleError(w, httputil.NewNotFound("channel not found"))
		return
	}
	ok, err := h.authz.CanReadChannel(r.Context(), ch, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !ok {
		httputil.HandleError(w, httputil.NewForbidden("not a member of this channel"))
		return
	}

	users, err := h.service.GetTypingUsers(r.Context(), chID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if users == nil {
		users = []string{}
	}
	httputil.JSON(w, http.StatusOK, map[string][]string{"typing": users})
}
