package workspace

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

type Handler struct {
	repo *Repository
}

func NewHandler(repo *Repository) *Handler {
	return &Handler{repo: repo}
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

	existing, err := h.repo.GetBySlug(r.Context(), input.Slug)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if existing != nil {
		httputil.JSONError(w, http.StatusConflict, "CONFLICT", "workspace slug already taken")
		return
	}

	ws := &Workspace{
		ID:          uuid.NewString(),
		Name:        input.Name,
		Slug:        input.Slug,
		Description: input.Description,
		OwnerID:     userID,
	}

	if err := h.repo.Create(r.Context(), ws); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	// Add creator as owner member
	if err := h.repo.AddMember(r.Context(), &Member{
		WorkspaceID: ws.ID,
		UserID:      userID,
		Role:        "owner",
	}); err != nil {
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

	if workspaces == nil {
		workspaces = []*Workspace{}
	}
	httputil.JSON(w, http.StatusOK, workspaces)
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	ws, err := h.repo.GetByID(r.Context(), r.PathValue("workspace_id"))
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

	member, err := h.repo.GetMember(r.Context(), wsID, userID)
	if err != nil || member == nil || (member.Role != "owner" && member.Role != "admin") {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "insufficient permissions")
		return
	}

	ws, err := h.repo.GetByID(r.Context(), wsID)
	if err != nil || ws == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "workspace not found")
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
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusOK, ws)
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("workspace_id")
	userID := authctx.UserID(r.Context())

	member, err := h.repo.GetMember(r.Context(), wsID, userID)
	if err != nil || member == nil || member.Role != "owner" {
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
	members, err := h.repo.ListMembers(r.Context(), r.PathValue("workspace_id"))
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if members == nil {
		members = []*Member{}
	}
	httputil.JSON(w, http.StatusOK, members)
}

func (h *Handler) UpdateMember(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("workspace_id")
	targetUID := r.PathValue("user_id")
	userID := authctx.UserID(r.Context())

	member, err := h.repo.GetMember(r.Context(), wsID, userID)
	if err != nil || member == nil || (member.Role != "owner" && member.Role != "admin") {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "insufficient permissions")
		return
	}

	var input struct {
		Role string `json:"role"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	if err := h.repo.UpdateMemberRole(r.Context(), wsID, targetUID, input.Role); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "role updated"})
}

func (h *Handler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("workspace_id")
	targetUID := r.PathValue("user_id")
	userID := authctx.UserID(r.Context())

	member, err := h.repo.GetMember(r.Context(), wsID, userID)
	if err != nil || member == nil || (member.Role != "owner" && member.Role != "admin") {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "insufficient permissions")
		return
	}

	if err := h.repo.RemoveMember(r.Context(), wsID, targetUID); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "member removed"})
}
