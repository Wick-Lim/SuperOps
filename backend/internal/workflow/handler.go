package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/audit"
	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

// maxWorkflowBody bounds a save. The steps column is capped at 64 KiB by the
// schema; this is the JSON envelope around it.
const maxWorkflowBody = 256 << 10

type Handler struct {
	repo   *Repository
	pool   *pgxpool.Pool
	authz  *authz.Checker
	audit  *audit.Service
	logger *slog.Logger
}

func NewHandler(pool *pgxpool.Pool, az *authz.Checker, auditor *audit.Service, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{repo: NewRepository(pool), pool: pool, authz: az, audit: auditor, logger: logger}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("POST /api/v1/workspaces/{workspace_id}/workflows", authMw(http.HandlerFunc(h.Create)))
	mux.Handle("GET /api/v1/workspaces/{workspace_id}/workflows", authMw(http.HandlerFunc(h.List)))
	mux.Handle("GET /api/v1/workflows/{workflow_id}", authMw(http.HandlerFunc(h.Get)))
	mux.Handle("PATCH /api/v1/workflows/{workflow_id}", authMw(http.HandlerFunc(h.Update)))
	mux.Handle("DELETE /api/v1/workflows/{workflow_id}", authMw(http.HandlerFunc(h.Archive)))
	mux.Handle("GET /api/v1/workflows/{workflow_id}/runs", authMw(http.HandlerFunc(h.ListRuns)))
	mux.Handle("GET /api/v1/workflows/{workflow_id}/rejections", authMw(http.HandlerFunc(h.ListRejections)))
	mux.Handle("GET /api/v1/runs/{run_id}", authMw(http.HandlerFunc(h.GetRun)))
	// The catalogue, so a client offers exactly the steps THIS deployment can
	// perform rather than a hardcoded list that drifts from the executor.
	mux.Handle("GET /api/v1/workflow-steps", authMw(http.HandlerFunc(h.Catalogue)))
}

// AUTHORIZATION, stated once because every handler below relies on it.
//
// A workflow is workspace-scoped and there is no per-workflow ACL. Reading the
// list needs workspace read; creating or changing one needs workspace ADMIN.
//
// Admin rather than write is deliberate and it follows from the execution
// model: a workflow runs as its OWNER, so saving one is deciding that an action
// will be taken under somebody's authority, repeatedly, without them being
// present. That is an administrative act even when the action itself is
// ordinary — and the owner is always the saver, so it can never be used to act
// as somebody else.
func (h *Handler) mayAdmin(w http.ResponseWriter, r *http.Request, workspaceID string) bool {
	got, err := h.authz.Capability(r.Context(), authz.UserSubject(authctx.UserID(r.Context())),
		authz.WorkspaceObject(workspaceID))
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return false
	}
	if !got.Implies(authz.CapAdmin) {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN",
			"only a workspace admin may change automation")
		return false
	}
	return true
}

// mayRead gates every workflow read, and it requires ADMIN — the same
// capability as writing.
//
// It used to require workspace CapRead, which RoleGuest holds. A workflow's step
// configs and its runs' step outputs are returned verbatim, so the least
// privileged role in the tenant could read the ids of private channels, the user
// ids of notify targets, the object ids of commented objects and the literal
// message bodies automation posts into them — every one of which is writable
// only by an admin and readable by hand only by someone in the channel.
//
// Read and write match because automation is one surface, not two. A workflow
// executes under its owner's authority without them present; describing what it
// will do is describing an administrator's standing decision, and there is no
// coherent reading in which that is less sensitive than the channel it posts to.
func (h *Handler) mayRead(w http.ResponseWriter, r *http.Request, workspaceID string) bool {
	got, err := h.authz.Capability(r.Context(), authz.UserSubject(authctx.UserID(r.Context())),
		authz.WorkspaceObject(workspaceID))
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return false
	}
	if !got.Implies(authz.CapAdmin) {
		// 404-shaped, as everywhere else: a 403 would confirm the workflow
		// exists to somebody who may not know that.
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return false
	}
	return true
}

// workspaceOf resolves a workflow's tenant. Every by-id route goes through it,
// so a caller cannot reach another tenant's workflow by guessing a uuid.
func (h *Handler) workspaceOf(ctx context.Context, workflowID string) (string, error) {
	var ws string
	err := h.pool.QueryRow(ctx,
		`SELECT workspace_id::text FROM workflows WHERE id = $1`, workflowID).Scan(&ws)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return ws, err
}

