package channel

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
)

type Handler struct {
	repo *Repository
	az   *authz.Checker
	// hub fans channel lifecycle events out to connected clients and revokes
	// subscriptions that have stopped being authorized. It may be nil.
	hub  Publisher
	nats *natspkg.Client
	log  *slog.Logger
}

func NewHandler(repo *Repository, az *authz.Checker, hub Publisher, nats *natspkg.Client, log *slog.Logger) *Handler {
	return &Handler{repo: repo, az: az, hub: hub, nats: nats, log: log}
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
	mux.Handle("POST /api/v1/workspaces/{workspace_id}/channels/{channel_id}/members", authMw(http.HandlerFunc(h.AddMember)))
	mux.Handle("DELETE /api/v1/workspaces/{workspace_id}/channels/{channel_id}/members/{user_id}", authMw(http.HandlerFunc(h.RemoveMember)))
	mux.Handle("PUT /api/v1/channels/{channel_id}/read", authMw(http.HandlerFunc(h.MarkRead)))
	mux.Handle("GET /api/v1/channels/{channel_id}/unread", authMw(http.HandlerFunc(h.Unread)))
	mux.Handle("PATCH /api/v1/channels/{channel_id}/prefs", authMw(http.HandlerFunc(h.UpdatePrefs)))
}

// ---------------------------------------------------------------------------
// Shared guards
// ---------------------------------------------------------------------------

// requireWorkspaceMember gates an endpoint on membership of the workspace named
// in the path. Browse, Join and Create had no such gate, which let any
// authenticated caller enumerate, join and create channels in another tenant.
func (h *Handler) requireWorkspaceMember(w http.ResponseWriter, r *http.Request) (string, bool) {
	wsID := r.PathValue("workspace_id")
	ok, err := h.az.IsWorkspaceMember(r.Context(), wsID, authctx.UserID(r.Context()))
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return "", false
	}
	if !ok {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "not a workspace member")
		return "", false
	}
	return wsID, true
}

// resolveChannel loads the channel named in the path and verifies it really
// belongs to the workspace also named in the path. Without this the
// {workspace_id} segment was decorative: every channel-scoped route resolved by
// channel id alone, so any id could be addressed through any workspace. A
// mismatch answers 404 rather than 403 so the caller learns nothing about ids
// outside their tenant.
func (h *Handler) resolveChannel(w http.ResponseWriter, r *http.Request) (*authz.ChannelInfo, bool) {
	ch, err := h.az.Channel(r.Context(), r.PathValue("channel_id"))
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return nil, false
	}
	if ch == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "channel not found")
		return nil, false
	}
	if wsID := r.PathValue("workspace_id"); wsID != "" && ch.WorkspaceID != wsID {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "channel not found")
		return nil, false
	}
	return ch, true
}

// requireChannelMember resolves the channel and asserts the caller has a
// membership row. Used by the read-marker and preference routes, which are
// meaningless without one.
func (h *Handler) requireChannelMember(w http.ResponseWriter, r *http.Request) (*authz.ChannelInfo, bool) {
	ch, ok := h.resolveChannel(w, r)
	if !ok {
		return nil, false
	}
	member, err := h.az.IsChannelMember(r.Context(), ch.ID, authctx.UserID(r.Context()))
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return nil, false
	}
	if !member {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "not a member of this channel")
		return nil, false
	}
	return ch, true
}

// canAdministerChannel is the gate for changing a channel or its roster: the
// channel's own admins, or an admin of the owning workspace.
func (h *Handler) canAdministerChannel(r *http.Request, ch *authz.ChannelInfo, userID string) (bool, error) {
	ok, err := h.az.IsChannelAdmin(r.Context(), ch.ID, userID)
	if err != nil || ok {
		return ok, err
	}
	return h.az.IsWorkspaceAdmin(r.Context(), ch.WorkspaceID, userID)
}

