// Package workflow is automation: when something happens, do something.
//
// THE ENTIRE DESIGN IS SHAPED BY AT-LEAST-ONCE DELIVERY. Triggers arrive on
// durable JetStream consumers, which redeliver on any nak and after any crash.
// So every layer has an idempotency key:
//
//	the RUN is keyed on the triggering event's id, so a redelivered event finds
//	the run it already started rather than starting a second one;
//
//	each STEP records its effect before performing it, in the same transaction,
//	so a retry that reaches a step which already ran skips it.
//
// Without both, the automation posts the message twice and becomes the feature
// people turn off. With both, a retry is free and the worker can nak whenever
// it is unsure — which is what makes "when in doubt, retry" a safe policy.
package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
)

var (
	ErrNotFound  = errors.New("workflow: not found")
	ErrDuplicate = errors.New("workflow: this event already started a run")
	ErrTooMany   = errors.New("workflow: too many steps")
)

// Step kinds. A CLOSED set, and the closure is the point: an unknown kind is a
// step that does nothing, and a workflow that silently does nothing is worse
// than one that refuses to save.
//
// What is deliberately ABSENT and why:
//
//   - http_request. An outbound call to an arbitrary URL from inside the
//     customer's network is an SSRF primitive, and doing it safely needs an
//     egress allowlist, a credential vault and a redirect policy. Migration 061
//     is earmarked for that; it is not spent.
//   - create_issue / update_issue. issue.Repository.CreateIssue mints the id
//     server-side AND burns a number from issue_counters under FOR UPDATE, so a
//     retried step creates a second issue and skips a number — and a gap in
//     PROJ-13 is indistinguishable from a deletion, which is the property that
//     FOR UPDATE exists to protect. Until it accepts a caller-supplied id, an
//     issue action cannot be made idempotent, so it is not offered.
const (
	StepPostMessage = "post_message"
	StepNotify      = "notify"
	StepAddComment  = "add_comment"
)

func validStepKind(k string) bool {
	switch k {
	case StepPostMessage, StepNotify, StepAddComment:
		return true
	}
	return false
}

// Step is one action.
type Step struct {
	Kind string `json:"kind"`
	// Config is kind-specific and validated by the executor for that kind.
	Config map[string]any `json:"config"`
}

type Workflow struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	OwnerID     string     `json:"owner_id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Enabled     bool       `json:"enabled"`
	Version     int        `json:"version"`
	Steps       []Step     `json:"steps"`
	Triggers    []Trigger  `json:"triggers"`
	CreatedAt   time.Time  `json:"created_at"`
	ArchivedAt  *time.Time `json:"archived_at"`
}

type Trigger struct {
	EventType string         `json:"event_type"`
	Filter    map[string]any `json:"filter"`
}

type Run struct {
	ID         string `json:"id"`
	WorkflowID string `json:"workflow_id"`
	// OwnerID is WHO THE RUN ACTS AS, and every action authorizes against it.
	// A run with no owner must not execute: an action with only a workspace id
	// has no authority to check, so it would reach every object in the tenant.
	OwnerID     string `json:"owner_id"`
	WorkspaceID string `json:"workspace_id"`
	// Depth is how many workflow generations produced this run. 0 is a human
	// action; anything above MaxDepth is refused before it is queued.
	Depth int `json:"depth"`
	// RootRunID is the run at the top of the chain, so an operator sees the
	// whole cascade rather than one link of it.
	RootRunID  string     `json:"root_run_id"`
	EventType  string     `json:"event_type"`
	TriggerKey string     `json:"trigger_key"`
	Status     string     `json:"status"`
	Error      string     `json:"error"`
	Payload    []byte     `json:"payload"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// Save creates a workflow or adds an immutable version to an existing one.
