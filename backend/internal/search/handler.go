package search

import (
	"net/http"
	"strconv"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

const maxSearchLimit = 100

type Handler struct {
	service *Service
	authz   *authz.Checker
}

func NewHandler(service *Service, az *authz.Checker) *Handler {
	return &Handler{service: service, authz: az}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("GET /api/v1/workspaces/{workspace_id}/search", authMw(http.HandlerFunc(h.Search)))
}

// Search full-text searches a workspace.
//
// Authorization is two-layered and both layers are load-bearing: the caller
// must be a member of the workspace, and the Meilisearch filter is constrained
// to the channels they may actually read. Filtering on workspace_id alone (the
// previous behaviour) exposed every private channel and 1:1 DM of every
// workspace to any authenticated user.
func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := authctx.UserID(ctx)
	wsID := r.PathValue("workspace_id")

	q := r.URL.Query().Get("q")
	if q == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "query parameter 'q' is required")
		return
	}

	member, err := h.authz.IsWorkspaceMember(ctx, wsID, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !member {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "workspace membership required")
		return
	}

	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		l, err := strconv.Atoi(raw)
		if err != nil || l <= 0 || l > maxSearchLimit {
			httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "limit must be between 1 and 100")
			return
		}
		limit = l
	}

	fromUserID := r.URL.Query().Get("from")
	if fromUserID != "" {
		if _, ok := canonicalUUID(fromUserID); !ok {
			httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "'from' must be a user id")
			return
		}
	}

	readable, err := h.authz.ReadableChannelIDs(ctx, wsID, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	// An explicit channel filter narrows the readable set; it can never widen it.
	if chID := r.URL.Query().Get("channel"); chID != "" {
		if !contains(readable, chID) {
			httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "not allowed to search this channel")
			return
		}
		readable = []string{chID}
	}

	result, err := h.service.Search(ctx, Query{
		WorkspaceID: wsID,
		Text:        q,
		ChannelIDs:  readable,
		FromUserID:  fromUserID,
		Limit:       limit,
	})
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusOK, result)
}

func contains(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