// ---------------------------------------------------------------------------
// Channels
// ---------------------------------------------------------------------------

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	wsID, ok := h.requireWorkspaceMember(w, r)
	if !ok {
		return
	}

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
	if input.Type == "" {
		chType = TypePublic
	}
	// Only named channels come through this route. Accepting dm/group_dm here
	// would forge a conversation with a slug and a single participant that the
	// DM dedupe key could never match.
	if chType != TypePublic && chType != TypePrivate {
		httputil.JSONError(w, http.StatusBadRequest, "INVALID_CHANNEL_TYPE", "type must be 'public' or 'private'")
		return
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

	if err := h.repo.CreateWithOwner(r.Context(), ch, userID); err != nil {
		if errors.Is(err, ErrSlugTaken) {
			httputil.HandleError(w, httputil.NewConflict("channel slug already taken"))
			return
		}
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	// The creator is the whole roster at this point, which is what makes a
	// private channel deliverable at all: the relay addresses a non-public
	// channel.created to member_ids only.
	h.emitChannelCreated(ch, []string{userID})

	httputil.JSON(w, http.StatusCreated, ch)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	wsID, ok := h.requireWorkspaceMember(w, r)
	if !ok {
		return
	}

	channels, err := h.repo.ListByWorkspaceAndUser(r.Context(), wsID, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusOK, channels)
}

func (h *Handler) Browse(w http.ResponseWriter, r *http.Request) {
	wsID, ok := h.requireWorkspaceMember(w, r)
	if !ok {
		return
	}

	channels, err := h.repo.ListPublicByWorkspace(r.Context(), wsID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusOK, channels)
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	info, ok := h.resolveChannel(w, r)
	if !ok {
		return
	}

	readable, err := h.az.CanReadChannel(r.Context(), info, authctx.UserID(r.Context()))
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !readable {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "not a member of this channel")
		return
	}

	ch, err := h.repo.GetByID(r.Context(), info.ID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if ch == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "channel not found")
		return
	}
	httputil.JSON(w, http.StatusOK, ch)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	info, ok := h.resolveChannel(w, r)
	if !ok {
		return
	}

	allowed, err := h.canAdministerChannel(r, info, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !allowed {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "insufficient permissions")
		return
	}
	if info.IsArchived {
		httputil.JSONError(w, http.StatusConflict, "CHANNEL_ARCHIVED", "channel is archived")
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

	ch, err := h.repo.GetByID(r.Context(), info.ID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if ch == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "channel not found")
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
		if errors.Is(err, ErrChannelGone) {
			httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "channel not found")
			return
		}
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	h.emitChannelUpdated(ch)

	httputil.JSON(w, http.StatusOK, ch)
}

func (h *Handler) Join(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	if _, ok := h.requireWorkspaceMember(w, r); !ok {
		return
	}
	info, ok := h.resolveChannel(w, r)
	if !ok {
		return
	}

	if info.Type != string(TypePublic) {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "cannot join non-public channel")
		return
	}
	if info.IsArchived {
		httputil.JSONError(w, http.StatusConflict, "CHANNEL_ARCHIVED", "channel is archived")
		return
	}

	if err := h.repo.AddMember(r.Context(), &ChannelMember{
		ChannelID: info.ID,
		UserID:    userID,
		Role:      RoleMember,
	}); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	h.emitMemberJoined(info.WorkspaceID, info.ID, userID, RoleMember)

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "joined channel"})
}

func (h *Handler) Leave(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	info, ok := h.resolveChannel(w, r)
	if !ok {
		return
	}
	if !h.checkDeparture(w, r, info, userID) {
		return
	}

	if err := h.repo.RemoveMember(r.Context(), info.ID, userID); err != nil {
		if errors.Is(err, ErrNotAMember) {
			httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "not a member of this channel")
			return
		}
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	h.emitMemberLeft(info.WorkspaceID, info.ID, userID)

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "left channel"})
}

// checkDeparture validates that target may be removed from the channel.
//
// A DM roster is fixed: letting a participant leave drops a 1:1 DM below two
// members, which orphaned the conversation entirely — the dedupe key no longer
// matched, so the next attempt minted a fresh channel, and Join refuses
// non-public types so the old history became unreachable. The last admin
// leaving is refused for the same class of reason: nobody could then rename,
// archive or manage the roster.
func (h *Handler) checkDeparture(w http.ResponseWriter, r *http.Request, info *authz.ChannelInfo, target string) bool {
	role, err := h.az.ChannelRole(r.Context(), info.ID, target)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return false
	}
	if role == "" {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "not a member of this channel")
		return false
	}

	if ChannelType(info.Type).IsDM() {
		httputil.JSONError(w, http.StatusConflict, "CANNOT_MODIFY_DM", "direct message participants are fixed")
		return false
	}
	if role != RoleAdmin {
		return true
	}

	total, admins, err := h.repo.MemberCounts(r.Context(), info.ID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return false
	}
	// Emptying the channel entirely is fine; leaving it populated but
	// unmanageable is not.
	if admins <= 1 && total > 1 {
		httputil.JSONError(w, http.StatusConflict, "LAST_CHANNEL_ADMIN", "promote another admin first")
		return false
	}
	return true
}

func (h *Handler) Archive(w http.ResponseWriter, r *http.Request) {
	h.setArchived(w, r, true)
}

func (h *Handler) Unarchive(w http.ResponseWriter, r *http.Request) {
	h.setArchived(w, r, false)
}

