package issue

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/audit"
	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/inbox"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

// Notifier is the inbox, narrowed. docs/plans/README.md ruling 1: a pillar
// publishes inbox.Request and adds zero durables and zero enum values.
type Notifier interface {
	Deliver(ctx context.Context, req inbox.Request) error
}

type Handler struct {
	repo     *Repository
	authz    *authz.Checker
	notifier Notifier
	audit    *audit.Service
}

func NewHandler(pool *pgxpool.Pool, az *authz.Checker, notifier Notifier, auditor *audit.Service) *Handler {
	return &Handler{repo: NewRepository(pool, az), authz: az, notifier: notifier, audit: auditor}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("GET /api/v1/workspaces/{workspace_id}/projects", authMw(http.HandlerFunc(h.ListProjects)))
	mux.Handle("POST /api/v1/workspaces/{workspace_id}/projects", authMw(http.HandlerFunc(h.CreateProject)))
	mux.Handle("GET /api/v1/projects/{project_id}", authMw(http.HandlerFunc(h.GetProject)))
	mux.Handle("GET /api/v1/projects/{project_id}/states", authMw(http.HandlerFunc(h.ListStates)))
	mux.Handle("GET /api/v1/projects/{project_id}/issues", authMw(http.HandlerFunc(h.ListIssues)))
	mux.Handle("POST /api/v1/projects/{project_id}/issues", authMw(http.HandlerFunc(h.CreateIssue)))
	mux.Handle("GET /api/v1/issues/{issue_id}", authMw(http.HandlerFunc(h.GetIssue)))
	mux.Handle("PATCH /api/v1/issues/{issue_id}", authMw(http.HandlerFunc(h.UpdateIssue)))
	mux.Handle("POST /api/v1/issues/{issue_id}/move", authMw(http.HandlerFunc(h.MoveIssue)))
}

// authorizeProject is the 404 gate. Listing then filters per row against
// acl_key, because this check is a SUFFICIENT condition and not the filter: it
// is blind to an issue shared individually with somebody who cannot read the
// project.
func (h *Handler) authorizeProject(w http.ResponseWriter, r *http.Request, want authz.Capability) (string, bool) {
	id, err := httputil.RequirePathParam(r, "project_id")
	if err != nil {
		httputil.HandleError(w, err)
		return "", false
	}
	return id, h.check(w, r, authz.ProjectObject(id), want)
}

func (h *Handler) check(w http.ResponseWriter, r *http.Request, obj authz.ObjectRef, want authz.Capability) bool {
	got, err := h.authz.Capability(r.Context(), authz.UserSubject(authctx.UserID(r.Context())), obj)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return false
	}
	switch {
	case !got.Implies(authz.CapRead):
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return false
	case !got.Implies(want):
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "insufficient permission")
		return false
	}
	return true
}

func (h *Handler) ListProjects(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workspaceID, err := httputil.RequirePathParam(r, "workspace_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	if !h.check(w, r, authz.WorkspaceObject(workspaceID), authz.CapRead) {
		return
	}
	keys, err := h.authz.KeysFor(ctx, authz.UserSubject(authctx.UserID(ctx)), workspaceID)
	if err != nil {
		fail(w, err)
		return
	}
	projects, err := h.repo.Projects(ctx, workspaceID, keys,
		httputil.QueryParam(r, "archived") == "true")
	if err != nil {
		fail(w, err)
		return
	}
	httputil.JSON(w, http.StatusOK, projects)
}

func (h *Handler) CreateProject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workspaceID, err := httputil.RequirePathParam(r, "workspace_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	var req struct {
		Name        string `json:"name"`
		Key         string `json:"key"`
		Description string `json:"description"`
	}
	if err := httputil.DecodeJSON(r, &req); err != nil {
		httputil.HandleError(w, err)
		return
	}
	// Write on the WORKSPACE: creating a project is a write into it.
	if !h.check(w, r, authz.WorkspaceObject(workspaceID), authz.CapWrite) {
		return
	}

	p, err := h.repo.CreateProject(ctx, workspaceID, req.Name, req.Key, req.Description,
		authctx.UserID(ctx))
	if err != nil {
		fail(w, err)
		return
	}
	h.record(ctx, workspaceID, "project.created", "project", p.ID,
		map[string]interface{}{"name": p.Name, "key": p.Key})
	httputil.JSON(w, http.StatusCreated, p)
}

