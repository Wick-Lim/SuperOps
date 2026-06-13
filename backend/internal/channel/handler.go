package channel

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
	mux.Handle("POST /api/v1/workspaces/{workspace_id}/channels", authMw(http.HandlerFunc(h.Create)))
	mux.Handle("POST /api/v1/workspaces/{workspace_id}/channels/dm", authMw(http.HandlerFunc(h.CreateDM)))
	mux.Handle("POST /api/v1/workspaces/{workspace_id}/channels/{channel_id}/archive", authMw(http.HandlerFunc(h.Archive)))
	mux.Handle("POST /api/v1/workspaces/{workspace_id}/channels/{channel_id}/unarchive", authMw(http.HandlerFunc(h.Unarchive)))
	mux.Handle("GET /api/v1/workspaces/{workspace_id}/channels", authMw(http.HandlerFunc(h.List)))
	mux.Handle("GET /api/v1/workspaces/{workspace_id}/channels/browse", authMw(http.HandlerFunc(h.Browse)))
	mux.Handle("GET /api/v1/workspaces/{workspace_id}/channels/{channel_id}", authMw(http.HandlerFunc(h.Get)))
	mux.Handle("PATCH /api/v1/workspaces/{workspace_id}/channels/{channel_id}", authMw(http.HandlerFunc(h.Update)))
	mux.Handle("POST /api/v1/workspaces/{workspace_id}/channels/{channel_id}/join", authMw(http.HandlerFunc(h.Join)))
	mux.Handle("POST /api/v1/workspaces/{workspace_id}/channels/{channel_id}/leave", authMw(http.HandlerFunc(h.Leave)))
	mux.Handle("GET /api/v1/workspaces/{workspace_id}/channels/{channel_id}/members", authMw(http.HandlerFunc(h.ListMembers)))
	mux.Handle("PUT /api/v1/channels/{channel_id}/read", authMw(http.HandlerFunc(h.MarkRead)))
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	wsID := r.PathValue("workspace_id")

	var input struct {
		Name        string `json:"name"`
		Slug        string `json:"slug"`
		Description string `json:"description"`
		Type        string `json:"type"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	if input.Name == "" || input.Slug == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "name and slug are required")
		return
	}

	chType := ChannelType(input.Type)
	if chType == "" {
		chType = TypePublic
	}

	ch := &Channel{
		ID:          uuid.NewString(),
		WorkspaceID: wsID,
		Name:        &input.Name,
		Slug:        &input.Slug,
		Description: input.Description,
		Type:        chType,
		CreatorID:   &userID,
	}

	if err := h.repo.Create(r.Context(), ch); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	// Add creator as admin member
	h.repo.AddMember(r.Context(), &ChannelMember{
		ChannelID: ch.ID,
		UserID:    userID,
		Role:      "admin",
	})

	httputil.JSON(w, http.StatusCreated, ch)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	wsID := r.PathValue("workspace_id")

	channels, err := h.repo.ListByWorkspaceAndUser(r.Context(), wsID, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if channels == nil {
		channels = []*Channel{}
	}
	httputil.JSON(w, http.StatusOK, channels)
}

func (h *Handler) Browse(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("workspace_id")

	channels, err := h.repo.ListPublicByWorkspace(r.Context(), wsID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if channels == nil {
		channels = []*Channel{}
	}
	httputil.JSON(w, http.StatusOK, channels)
}

// isMember reports whether the caller belongs to the channel.
func (h *Handler) isMember(r *http.Request, channelID string) bool {
	m, err := h.repo.GetMember(r.Context(), channelID, authctx.UserID(r.Context()))
	return err == nil && m != nil
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	ch, err := h.repo.GetByID(r.Context(), r.PathValue("channel_id"))
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if ch == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "channel not found")
		return
	}
	// Non-public channels (private/dm/group_dm) are only visible to members.
	if ch.Type != TypePublic && !h.isMember(r, ch.ID) {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "not a member of this channel")
		return
	}
	httputil.JSON(w, http.StatusOK, ch)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	chID := r.PathValue("channel_id")
	userID := authctx.UserID(r.Context())

	member, err := h.repo.GetMember(r.Context(), chID, userID)
	if err != nil || member == nil || member.Role != "admin" {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "insufficient permissions")
		return
	}

	ch, err := h.repo.GetByID(r.Context(), chID)
	if err != nil || ch == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "channel not found")
		return
	}

	var input struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		Topic       *string `json:"topic"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	if input.Name != nil {
		ch.Name = input.Name
	}
	if input.Description != nil {
		ch.Description = *input.Description
	}
	if input.Topic != nil {
		ch.Topic = *input.Topic
	}

	if err := h.repo.Update(r.Context(), ch); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusOK, ch)
}

