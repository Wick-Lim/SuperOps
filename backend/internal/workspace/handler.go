package workspace

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

// SubscriptionRevoker is the subset of *ws.Hub this package needs. Kept as an
// interface for the same reason internal/channel does: no dependency on the
// WebSocket implementation, and handlers stay unit-testable without a hub.
//
// Revocation is best effort — the membership rows are already deleted by the
// time it is called, so a hub that cannot be reached must not fail the request.
type SubscriptionRevoker interface {
	RevokeChannelSubscription(userID, channelID string)
}

type Handler struct {
	repo *Repository
	az   *authz.Checker
	hub  SubscriptionRevoker
}

func NewHandler(repo *Repository, az *authz.Checker, hub SubscriptionRevoker) *Handler {
	return &Handler{repo: repo, az: az, hub: hub}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("POST /api/v1/workspaces", authMw(http.HandlerFunc(h.Create)))
	mux.Handle("GET /api/v1/workspaces", authMw(http.HandlerFunc(h.List)))
	mux.Handle("GET /api/v1/workspaces/{workspace_id}", authMw(http.HandlerFunc(h.Get)))
	mux.Handle("PATCH /api/v1/workspaces/{workspace_id}", authMw(http.HandlerFunc(h.Update)))
	mux.Handle("DELETE /api/v1/workspaces/{workspace_id}", authMw(http.HandlerFunc(h.Delete)))
	mux.Handle("GET /api/v1/workspaces/{workspace_id}/members", authMw(http.HandlerFunc(h.ListMembers)))
	mux.Handle("PATCH /api/v1/workspaces/{workspace_id}/members/{user_id}", authMw(http.HandlerFunc(h.UpdateMember)))
	mux.Handle("DELETE /api/v1/workspaces/{workspace_id}/members/{user_id}", authMw(http.HandlerFunc(h.RemoveMember)))
	mux.Handle("POST /api/v1/workspaces/{workspace_id}/transfer-ownership", authMw(http.HandlerFunc(h.TransferOwnership)))
}

// requireMember gates a read endpoint on membership. Get and ListMembers had no
// authorization at all: any bearer token could read a workspace's name, slug and
// owner, and dump its entire roster, by id alone. A non-member gets 404 rather
// than 403 so workspace ids remain unprobeable.
func (h *Handler) requireMember(w http.ResponseWriter, r *http.Request) (string, bool) {
	wsID := r.PathValue("workspace_id")
	ok, err := h.az.Can(r.Context(), authz.UserSubject(authctx.UserID(r.Context())),
		authz.WorkspaceObject(wsID), authz.CapRead)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return "", false
	}
	if !ok {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "workspace not found")
		return "", false
	}
	return wsID, true
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())

	var input struct {
		Name        string `json:"name"`
		Slug        string `json:"slug"`
		Description string `json:"description"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	if input.Name == "" || input.Slug == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "name and slug are required")
		return
	}

	ws := &Workspace{
		ID:          uuid.NewString(),
		Name:        input.Name,
		Slug:        input.Slug,
		Description: input.Description,
		OwnerID:     userID,
	}

	// Uniqueness is enforced by the index, not by a preceding SELECT: the
	// pre-check was a race the loser lost with a 500.
	if err := h.repo.CreateWithOwner(r.Context(), ws); err != nil {
		if errors.Is(err, ErrSlugTaken) {
			httputil.HandleError(w, httputil.NewConflict("workspace slug already taken"))
			return
		}
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusCreated, ws)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())

	workspaces, err := h.repo.ListByUser(r.Context(), userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusOK, workspaces)
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	wsID, ok := h.requireMember(w, r)
	if !ok {
		return
	}

	ws, err := h.repo.GetByID(r.Context(), wsID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if ws == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "workspace not found")
		return
	}
	httputil.JSON(w, http.StatusOK, ws)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("workspace_id")
	userID := authctx.UserID(r.Context())

	admin, err := h.az.Can(r.Context(), authz.UserSubject(userID), authz.WorkspaceObject(wsID), authz.CapAdmin)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !admin {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "insufficient permissions")
		return
	}

	var input struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		IconURL     *string `json:"icon_url"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	ws, err := h.repo.GetByID(r.Context(), wsID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if ws == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "workspace not found")
		return
	}

	if input.Name != nil {
		ws.Name = *input.Name
	}
	if input.Description != nil {
		ws.Description = *input.Description
	}
	if input.IconURL != nil {
		ws.IconURL = *input.IconURL
	}

	if err := h.repo.Update(r.Context(), ws); err != nil {
		if errors.Is(err, ErrWorkspaceGone) {
			httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "workspace not found")
			return
		}
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusOK, ws)
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("workspace_id")
	userID := authctx.UserID(r.Context())

	owner, err := h.az.IsWorkspaceOwner(r.Context(), wsID, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !owner {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "only workspace owner can delete")
		return
	}

	if err := h.repo.Delete(r.Context(), wsID); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "workspace deleted"})
}