//
// A new VERSION rather than an edit, because a run records which version it
// executed: without that, reading a two-week-old run tells you what the
// workflow does now, which is a guess dressed up as an audit trail.
func (r *Repository) Save(ctx context.Context, workspaceID, id, name, description string,
	steps []Step, triggers []Trigger, enabled bool, actorID string) (*Workflow, error) {

	if len(steps) > 20 {
		return nil, ErrTooMany
	}
	for i, s := range steps {
		if !validStepKind(s.Kind) {
			return nil, fmt.Errorf("workflow: step %d has unknown kind %q", i, s.Kind)
		}
	}
	for i, t := range triggers {
		if !eventShape.MatchString(t.EventType) {
			return nil, fmt.Errorf("workflow: trigger %d has invalid event type %q", i, t.EventType)
		}
	}

	stepsJSON, err := json.Marshal(nonNilSteps(steps))
	if err != nil {
		return nil, fmt.Errorf("encode steps: %w", err)
	}

	var wf Workflow
	err = database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		if id == "" {
			err := tx.QueryRow(ctx, `
				INSERT INTO workflows (workspace_id, name, description, enabled, created_by, owner_id)
				VALUES ($1, $2, $3, $4, $5, $5)
				RETURNING id::text, workspace_id::text, owner_id::text, name, description, enabled,
				          created_at, archived_at`,
				workspaceID, name, description, enabled, actorID,
			).Scan(&wf.ID, &wf.WorkspaceID, &wf.OwnerID, &wf.Name, &wf.Description, &wf.Enabled,
				&wf.CreatedAt, &wf.ArchivedAt)
			if err != nil {
				return fmt.Errorf("insert workflow: %w", err)
			}
		} else {
			// OWNER_ID MOVES TO THE SAVER. This is the whole of the
			// "you cannot automate what you could not do by hand" rule on the
			// edit path, and leaving it out made the rule enforce itself
			// against the wrong person: one admin could rewrite another
			// admin's steps and have them execute under the original owner's
			// capability. The victim's private-channel write, comment and
			// notify reach became the editor's, silently and repeatedly.
			//
			// Every save rewrites the steps — they come from the request, and
			// a new workflow_versions row is inserted below — so the saver is
			// unambiguously responsible for what the workflow now does. Making
			// them the owner is the only assignment under which the executor's
			// per-action authorization is a real constraint.
			err := tx.QueryRow(ctx, `
				UPDATE workflows
				   SET name = $3, description = $4, enabled = $5,
				       owner_id = $6, updated_at = NOW()
				 WHERE id = $1 AND workspace_id = $2 AND archived_at IS NULL
				RETURNING id::text, workspace_id::text, owner_id::text, name, description, enabled,
				          created_at, archived_at`,
				id, workspaceID, name, description, enabled, actorID,
			).Scan(&wf.ID, &wf.WorkspaceID, &wf.OwnerID, &wf.Name, &wf.Description, &wf.Enabled,
				&wf.CreatedAt, &wf.ArchivedAt)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return fmt.Errorf("update workflow: %w", err)
			}
		}

		// The version number comes from the existing rows rather than a
		// counter column: versions are append-only, so max+1 is exact, and it
		// cannot drift from what is actually stored.
		if err := tx.QueryRow(ctx, `
			INSERT INTO workflow_versions (workflow_id, version, steps, created_by)
			SELECT $1, COALESCE(MAX(version), 0) + 1, $2, $3
			  FROM workflow_versions WHERE workflow_id = $1
			RETURNING version`, wf.ID, stepsJSON, actorID).Scan(&wf.Version); err != nil {
			return fmt.Errorf("insert workflow version: %w", err)
		}

		// Triggers are replaced wholesale: they are a small set and describing
		// a diff would be more code than rewriting them.
		if _, err := tx.Exec(ctx, `DELETE FROM workflow_triggers WHERE workflow_id = $1`, wf.ID); err != nil {
			return fmt.Errorf("clear triggers: %w", err)
		}
		for _, t := range triggers {
			filter, err := json.Marshal(nonNilMap(t.Filter))
			if err != nil {
				return fmt.Errorf("encode trigger filter: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO workflow_triggers (workflow_id, event_type, filter)
				VALUES ($1, $2, $3)
				ON CONFLICT (workflow_id, event_type) DO UPDATE SET filter = EXCLUDED.filter`,
				wf.ID, t.EventType, filter); err != nil {
				return fmt.Errorf("insert trigger: %w", err)
			}
		}
		wf.Steps = steps
		wf.Triggers = triggers
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &wf, nil
}

// Matching is one workflow that wants an event.
type Matching struct {
	WorkflowID  string
	WorkspaceID string
	// OwnerID is carried from the match so the run records who it acts as at
	// the moment it is queued — not who owns the workflow when it executes,
	// which may be somebody else by then.
	OwnerID   string
	VersionID string
	Steps     []Step
	Filter    map[string]any
}

// MatchingWorkflows finds the enabled workflows triggered by an event.
//
// It reads the LATEST version per workflow in the same query, so a workflow
// edited between the event arriving and the run starting executes the version
// that existed when the run was created — not a half-saved one.
func (r *Repository) MatchingWorkflows(ctx context.Context, workspaceID, eventType string) ([]Matching, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT w.id::text, w.workspace_id::text, w.owner_id::text, v.id::text, v.steps, t.filter
		  FROM workflow_triggers t
		  JOIN workflows w ON w.id = t.workflow_id
		  JOIN LATERAL (
		      SELECT id, steps FROM workflow_versions
		       WHERE workflow_id = w.id ORDER BY version DESC LIMIT 1
		  ) v ON TRUE
		 WHERE t.event_type = $1
		   AND w.workspace_id = $2
		   AND w.enabled
		   AND w.archived_at IS NULL`, eventType, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("match workflows: %w", err)
	}
	defer rows.Close()

	out := make([]Matching, 0, 4)
	for rows.Next() {
		var m Matching
		var stepsJSON, filterJSON []byte
		if err := rows.Scan(&m.WorkflowID, &m.WorkspaceID, &m.OwnerID, &m.VersionID,
			&stepsJSON, &filterJSON); err != nil {
			return nil, fmt.Errorf("scan matching workflow: %w", err)
		}
		if err := json.Unmarshal(stepsJSON, &m.Steps); err != nil {
			return nil, fmt.Errorf("decode steps: %w", err)
		}
		if err := json.Unmarshal(filterJSON, &m.Filter); err != nil {
			return nil, fmt.Errorf("decode filter: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Loop guards.
//
// MaxDepth bounds a chain whose provenance we can follow. Three is enough for
// every legitimate cascade anybody has described — "a message files an issue,
// the issue notifies a channel" is two — and small enough that a runaway is
// stopped before it is expensive.
const MaxDepth = 3

// MaxRunsPerMinute is the blunt backstop, and it is the one that matters.
//
// Depth only works through producers that propagate the marker. Two workflows
// triggering each other through a pillar that propagates nothing, or a
// publisher added next year by somebody who never read this file, defeat it
// entirely. A cap on how fast ONE workflow may start runs needs no cooperation
// from anything in the middle.
//
// Set well above any plausible burst: a busy channel is a few messages a
// second, and a workflow that legitimately wants more than this is a workflow
// that should be batching.
const MaxRunsPerMinute = 120

// EnqueueRun creates a run for one workflow and one event.
//
// THE TRIGGER KEY IS THE IDEMPOTENCY KEY, and it comes from the event's own id
// rather than from anything this process generates. A redelivered event
// produces the same key, conflicts, and returns ErrDuplicate — which the
// consumer treats as success, because the work is already queued.
func (r *Repository) EnqueueRun(ctx context.Context, m Matching, eventType, triggerKey string,
	payload []byte) (string, error) {
	return r.EnqueueRunAt(ctx, m, eventType, triggerKey, payload, 0, "")
}

// ErrLoopGuard is a run refused because it would continue a runaway chain.
// Recorded as a rejection, not returned to the message bus: redelivering it
// produces the same refusal.
var ErrLoopGuard = errors.New("workflow: refused to continue a runaway chain")

// EnqueueRunAt is EnqueueRun with provenance.
//
// depth and rootRunID come from the event that caused it: a workflow's own
// output carries where it came from, so a descendant knows how deep it is.
func (r *Repository) EnqueueRunAt(ctx context.Context, m Matching, eventType, triggerKey string,
	payload []byte, depth int, rootRunID string) (string, error) {

	if triggerKey == "" {
		return "", errors.New("workflow: an event with no id cannot start a run safely")
	}
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	if depth > MaxDepth {
		return "", ErrLoopGuard
	}

	// THE RATE CAP, checked before the insert. A workflow that has started more
	// than MaxRunsPerMinute runs in the last minute is looping — no legitimate
	// trigger produces that — and the cheapest correct response is to stop
	// queueing rather than to queue and fail.
	var recent int
	if err := r.pool.QueryRow(ctx,
		`SELECT count(*) FROM workflow_runs
		  WHERE workflow_id = $1 AND created_at > NOW() - INTERVAL '1 minute'`,
		m.WorkflowID).Scan(&recent); err != nil {
		return "", fmt.Errorf("check workflow rate: %w", err)
	}
	if recent >= MaxRunsPerMinute {
		return "", ErrLoopGuard
	}

	var root any
	if rootRunID != "" {
		root = rootRunID
	}

	var runID string
	err := r.pool.QueryRow(ctx, `
		INSERT INTO workflow_runs
		       (workflow_id, version_id, workspace_id, trigger_key, event_type, payload,
		        depth, root_run_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (workflow_id, trigger_key) DO NOTHING
		RETURNING id::text`,
		m.WorkflowID, m.VersionID, m.WorkspaceID, triggerKey, eventType, payload,
		depth, root).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrDuplicate
	}
	if err != nil {
		return "", fmt.Errorf("enqueue run: %w", err)
	}
	return runID, nil
}

// RecordRejection notes that a trigger matched and was refused.
//
// The two ways automation fails invisibly are "it never fired" and "it fired
// and was dropped", and to a user staring at an enabled workflow they look
// identical. This is what tells them apart.
func (r *Repository) RecordRejection(ctx context.Context, workflowID, workspaceID, eventType, reason string) {
	_, _ = r.pool.Exec(ctx,
		`INSERT INTO workflow_trigger_rejections (workflow_id, workspace_id, event_type, reason)
		 VALUES ($1, $2, $3, $4)`, workflowID, workspaceID, eventType, reason)
}

// ClaimRun takes the oldest pending run, or returns ErrNotFound.
//
// FOR UPDATE SKIP LOCKED, not an advisory lock: several workers share the queue
// and each takes a different row, which is the same mechanism the scheduled
// message promoter uses. An advisory lock would serialize the whole queue
// behind one worker.
func (r *Repository) ClaimRun(ctx context.Context) (*Run, []Step, error) {
	var run Run
	var steps []Step

	err := database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		var stepsJSON []byte
		err := tx.QueryRow(ctx, `
			WITH claimed AS (
				SELECT id FROM workflow_runs
				 WHERE status = 'pending'
				 ORDER BY created_at
				 FOR UPDATE SKIP LOCKED
				 LIMIT 1
			)
			UPDATE workflow_runs r
			   SET status = 'running', started_at = NOW()
			  FROM claimed, workflow_versions v
			 WHERE r.id = claimed.id AND v.id = r.version_id
			RETURNING r.id::text, r.workflow_id::text, r.workspace_id::text, r.event_type,
			          r.trigger_key, r.status, r.error, r.payload, r.started_at, r.finished_at,
			          r.created_at, v.steps,
			          (SELECT w.owner_id::text FROM workflows w WHERE w.id = r.workflow_id),
			          r.depth, COALESCE(r.root_run_id::text, r.id::text)`,
		).Scan(&run.ID, &run.WorkflowID, &run.WorkspaceID, &run.EventType, &run.TriggerKey,
			&run.Status, &run.Error, &run.Payload, &run.StartedAt, &run.FinishedAt,
			&run.CreatedAt, &stepsJSON, &run.OwnerID, &run.Depth, &run.RootRunID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("claim run: %w", err)
		}
		return json.Unmarshal(stepsJSON, &steps)
	})
	if err != nil {
		return nil, nil, err
	}
	return &run, steps, nil
}

// ClaimEffect reserves a step's side effect.
//
// THE SAFETY PROPERTY OF THE WHOLE PACKAGE. It returns false when the effect
// was already recorded — which means a previous delivery performed it — and the
// caller must then SKIP the action rather than repeat it.
//
// The insert commits BEFORE the effect is performed, which is the deliberate
// direction: a crash between them loses the effect, and a workflow that
// occasionally fails to post a message is recoverable, while one that
// occasionally posts twice is what makes people disable automation.
func (r *Repository) ClaimEffect(ctx context.Context, runID string, stepIndex int, kind string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		INSERT INTO workflow_effects (run_id, step_index, kind)
		VALUES ($1, $2, $3)
		ON CONFLICT (run_id, step_index) DO NOTHING`, runID, stepIndex, kind)
	if err != nil {
		return false, fmt.Errorf("claim effect: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RecordEffectResult stores what an effect produced, so a retry can report the
// original result rather than an empty success.
func (r *Repository) RecordEffectResult(ctx context.Context, runID string, stepIndex int, result any) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return
	}
	_, _ = r.pool.Exec(ctx,
		`UPDATE workflow_effects SET result = $3 WHERE run_id = $1 AND step_index = $2`,
		runID, stepIndex, encoded)
}

func (r *Repository) FinishStep(ctx context.Context, runID string, index int, kind, status, errText string, output any) error {
	encoded, err := json.Marshal(nonNilAny(output))
	if err != nil {
		encoded = []byte("{}")
	}
	_, err = r.pool.Exec(ctx, `
		INSERT INTO workflow_step_runs (run_id, step_index, kind, status, error, output, started_at, finished_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW())
		ON CONFLICT (run_id, step_index) DO UPDATE
		   SET status = EXCLUDED.status, error = EXCLUDED.error,
		       output = EXCLUDED.output, finished_at = NOW()`,
		runID, index, kind, status, truncate(errText, 2000), encoded)
	if err != nil {
		return fmt.Errorf("record step: %w", err)
	}
	return nil
}

func (r *Repository) FinishRun(ctx context.Context, runID, status, errText string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE workflow_runs SET status = $2, error = $3, finished_at = NOW() WHERE id = $1`,
		runID, status, truncate(errText, 2000))
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	return nil
}

// ReleaseStaleRuns puts runs back that a worker claimed and never finished.
//
// A worker killed mid-run leaves its row 'running' forever, and nothing else
// would ever notice. Retrying is safe precisely because of workflow_effects:
// the steps that already ran are skipped.
func (r *Repository) ReleaseStaleRuns(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE workflow_runs SET status = 'pending', started_at = NULL
		 WHERE status = 'running' AND started_at < NOW() - $1::interval`,
		fmt.Sprintf("%d seconds", int(olderThan.Seconds())))
	if err != nil {
		return 0, fmt.Errorf("release stale runs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Matches reports whether an event payload satisfies a trigger's filter.
//
// Equality on top-level string fields only. Deliberately not a query language:
// the moment a filter can express "or" and "not" it needs its own evaluator,
// its own tests and its own error reporting, and this is a product feature
// rather than a database.
func Matches(filter map[string]any, payload map[string]any) bool {
	for k, want := range filter {
		got, ok := payload[k]
		if !ok {
			return false
		}
		if fmt.Sprint(want) != fmt.Sprint(got) {
			return false
		}
	}
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func nonNilSteps(s []Step) []Step {
	if s == nil {
		return []Step{}
	}
	return s
}

func nonNilMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func nonNilAny(v any) any {
	if v == nil {
		return map[string]any{}
	}
	return v
}

// eventShape mirrors the column's CHECK. Refusing here rather than at the
// database means the caller is told which trigger is wrong.
var eventShape = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)