func (h *Handler) Join(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	chID := r.PathValue("channel_id")

	ch, err := h.repo.GetByID(r.Context(), chID)
	if err != nil || ch == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "channel not found")
		return
	}

	if ch.Type != TypePublic {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "cannot join non-public channel")
		return
	}

	if err := h.repo.AddMember(r.Context(), &ChannelMember{
		ChannelID: chID,
		UserID:    userID,
		Role:      "member",
	}); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "joined channel"})
}

func (h *Handler) Leave(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	chID := r.PathValue("channel_id")

	if err := h.repo.RemoveMember(r.Context(), chID, userID); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "left channel"})
}

func (h *Handler) ListMembers(w http.ResponseWriter, r *http.Request) {
	chID := r.PathValue("channel_id")
	if !h.isMember(r, chID) {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "not a member of this channel")
		return
	}
	members, err := h.repo.ListMembers(r.Context(), chID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if members == nil {
		members = []*ChannelMember{}
	}
	httputil.JSON(w, http.StatusOK, members)
}

func (h *Handler) MarkRead(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	chID := r.PathValue("channel_id")

	if !h.isMember(r, chID) {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "not a member of this channel")
		return
	}
	if err := h.repo.UpdateReadAt(r.Context(), chID, userID); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "marked as read"})
}

func (h *Handler) CreateDM(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	wsID := r.PathValue("workspace_id")

	var input struct {
		UserIDs []string `json:"user_ids"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	// Caller must be a member of the workspace.
	ok, err := h.repo.IsWorkspaceMember(r.Context(), wsID, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !ok {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "not a workspace member")
		return
	}

	// Build the unique set of participants: caller plus the requested others.
	seen := map[string]bool{userID: true}
	others := make([]string, 0, len(input.UserIDs))
	for _, uid := range input.UserIDs {
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		others = append(others, uid)
	}

	if len(others) == 0 {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "at least one other user_id is required")
		return
	}

	// Validate every other participant is a member of the workspace.
	for _, uid := range others {
		ok, err := h.repo.IsWorkspaceMember(r.Context(), wsID, uid)
		if err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		if !ok {
			httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "user_id is not a workspace member")
			return
		}
	}

	chType := TypeDM
	if len(others) > 1 {
		chType = TypeGroupDM
	}

	// For 1:1 DMs, be idempotent: return an existing dm channel if one exists.
	if chType == TypeDM {
		existing, err := h.repo.FindDirectChannel(r.Context(), wsID, userID, others[0])
		if err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		if existing != nil {
			httputil.JSON(w, http.StatusOK, existing)
			return
		}
	}

	ch := &Channel{
		ID:          uuid.NewString(),
		WorkspaceID: wsID,
		Name:        nil,
		Slug:        nil,
		Type:        chType,
		CreatorID:   &userID,
	}

	if err := h.repo.Create(r.Context(), ch); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	// Add the caller and all other participants as members.
	participants := append([]string{userID}, others...)
	for _, uid := range participants {
		if err := h.repo.AddMember(r.Context(), &ChannelMember{
			ChannelID: ch.ID,
			UserID:    uid,
			Role:      "member",
		}); err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
	}

	httputil.JSON(w, http.StatusCreated, ch)
}

func (h *Handler) Archive(w http.ResponseWriter, r *http.Request) {
	h.setArchived(w, r, true)
}

func (h *Handler) Unarchive(w http.ResponseWriter, r *http.Request) {
	h.setArchived(w, r, false)
}

func (h *Handler) setArchived(w http.ResponseWriter, r *http.Request, archived bool) {
	userID := authctx.UserID(r.Context())
	chID := r.PathValue("channel_id")

	member, err := h.repo.GetMember(r.Context(), chID, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if member == nil || member.Role != "admin" {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "insufficient permissions")
		return
	}

	ch, err := h.repo.GetByID(r.Context(), chID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if ch == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "channel not found")
		return
	}

	if err := h.repo.SetArchived(r.Context(), chID, archived); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	ch.IsArchived = archived
	httputil.JSON(w, http.StatusOK, ch)
}