type saveRequest struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Enabled     bool      `json:"enabled"`
	Steps       []Step    `json:"steps"`
	Triggers    []Trigger `json:"triggers"`
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	workspaceID, err := httputil.RequirePathParam(r, "workspace_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	if !h.mayAdmin(w, r, workspaceID) {
		return
	}

	var req saveRequest
	if err := httputil.DecodeJSONLimit(r, &req, maxWorkflowBody); err != nil {
		httputil.HandleError(w, err)
		return
	}

	actor := authctx.UserID(r.Context())
	wf, err := h.repo.Save(r.Context(), workspaceID, "", req.Name, req.Description,
		req.Steps, req.Triggers, req.Enabled, actor)
	if err != nil {
		h.saveFailed(w, err)
		return
	}
	h.record(r.Context(), workspaceID, "workflow.created", wf.ID,
		map[string]any{"name": wf.Name, "enabled": wf.Enabled})
	httputil.JSON(w, http.StatusCreated, wf)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	id, err := httputil.RequirePathParam(r, "workflow_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	workspaceID, err := h.workspaceOf(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !h.mayAdmin(w, r, workspaceID) {
		return
	}

	var req saveRequest
	if err := httputil.DecodeJSONLimit(r, &req, maxWorkflowBody); err != nil {
		httputil.HandleError(w, err)
		return
	}

	// Save appends a VERSION rather than mutating the existing one, so a run
	// already queued still executes what it was queued with.
	wf, err := h.repo.Save(r.Context(), workspaceID, id, req.Name, req.Description,
		req.Steps, req.Triggers, req.Enabled, authctx.UserID(r.Context()))
	if err != nil {
		h.saveFailed(w, err)
		return
	}
	h.record(r.Context(), workspaceID, "workflow.updated", wf.ID,
		map[string]any{"version": wf.Version, "enabled": wf.Enabled})
	httputil.JSON(w, http.StatusOK, wf)
}