func (h *Handler) setArchived(w http.ResponseWriter, r *http.Request, archived bool) {
	userID := authctx.UserID(r.Context())
	info, ok := h.resolveChannel(w, r)
	if !ok {
		return
	}

	allowed, err := h.canAdministerChannel(r, info, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !allowed {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "insufficient permissions")
		return
	}

	ch, err := h.repo.SetArchived(r.Context(), info.ID, archived)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if ch == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "channel not found")
		return
	}

	h.emitChannelUpdated(ch)
	if archived {
		// An archived channel is gone from every client's view, but delivery is
		// gated on a subscription map written only at subscribe time, so its
		// subscribers would otherwise keep receiving its traffic for the life of
		// their sockets. Revoke after the announcement so they see the state
		// change before the "unsubscribed" frame explains it.
		h.revokeChannelForAll(ch.ID)
	}

	httputil.JSON(w, http.StatusOK, ch)
}

// ---------------------------------------------------------------------------
// Members
// ---------------------------------------------------------------------------

func (h *Handler) ListMembers(w http.ResponseWriter, r *http.Request) {
	info, ok := h.resolveChannel(w, r)
	if !ok {
		return
	}

	readable, err := h.az.CanReadChannel(r.Context(), info, authctx.UserID(r.Context()))
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !readable {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "not a member of this channel")
		return
	}

	members, err := h.repo.ListMembers(r.Context(), info.ID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusOK, members)
}

// AddMember admits a workspace member to a channel. Until this route existed a
// private channel was single-occupant by construction: nothing anywhere added a
// member, and Join refuses non-public types.
func (h *Handler) AddMember(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	info, ok := h.resolveChannel(w, r)
	if !ok {
		return
	}

	var input struct {
		UserID string `json:"user_id"`
		Role   string `json:"role"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if input.UserID == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "user_id is required")
		return
	}
	role := input.Role
	if role == "" {
		role = RoleMember
	}
	if role != RoleAdmin && role != RoleMember {
		httputil.JSONError(w, http.StatusBadRequest, "INVALID_ROLE", "role must be 'admin' or 'member'")
		return
	}

	// Authorize before reporting anything about the channel's state, so a
	// stranger cannot probe whether an id is a DM or an archived channel.
	allowed, err := h.canAdministerChannel(r, info, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !allowed {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "insufficient permissions")
		return
	}

	if ChannelType(info.Type).IsDM() {
		httputil.JSONError(w, http.StatusConflict, "CANNOT_MODIFY_DM", "direct message participants are fixed")
		return
	}
	if info.IsArchived {
		httputil.JSONError(w, http.StatusConflict, "CHANNEL_ARCHIVED", "channel is archived")
		return
	}

	// A channel never grants access broader than its workspace.
	target, err := h.az.IsWorkspaceMember(r.Context(), info.WorkspaceID, input.UserID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !target {
		httputil.JSONError(w, http.StatusBadRequest, "NOT_WORKSPACE_MEMBER", "user is not a member of this workspace")
		return
	}

	if err := h.repo.AddMember(r.Context(), &ChannelMember{
		ChannelID: info.ID,
		UserID:    input.UserID,
		Role:      role,
	}); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	member, err := h.repo.GetMember(r.Context(), info.ID, input.UserID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if member == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "member not found")
		return
	}

	h.announceMemberAdded(r.Context(), info, input.UserID, userID, role)

	httputil.JSON(w, http.StatusCreated, member)
}

// RemoveMember evicts a member from a channel. Self-removal is always allowed
// (it is Leave by another name); removing somebody else requires channel or
// workspace admin.
func (h *Handler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	targetID := r.PathValue("user_id")
	info, ok := h.resolveChannel(w, r)
	if !ok {
		return
	}

	if targetID != userID {
		allowed, err := h.canAdministerChannel(r, info, userID)
		if err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		if !allowed {
			httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "insufficient permissions")
			return
		}
	}

	if !h.checkDeparture(w, r, info, targetID) {
		return
	}

	if err := h.repo.RemoveMember(r.Context(), info.ID, targetID); err != nil {
		if errors.Is(err, ErrNotAMember) {
			httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "not a member of this channel")
			return
		}
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	h.emitMemberLeft(info.WorkspaceID, info.ID, targetID)

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "member removed"})
}

// ---------------------------------------------------------------------------
// Read state and preferences
// ---------------------------------------------------------------------------

// MarkRead advances the caller's read marker. The client may name the last
// message it actually rendered (message_id, preferred) or supply the timestamp
// it rendered at (read_at); with neither, the marker moves to now, which
// silently swallows anything that arrived between render and this call.
func (h *Handler) MarkRead(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	info, ok := h.requireChannelMember(w, r)
	if !ok {
		return
	}

	var input struct {
		MessageID string     `json:"message_id"`
		ReadAt    *time.Time `json:"read_at"`
	}
	// The body is optional: the original route took none.
	if err := httputil.DecodeJSON(r, &input); err != nil && !errors.Is(err, io.EOF) {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}

	at := time.Now()
	switch {
	case input.MessageID != "":
		ts, err := h.repo.MessageTimestamp(r.Context(), info.ID, input.MessageID)
		if err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		if ts == nil {
			httputil.JSONError(w, http.StatusBadRequest, "MESSAGE_NOT_IN_CHANNEL", "message does not belong to this channel")
			return
		}
		at = *ts
	case input.ReadAt != nil:
		at = *input.ReadAt
	}

	if err := h.repo.UpdateReadAt(r.Context(), info.ID, userID, at); err != nil {
		if errors.Is(err, ErrNotAMember) {
			httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "not a member of this channel")
			return
		}
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	unread, err := h.repo.UnreadCount(r.Context(), info.ID, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	// The badge just moved on this device; the caller's other sockets only learn
	// about it from this event.
	h.emitUnreadUpdate(info.WorkspaceID, userID, unread)

	httputil.JSON(w, http.StatusOK, unread)
}

// Unread reports how many messages the caller has not seen in a channel.
func (h *Handler) Unread(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	info, ok := h.requireChannelMember(w, r)
	if !ok {
		return
	}

	unread, err := h.repo.UnreadCount(r.Context(), info.ID, userID)
	if err != nil {
		if errors.Is(err, ErrNotAMember) {
			httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "not a member of this channel")
			return
		}
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusOK, unread)
}

// UpdatePrefs sets the per-channel mute and notification preference. Both
// columns were serialized to clients since the first migration with no endpoint
// able to change them.
func (h *Handler) UpdatePrefs(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	info, ok := h.requireChannelMember(w, r)
	if !ok {
		return
	}

	var input struct {
		Muted            *bool   `json:"muted"`
		NotificationPref *string `json:"notification_pref"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	if input.Muted == nil && input.NotificationPref == nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "muted or notification_pref is required")
		return
	}
	if input.NotificationPref != nil && !validNotificationPref(*input.NotificationPref) {
		httputil.JSONError(w, http.StatusBadRequest, "INVALID_NOTIFICATION_PREF", "notification_pref must be one of 'all', 'mentions', 'none', 'default'")
		return
	}

	member, err := h.repo.UpdateMemberPrefs(r.Context(), info.ID, userID, input.Muted, input.NotificationPref)
	if err != nil {
		if errors.Is(err, ErrNotAMember) {
			httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "not a member of this channel")
			return
		}
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusOK, member)
}

