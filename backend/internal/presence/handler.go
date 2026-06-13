package presence

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

type Handler struct {
	service *Service
	pool    *pgxpool.Pool
}

func NewHandler(service *Service, pool *pgxpool.Pool) *Handler {
	return &Handler{service: service, pool: pool}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("GET /api/v1/workspaces/{workspace_id}/presence", authMw(http.HandlerFunc(h.WorkspacePresence)))
	mux.Handle("GET /api/v1/channels/{channel_id}/typing", authMw(http.HandlerFunc(h.ChannelTyping)))
}

// WorkspacePresence returns the presence status of every member of a workspace.
func (h *Handler) WorkspacePresence(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("workspace_id")

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
			continue
		}
		userIDs = append(userIDs, uid)
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
	users := h.service.GetTypingUsers(r.Context(), chID)
	if users == nil {
		users = []string{}
	}
	httputil.JSON(w, http.StatusOK, map[string][]string{"typing": users})
}
