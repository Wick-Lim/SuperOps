package search

import (
	"context"
	"net/http"
	"strconv"
	"strings"

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

// AccessKeys resolves the set of access keys a caller holds in a workspace.
//
// This is the query side of the model described in doc.go, and it is the only
// place the caller's key set is built. It answers exclusively from
// internal/authz — no membership SQL of its own — and it is bounded by the
// caller's memberships, not by the number of objects in the workspace:
//
//	w-<workspace>  the caller is a member of the workspace
//	u-<caller>     objects shared with, or owned by, this user
//	c-<channel>    one per channel the caller may read
//
// A non-member gets an empty slice, which buildFilter renders as "no results"
// rather than "no filter".
func AccessKeys(ctx context.Context, az *authz.Checker, workspaceID, userID string) ([]string, error) {
	// Deliberately re-checked here even though the handler has already refused
	// non-members: this function is the one thing every future search caller
	// will reuse, and it must be safe on its own.
	member, err := az.IsWorkspaceMember(ctx, workspaceID, userID)
	if err != nil {
		return nil, err
	}
	if !member {
		return []string{}, nil
	}

	readable, err := az.ReadableChannelIDs(ctx, workspaceID, userID)
	if err != nil {
		return nil, err
	}

	keys := keySet(WorkspaceKey(workspaceID), UserKey(userID))
	for _, id := range readable {
		if k := ChannelKey(id); k != "" {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

// Search full-text searches every object type in a workspace.
//
// Authorization is two-layered and both layers are load-bearing: the caller
// must be a member of the workspace, and the Meilisearch filter is constrained
// to the access keys they actually hold. Filtering on workspace_id alone (the
// original behaviour) exposed every private channel and 1:1 DM of every
// workspace to any authenticated user.
//
// Query parameters:
//
//	q        required, the search text
//	type     optional, comma-separated object types (message,file,...)
//	channel  optional, narrows to one channel the caller may already read
//	from     optional, narrows to one author
//	limit    optional, 1..100
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

	limit := defaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		l, err := strconv.Atoi(raw)
		if err != nil || l <= 0 || l > maxSearchLimit {
			httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "limit must be between 1 and 100")
			return
		}
		limit = l
	}

	types, ok := parseTypes(r.URL.Query().Get("type"))
	if !ok {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST",
			"'type' must be a comma-separated list of "+strings.Join(typeNames(), ", "))
		return
	}

	fromUserID := r.URL.Query().Get("from")
	if fromUserID != "" {
		if _, ok := canonicalUUID(fromUserID); !ok {
			httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "'from' must be a user id")
			return
		}
	}

	keys, err := AccessKeys(ctx, h.authz, wsID, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	// An explicit channel filter narrows what the keys already allow; it can
	// never widen it. Asking for a channel the caller cannot read is refused
	// rather than quietly ignored.
	channelID := r.URL.Query().Get("channel")
	if channelID != "" {
		key := ChannelKey(channelID)
		if key == "" || !contains(keys, key) {
			httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "not allowed to search this channel")
			return
		}
	}

	result, err := h.service.Search(ctx, Query{
		WorkspaceID: wsID,
		Text:        q,
		Keys:        keys,
		Types:       types,
		ChannelID:   channelID,
		FromUserID:  fromUserID,
		Limit:       limit,
	})
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusOK, result)
}

// parseTypes reads the `type` parameter. An empty parameter means every type —
// that is the "one search across everything" default — but a parameter that
// names something unknown is an error, never an empty narrowing.
func parseTypes(raw string) ([]ObjectType, bool) {
	if strings.TrimSpace(raw) == "" {
		return nil, true
	}
	parts := strings.Split(raw, ",")
	types := make([]ObjectType, 0, len(parts))
	for _, part := range parts {
		t, ok := ParseObjectType(part)
		if !ok {
			return nil, false
		}
		types = append(types, t)
	}
	return types, true
}

func typeNames() []string {
	names := make([]string, 0, len(ObjectTypes))
	for _, t := range ObjectTypes {
		names = append(names, string(t))
	}
	return names
}

func contains(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