// ---------------------------------------------------------------------------
// Direct messages
// ---------------------------------------------------------------------------

func (h *Handler) CreateDM(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())
	wsID, ok := h.requireWorkspaceMember(w, r)
	if !ok {
		return
	}

	var input struct {
		UserIDs []string `json:"user_ids"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
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

	for _, uid := range others {
		member, err := h.az.IsWorkspaceMember(r.Context(), wsID, uid)
		if err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		if !member {
			httputil.JSONError(w, http.StatusBadRequest, "NOT_WORKSPACE_MEMBER", "user_id is not a workspace member")
			return
		}
		// A block has to stop the conversation from opening; filtering blocked
		// users out of search only hid them from the picker.
		blocked, err := h.az.IsBlocked(r.Context(), userID, uid)
		if err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		if blocked {
			httputil.JSONError(w, http.StatusForbidden, "BLOCKED", "cannot open a conversation with this user")
			return
		}
	}

	chType := TypeDM
	var dmKey *string
	if len(others) > 1 {
		// Group DMs are intentionally never deduped, so they carry no dm_key.
		chType = TypeGroupDM
	} else {
		k := DMKey(userID, others[0])
		dmKey = &k
	}

	ch := &Channel{
		ID:          uuid.NewString(),
		WorkspaceID: wsID,
		Type:        chType,
		CreatorID:   &userID,
	}

	participants := append([]string{userID}, others...)
	created, err := h.repo.EnsureDM(r.Context(), ch, dmKey, participants)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !created {
		// An adopted conversation was already announced when it was created.
		httputil.JSON(w, http.StatusOK, ch)
		return
	}

	// A DM is never public, so the relay addresses it to its participants —
	// which is how the other side's client learns the conversation exists
	// without a reload.
	h.emitChannelCreated(ch, participants)

	httputil.JSON(w, http.StatusCreated, ch)
}