func (h *Handler) GetProject(w http.ResponseWriter, r *http.Request) {
	id, ok := h.authorizeProject(w, r, authz.CapRead)
	if !ok {
		return
	}
	p, err := h.repo.Project(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	if p == nil {
		fail(w, ErrNotFound)
		return
	}
	httputil.JSON(w, http.StatusOK, p)
}

func (h *Handler) ListStates(w http.ResponseWriter, r *http.Request) {
	id, ok := h.authorizeProject(w, r, authz.CapRead)
	if !ok {
		return
	}
	states, err := h.repo.States(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	httputil.JSON(w, http.StatusOK, states)
}

func (h *Handler) ListIssues(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	projectID, ok := h.authorizeProject(w, r, authz.CapRead)
	if !ok {
		return
	}
	p, err := h.repo.Project(ctx, projectID)
	if err != nil || p == nil {
		fail(w, orNotFound(err))
		return
	}
	keys, err := h.authz.KeysFor(ctx, authz.UserSubject(authctx.UserID(ctx)), p.WorkspaceID)
	if err != nil {
		fail(w, err)
		return
	}

	issues, nextRank, nextID, hasMore, err := h.repo.List(ctx, keys, ListOptions{
		ProjectID:  projectID,
		StateID:    httputil.QueryParam(r, "state_id"),
		CursorRank: httputil.QueryParam(r, "after_rank"),
		CursorID:   httputil.QueryParam(r, "after_id"),
	})
	if err != nil {
		fail(w, err)
		return
	}

	// The cursor is (rank, id), not the usual (created_at, id): the list is
	// ordered by RANK, and paginating a rank-ordered list by time skips and
	// repeats rows at every page boundary. It is two explicit query parameters
	// rather than an opaque token so it stays obviously different from the
	// product's other cursors.
	httputil.JSON(w, http.StatusOK, map[string]interface{}{
		"issues":   issues,
		"next":     map[string]string{"after_rank": nextRank, "after_id": nextID},
		"has_more": hasMore,
	})
}

func (h *Handler) CreateIssue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	projectID, ok := h.authorizeProject(w, r, authz.CapWrite)
	if !ok {
		return
	}
	var req struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		StateID     string `json:"state_id"`
		AssigneeID  string `json:"assignee_id"`
		Priority    int    `json:"priority"`
	}
	if err := httputil.DecodeJSON(r, &req); err != nil {
		httputil.HandleError(w, err)
		return
	}

	i, err := h.repo.CreateIssue(ctx, projectID, req.Title, req.Description,
		req.StateID, req.AssigneeID, authctx.UserID(ctx), req.Priority)
	if err != nil {
		fail(w, err)
		return
	}
	h.record(ctx, i.WorkspaceID, "issue.created", "issue", i.ID,
		map[string]interface{}{"key": i.Key, "title": i.Title})
	h.notifyAssigned(ctx, i, nil)
	httputil.JSON(w, http.StatusCreated, i)
}

func (h *Handler) GetIssue(w http.ResponseWriter, r *http.Request) {
	id, err := httputil.RequirePathParam(r, "issue_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	if !h.check(w, r, authz.IssueObject(id), authz.CapRead) {
		return
	}
	i, err := h.repo.Issue(r.Context(), id)
	if err != nil || i == nil {
		fail(w, orNotFound(err))
		return
	}
	httputil.JSON(w, http.StatusOK, i)
}

func (h *Handler) UpdateIssue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := httputil.RequirePathParam(r, "issue_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	var req struct {
		Title       *string `json:"title"`
		Description *string `json:"description"`
		AssigneeID  *string `json:"assignee_id"`
		Priority    *int    `json:"priority"`
	}
	if err := httputil.DecodeJSON(r, &req); err != nil {
		httputil.HandleError(w, err)
		return
	}
	if !h.check(w, r, authz.IssueObject(id), authz.CapWrite) {
		return
	}

	before, err := h.repo.Issue(ctx, id)
	if err != nil || before == nil {
		fail(w, orNotFound(err))
		return
	}
	after, err := h.repo.Update(ctx, id, req.Title, req.Description, req.AssigneeID,
		req.Priority, authctx.UserID(ctx))
	if err != nil {
		fail(w, err)
		return
	}
	h.record(ctx, after.WorkspaceID, "issue.updated", "issue", after.ID, nil)
	h.notifyAssigned(ctx, after, before.AssigneeID)
	httputil.JSON(w, http.StatusOK, after)
}