func (h *Handler) Archive(w http.ResponseWriter, r *http.Request) {
	id, err := httputil.RequirePathParam(r, "workflow_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	workspaceID, err := h.workspaceOf(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !h.mayAdmin(w, r, workspaceID) {
		return
	}

	// Archived AND disabled, in one statement. Archiving without disabling
	// would leave a workflow the UI has hidden still firing on every event —
	// the worst possible combination, because nobody would think to look for it.
	if _, err := h.pool.Exec(r.Context(),
		`UPDATE workflows SET archived_at = NOW(), enabled = FALSE, updated_at = NOW()
		  WHERE id = $1 AND archived_at IS NULL`, id); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	h.record(r.Context(), workspaceID, "workflow.archived", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	workspaceID, err := httputil.RequirePathParam(r, "workspace_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	if !h.mayRead(w, r, workspaceID) {
		return
	}

	rows, err := h.pool.Query(r.Context(), `
		SELECT w.id::text, w.workspace_id::text, w.owner_id::text, w.name, w.description,
		       w.enabled, w.created_at, w.archived_at,
		       COALESCE(v.version, 0),
		       COALESCE(v.steps, '[]'::jsonb),
		       COALESCE(t.triggers, '[]'::jsonb)
		  FROM workflows w
		  LEFT JOIN LATERAL (
		      SELECT version, steps FROM workflow_versions
		       WHERE workflow_id = w.id ORDER BY version DESC LIMIT 1
		  ) v ON TRUE
		  LEFT JOIN LATERAL (
		      SELECT jsonb_agg(jsonb_build_object('event_type', event_type, 'filter', filter)) AS triggers
		        FROM workflow_triggers WHERE workflow_id = w.id
		  ) t ON TRUE
		 WHERE w.workspace_id = $1 AND w.archived_at IS NULL
		 ORDER BY w.created_at DESC`, workspaceID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	defer rows.Close()

	out := make([]Workflow, 0, 16)
	for rows.Next() {
		wf, err := scanWorkflow(rows)
		if err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		out = append(out, *wf)
	}
	if err := rows.Err(); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusOK, out)
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := httputil.RequirePathParam(r, "workflow_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	workspaceID, err := h.workspaceOf(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !h.mayRead(w, r, workspaceID) {
		return
	}

	row := h.pool.QueryRow(r.Context(), `
		SELECT w.id::text, w.workspace_id::text, w.owner_id::text, w.name, w.description,
		       w.enabled, w.created_at, w.archived_at,
		       COALESCE(v.version, 0),
		       COALESCE(v.steps, '[]'::jsonb),
		       COALESCE(t.triggers, '[]'::jsonb)
		  FROM workflows w
		  LEFT JOIN LATERAL (
		      SELECT version, steps FROM workflow_versions
		       WHERE workflow_id = w.id ORDER BY version DESC LIMIT 1
		  ) v ON TRUE
		  LEFT JOIN LATERAL (
		      SELECT jsonb_agg(jsonb_build_object('event_type', event_type, 'filter', filter)) AS triggers
		        FROM workflow_triggers WHERE workflow_id = w.id
		  ) t ON TRUE
		 WHERE w.id = $1`, id)
	wf, err := scanWorkflow(row)
	if err != nil {
		h.fail(w, err)
		return
	}
	httputil.JSON(w, http.StatusOK, wf)
}

// RunSummary is a run as the history list shows it.
type RunSummary struct {
	ID         string     `json:"id"`
	WorkflowID string     `json:"workflow_id"`
	EventType  string     `json:"event_type"`
	Status     string     `json:"status"`
	Error      string     `json:"error"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

// StepSummary is one step of one run.
type StepSummary struct {
	StepIndex  int             `json:"step_index"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	Error      string          `json:"error"`
	Output     json.RawMessage `json:"output"`
	FinishedAt *time.Time      `json:"finished_at"`
}

// ListRuns is the history. It is the ONLY place a user can find out whether
// their automation is working, so it carries the error text verbatim rather
// than a status alone.
func (h *Handler) ListRuns(w http.ResponseWriter, r *http.Request) {
	id, err := httputil.RequirePathParam(r, "workflow_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	workspaceID, err := h.workspaceOf(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !h.mayRead(w, r, workspaceID) {
		return
	}

	page, err := httputil.ParsePagination(r)
	if err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "INVALID_CURSOR", err.Error())
		return
	}
	hasCur := !page.Cursor.IsZero()
	cursorID := page.Cursor.ID
	if !hasCur {
		cursorID = "00000000-0000-0000-0000-000000000000"
	}

	rows, err := h.pool.Query(r.Context(), `
		SELECT id::text, workflow_id::text, event_type, status, error,
		       started_at, finished_at, created_at
		  FROM workflow_runs
		 WHERE workflow_id = $1
		   AND (NOT $2::bool OR (created_at, id) < ($3::timestamptz, $4::uuid))
		 ORDER BY created_at DESC, id DESC
		 LIMIT $5`, id, hasCur, page.Cursor.CreatedAt, cursorID, page.Limit)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	defer rows.Close()

	out := make([]RunSummary, 0, page.Limit)
	for rows.Next() {
		var s RunSummary
		if err := rows.Scan(&s.ID, &s.WorkflowID, &s.EventType, &s.Status, &s.Error,
			&s.StartedAt, &s.FinishedAt, &s.CreatedAt); err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	var next string
	hasMore := page.Limit > 0 && len(out) == page.Limit
	if hasMore {
		last := out[len(out)-1]
		next = httputil.EncodeCursor(last.CreatedAt, last.ID)
	}
	httputil.JSONList(w, http.StatusOK, out, next, hasMore)
}