func (h *Handler) ListMembers(w http.ResponseWriter, r *http.Request) {
	wsID, ok := h.requireMember(w, r)
	if !ok {
		return
	}

	members, err := h.repo.ListMembers(r.Context(), wsID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusOK, members)
}

func (h *Handler) UpdateMember(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("workspace_id")
	targetUID := r.PathValue("user_id")
	userID := authctx.UserID(r.Context())

	var input struct {
		Role string `json:"role"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	actorRole, err := h.az.WorkspaceRole(r.Context(), wsID, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	targetRole, err := h.az.WorkspaceRole(r.Context(), wsID, targetUID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	if appErr := validateRoleChange(actorRole, targetRole, input.Role); appErr != nil {
		httputil.HandleError(w, appErr)
		return
	}

	if err := h.repo.UpdateMemberRole(r.Context(), wsID, targetUID, input.Role); err != nil {
		if errors.Is(err, ErrMemberNotFound) {
			httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "member not found")
			return
		}
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "role updated"})
}

func (h *Handler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("workspace_id")
	targetUID := r.PathValue("user_id")
	userID := authctx.UserID(r.Context())

	actorRole, err := h.az.WorkspaceRole(r.Context(), wsID, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	targetRole, err := h.az.WorkspaceRole(r.Context(), wsID, targetUID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	if appErr := validateRemoval(actorRole, targetRole); appErr != nil {
		httputil.HandleError(w, appErr)
		return
	}

	removedFrom, err := h.repo.RemoveMember(r.Context(), wsID, targetUID)
	if err != nil {
		if errors.Is(err, ErrMemberNotFound) {
			httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "member not found")
			return
		}
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	// Hub subscriptions live in memory and survive the DELETE, so without this
	// the evicted user keeps receiving message.new for every channel they were
	// in until they happen to disconnect.
	if h.hub != nil {
		for _, channelID := range removedFrom {
			h.hub.RevokeChannelSubscription(targetUID, channelID)
		}
	}

	httputil.JSON(w, http.StatusOK, map[string]any{
		"message":             "member removed",
		"channels_removed":    len(removedFrom),
		"removed_channel_ids": removedFrom,
	})
}

// TransferOwnership is the only way the 'owner' role moves. Making it an
// explicit endpoint keeps PATCH /members/{user_id} unable to mint owners — an
// admin could otherwise promote themselves — and lets the demotion of the
// current owner, the promotion of the new one and workspaces.owner_id all
// change in a single transaction.
func (h *Handler) TransferOwnership(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("workspace_id")
	userID := authctx.UserID(r.Context())

	var input struct {
		UserID string `json:"user_id"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if input.UserID == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "user_id is required")
		return
	}

	owner, err := h.az.IsWorkspaceOwner(r.Context(), wsID, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !owner {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "only the workspace owner can transfer ownership")
		return
	}
	if input.UserID == userID {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "already the workspace owner")
		return
	}

	targetRole, err := h.az.WorkspaceRole(r.Context(), wsID, input.UserID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if targetRole == "" {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "member not found")
		return
	}

	if err := h.repo.TransferOwnership(r.Context(), wsID, userID, input.UserID); err != nil {
		if errors.Is(err, ErrMemberNotFound) {
			httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "member not found")
			return
		}
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "ownership transferred"})
}