func (h *Handler) MoveIssue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := httputil.RequirePathParam(r, "issue_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	var req struct {
		// The NEIGHBOURS, not a rank. The client knows what it dropped between;
		// it does not know the rank algebra, and a client-computed rank would be
		// a second implementation of it against a board that may have moved.
		AfterID  string `json:"after_id"`
		BeforeID string `json:"before_id"`
		StateID  string `json:"state_id"`
	}
	if err := httputil.DecodeJSON(r, &req); err != nil {
		httputil.HandleError(w, err)
		return
	}
	if !h.check(w, r, authz.IssueObject(id), authz.CapWrite) {
		return
	}

	moved, err := h.repo.Move(ctx, id, req.AfterID, req.BeforeID, req.StateID, authctx.UserID(ctx))
	if err != nil {
		fail(w, err)
		return
	}
	h.record(ctx, moved.WorkspaceID, "issue.moved", "issue", moved.ID,
		map[string]interface{}{"state_id": moved.StateID})
	httputil.JSON(w, http.StatusOK, moved)
}

// notifyAssigned tells somebody an issue is theirs.
//
// Ruling 1, applied: the recipient list is one person, explicit and directed;
// suppression happens HERE (assigning to yourself notifies nobody); the kind is
// a TEXT string with no enum behind it.
func (h *Handler) notifyAssigned(ctx context.Context, i *Issue, previous *string) {
	if h.notifier == nil || i == nil || i.AssigneeID == nil {
		return
	}
	actor := authctx.UserID(ctx)
	if *i.AssigneeID == actor {
		// Assigning something to yourself is not news.
		return
	}
	if previous != nil && *previous == *i.AssigneeID {
		// Unchanged: a title edit must not re-notify the assignee.
		return
	}

	_ = h.notifier.Deliver(ctx, inbox.Request{
		WorkspaceID: i.WorkspaceID,
		Kind:        "issue.assigned",
		ObjectType:  "issue",
		ObjectID:    i.ID,
		// Subject and object are the same for an issue: reassigning twice is one
		// badge, not two.
		SubjectType: "issue",
		SubjectID:   i.ID,
		ActorID:     actor,
		Title:       i.Key + " " + i.Title,
		Body:        "assigned to you",
		Data:        map[string]string{"issue_id": i.ID, "project_id": i.ProjectID},
		Recipients:  []string{*i.AssigneeID},
	})
}

func (h *Handler) record(ctx context.Context, workspaceID, action, resourceType, resourceID string, meta map[string]interface{}) {
	if h.audit == nil {
		return
	}
	h.audit.Try(ctx, audit.Entry{
		WorkspaceID:  workspaceID,
		ActorID:      authctx.UserID(ctx),
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Metadata:     meta,
	})
}

func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "not found")
	case errors.Is(err, ErrDuplicateKey):
		httputil.JSONError(w, http.StatusConflict, "PROJECT_KEY_TAKEN", err.Error())
	case errors.Is(err, ErrCrossProject):
		httputil.JSONError(w, http.StatusConflict, "CROSS_PROJECT", err.Error())
	case errors.Is(err, ErrStateInUse):
		httputil.JSONError(w, http.StatusConflict, "STATE_IN_USE", err.Error())
	case errors.Is(err, ErrProjectArchived):
		httputil.JSONError(w, http.StatusConflict, "PROJECT_ARCHIVED", err.Error())
	case errors.Is(err, ErrSelfRelation), errors.Is(err, ErrBadInput):
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
	case errors.Is(err, ErrRankExhausted):
		// The caller's remedy is to retry: Move renormalises and retries once
		// internally, so reaching here means it failed twice.
		httputil.JSONError(w, http.StatusConflict, "RANK_EXHAUSTED",
			"the ordering needs renormalising; try again")
	default:
		httputil.HandleError(w, httputil.NewInternal(err))
	}
}

func orNotFound(err error) error {
	if err != nil {
		return err
	}
	return ErrNotFound
}