// GetRun returns one run with its steps.
func (h *Handler) GetRun(w http.ResponseWriter, r *http.Request) {
	runID, err := httputil.RequirePathParam(r, "run_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}

	var run RunSummary
	var workspaceID string
	err = h.pool.QueryRow(r.Context(), `
		SELECT id::text, workflow_id::text, workspace_id::text, event_type, status, error,
		       started_at, finished_at, created_at
		  FROM workflow_runs WHERE id = $1`, runID,
	).Scan(&run.ID, &run.WorkflowID, &workspaceID, &run.EventType, &run.Status, &run.Error,
		&run.StartedAt, &run.FinishedAt, &run.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return
	}
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !h.mayRead(w, r, workspaceID) {
		return
	}

	rows, err := h.pool.Query(r.Context(), `
		SELECT step_index, kind, status, error, output, finished_at
		  FROM workflow_step_runs WHERE run_id = $1 ORDER BY step_index`, runID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	defer rows.Close()

	steps := make([]StepSummary, 0, 8)
	for rows.Next() {
		var s StepSummary
		if err := rows.Scan(&s.StepIndex, &s.Kind, &s.Status, &s.Error, &s.Output, &s.FinishedAt); err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		steps = append(steps, s)
	}
	httputil.JSON(w, http.StatusOK, map[string]any{"run": run, "steps": steps})
}

// Rejection is a trigger that matched and was refused.
type Rejection struct {
	EventType string    `json:"event_type"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

// ListRejections answers "it looks enabled and nothing happens".
//
// Without it the two ways automation fails invisibly — never fired, and fired
// then dropped — are indistinguishable to somebody staring at a workflow whose
// toggle is on.
func (h *Handler) ListRejections(w http.ResponseWriter, r *http.Request) {
	id, err := httputil.RequirePathParam(r, "workflow_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	workspaceID, err := h.workspaceOf(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !h.mayRead(w, r, workspaceID) {
		return
	}

	rows, err := h.pool.Query(r.Context(), `
		SELECT event_type, reason, created_at FROM workflow_trigger_rejections
		 WHERE workflow_id = $1 ORDER BY created_at DESC LIMIT 100`, id)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	defer rows.Close()

	out := make([]Rejection, 0, 16)
	for rows.Next() {
		var x Rejection
		if err := rows.Scan(&x.EventType, &x.Reason, &x.CreatedAt); err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		out = append(out, x)
	}
	httputil.JSON(w, http.StatusOK, out)
}

// StepDescriptor tells a client what one step kind needs.
type StepDescriptor struct {
	Kind        string   `json:"kind"`
	DisplayName string   `json:"display_name"`
	Fields      []string `json:"fields"`
}

// Catalogue is the step vocabulary and the trigger vocabulary this deployment
// can actually serve.
//
// Served rather than hardcoded in the client for the same reason the Drive
// registry is: a client with its own list offers steps the executor has no
// adapter for, and the user finds out when the run fails.
func (h *Handler) Catalogue(w http.ResponseWriter, _ *http.Request) {
	httputil.JSON(w, http.StatusOK, map[string]any{
		"steps": []StepDescriptor{
			{Kind: StepPostMessage, DisplayName: "Post a message", Fields: []string{"channel_id", "body"}},
			{Kind: StepNotify, DisplayName: "Notify someone", Fields: []string{"user_id", "title", "body"}},
			{Kind: StepAddComment, DisplayName: "Add a comment", Fields: []string{"object_type", "object_id", "body"}},
		},
		// The subjects the trigger consumer is actually bound to. A client that
		// offered anything else would let somebody save a workflow that can
		// never fire.
		"triggers": []string{
			"message.created", "message.updated",
			"channel.member_added",
			"file.uploaded", "file.updated",
		},
	})
}

// ---------------------------------------------------------------------------

type scanner interface {
	Scan(dest ...any) error
}

func scanWorkflow(row scanner) (*Workflow, error) {
	var wf Workflow
	var stepsJSON, triggersJSON []byte
	if err := row.Scan(&wf.ID, &wf.WorkspaceID, &wf.OwnerID, &wf.Name, &wf.Description,
		&wf.Enabled, &wf.CreatedAt, &wf.ArchivedAt, &wf.Version, &stepsJSON, &triggersJSON); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal(stepsJSON, &wf.Steps); err != nil {
		return nil, err
	}
	// jsonb_agg over an empty set is NULL, which decodes to a nil slice; the
	// COALESCE above turns it into '[]' so a client never sees null where it
	// expects a list.
	if err := json.Unmarshal(triggersJSON, &wf.Triggers); err != nil {
		return nil, err
	}
	if wf.Steps == nil {
		wf.Steps = []Step{}
	}
	if wf.Triggers == nil {
		wf.Triggers = []Trigger{}
	}
	return &wf, nil
}

func (h *Handler) saveFailed(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "not found")
	case errors.Is(err, ErrTooMany):
		httputil.JSONError(w, http.StatusBadRequest, "TOO_MANY_STEPS",
			"a workflow may have at most 20 steps")
	default:
		// A validation failure names the step or trigger that is wrong, which
		// is the whole reason validation happens in Go rather than at the
		// database's CHECK constraints.
		httputil.JSONError(w, http.StatusBadRequest, "INVALID_WORKFLOW", err.Error())
	}
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return
	}
	httputil.HandleError(w, httputil.NewInternal(err))
}

func (h *Handler) record(ctx context.Context, workspaceID, action, id string, meta map[string]any) {
	if h.audit == nil {
		return
	}
	h.audit.Try(ctx, audit.Entry{
		WorkspaceID:  workspaceID,
		ActorID:      authctx.UserID(ctx),
		Action:       action,
		ResourceType: "workflow",
		ResourceID:   id,
		Metadata:     meta,
	})
}
