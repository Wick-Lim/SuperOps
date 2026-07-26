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
	"strings"
	"time"
	"unicode/utf8"

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
// Patch is Save for a PARTIAL update: it reads the current workflow, lays the
// caller's non-nil fields over it, and writes — all under one row lock.
//
// The handler did this with a read on the pool followed by Save in its own
// transaction, which is a lost update. Two admins in two tabs, one saving new
// steps and one renaming: both read the same state, the second commit writes
// its stale copy of what it did not send, and because Save APPENDS a version
// the reverted content becomes the version that runs. Reproduced in 5 of 40
// paired concurrent requests.
//
// FOR NO KEY UPDATE is what serialises them. The second request blocks until
// the first commits and then reads what the first wrote, so a rename lands on
// top of new steps instead of underneath them.
//
// NO KEY, not plain FOR UPDATE. Both self-conflict, so the mutual exclusion
// between two Patches is identical — but FOR UPDATE also conflicts with
// FOR KEY SHARE, which is exactly the lock a foreign-key check takes. Four
// tables reference workflows(id), so for the life of a PATCH every event that
// would start a run of that workflow stalled on its FK check. Demonstrated:
// with FOR UPDATE held, `SELECT … FROM ONLY workflows WHERE id = $1 FOR KEY
// SHARE` times out; with FOR NO KEY UPDATE it passes immediately.
//
// The step engine returns that error so JetStream redelivers, which is only
// delay — but RecordRejection discards its error, so a trigger rejection
// landing in that window vanished, and that table is the only thing telling an
// author why nothing fired.
func (r *Repository) Patch(ctx context.Context, workspaceID, id string, p PatchFields,
	actorID string, load func(context.Context, pgx.Tx, string) (*Workflow, error),
) (*Workflow, *Workflow, error) {

	var current, saved *Workflow
	err := database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		var locked string
		err := tx.QueryRow(ctx,
			`SELECT id::text FROM workflows
			  WHERE id = $1 AND workspace_id = $2 AND archived_at IS NULL
			  FOR NO KEY UPDATE`, id, workspaceID).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock workflow: %w", err)
		}

		cur, err := load(ctx, tx, id)
		if err != nil {
			return err
		}
		current = cur

		name, description, enabled := cur.Name, cur.Description, cur.Enabled
		steps, triggers := cur.Steps, cur.Triggers
		if p.Name != nil {
			name = *p.Name
		}
		if p.Description != nil {
			description = *p.Description
		}
		if p.Enabled != nil {
			enabled = *p.Enabled
		}
		if p.Steps != nil {
			steps = *p.Steps
		}
		if p.Triggers != nil {
			triggers = *p.Triggers
		}

		saved, err = r.saveTx(ctx, tx, workspaceID, id, name, description,
			steps, triggers, enabled, actorID)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return current, saved, nil
}

// PatchFields is a partial workflow, and the PATCH route decodes straight into
// it.
//
// EVERY FIELD IS OPTIONAL, which is the difference between PATCH and PUT and
// the reason this type exists. The route used to decode into the same struct
// POST does, so an omitted `steps` arrived as an empty slice and Save — which
// writes a new version from whatever it is handed and replaces the triggers
// wholesale — deleted the automation. A body as small as {"name": "renamed"}
// left a workflow that still existed, still said enabled, and did nothing.
//
// A nil field is one the caller did not send and must not change. A non-nil
// EMPTY slice is somebody deliberately emptying a workflow, and must still
// work — which is why these are pointers rather than a merge helper.
type PatchFields struct {
	Name        *string    `json:"name"`
	Description *string    `json:"description"`
	Enabled     *bool      `json:"enabled"`
	Steps       *[]Step    `json:"steps"`
	Triggers    *[]Trigger `json:"triggers"`
}

func (r *Repository) Save(ctx context.Context, workspaceID, id, name, description string,
	steps []Step, triggers []Trigger, enabled bool, actorID string) (*Workflow, error) {

	// Validation lives in saveTx, so the two entry points cannot disagree
	// about what a valid workflow is.
	var wf *Workflow
	err := database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		wf, err = r.saveTx(ctx, tx, workspaceID, id, name, description, steps, triggers, enabled, actorID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return wf, nil
}

// saveTx is Save's body, so Patch can run it inside a transaction it already
// holds — which is what makes read-merge-write one atomic step instead of a
// lost update.
func (r *Repository) saveTx(ctx context.Context, tx pgx.Tx, workspaceID, id, name, description string,
	steps []Step, triggers []Trigger, enabled bool, actorID string) (*Workflow, error) {

	// THE NAME IS CHECKED HERE BECAUSE THE DATABASE'S CHECK IS NOT AN API.
	//
	// `workflows_name_bounded CHECK (char_length(name) BETWEEN 1 AND 200)` is
	// caller-reachable — an empty name, or a pasted paragraph — and nothing in
	// Go looked at it, so the refusal was whatever Postgres says about a
	// violated constraint, verbatim on the wire. char_length counts CHARACTERS
	// and len() counts BYTES, so this uses runes: a 200-character Korean name is
	// 600 bytes and was refused by a byte check that agreed with nothing.
	//
	// IT DOES NOT TRIM. Trimming here also trimmed the name Patch MERGED IN for
	// a caller who sent no name at all — so a PATCH of `enabled` quietly renamed
	// "  Nightly digest  " to "Nightly digest", against PatchFields' own promise
	// that a nil field must not change. Worse, a row written before this check
	// existed can hold "   ", which char_length accepts as 3: trimming made it 0
	// and refused every future PATCH of that workflow, naming a field the caller
	// had not sent. Normalising an INPUT belongs where the input is, so the
	// handler trims what the caller actually supplied.
	if n := utf8.RuneCountInString(name); n < 1 || n > 200 {
		return nil, invalidf("a workflow name is 1 to 200 characters (got %d)", n)
	}
	// Postgres text cannot hold U+0000 at all: `\u0000` is legal JSON, Go's
	// decoder turns it into a real NUL, and the INSERT then fails with 22021 (or
	// 22P05 for the jsonb columns). Caught here so the answer names the cause.
	if strings.ContainsRune(name, 0) || strings.ContainsRune(description, 0) {
		return nil, invalidf("a workflow name or description cannot contain a NUL character")
	}
	for i, st := range steps {
		if hasNUL(st.Config) {
			return nil, invalidf("the config of step %d cannot contain a NUL character", i)
		}
	}
	for i, t := range triggers {
		if hasNUL(t.Filter) {
			return nil, invalidf("the filter of trigger %d cannot contain a NUL character", i)
		}
	}

	if len(steps) > 20 {
		return nil, ErrTooMany
	}
	for i, st := range steps {
		if !validStepKind(st.Kind) {
			return nil, invalidf("step %d has an unknown kind %q", i, st.Kind)
		}
	}
	for i, t := range triggers {
		if !eventShape.MatchString(t.EventType) {
			return nil, invalidf("trigger %d has an invalid event type %q", i, t.EventType)
		}
	}
	stepsJSON, err := json.Marshal(nonNilSteps(steps))
	if err != nil {
		return nil, fmt.Errorf("encode steps: %w", err)
	}
	// NO GO-SIDE SIZE CHECK. `workflow_versions_steps_size CHECK
	// (pg_column_size(steps) <= 65536)` is measured on the JSONB datum, and a
	// byte count of the JSON TEXT cannot predict it: jsonb carries a 4-byte
	// JEntry per array element and per object key/value pair, so a config of
	// many small keys inflates. For
	// `[{"kind":"post_message","config":{"k0":0,…,"k4999":4999}}]`, which is what
	// the test sends: 62,816 bytes of JSON text, 103,952 of jsonb. A guard
	// written here passed exactly the payloads the constraint rejects, which is
	// worse than no guard: it reads as protection.
	//
	// It is NOT the size of the stored column. A CHECK runs before the row is
	// toasted, so the constraint sees the uncompressed datum while
	// `SELECT pg_column_size(steps)` afterwards reports the compressed one. For
	// `[{"kind":"post_message","config":{"k0":0,…,"k2999":2999}}]` on this
	// database: 61,952 to the CHECK, 21,207 once stored. Anyone measuring
	// headroom with that query reads about three times more than there is —
	// which is the wrong direction to be wrong in.
	//
	// The constraint is the authority on its own limit, so its violation is
	// translated at the boundary instead — see saveFailed.

	var wf Workflow
	if err := func(tx pgx.Tx) error {
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
	}(tx); err != nil {
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
// hasNUL reports whether a decoded JSON value contains U+0000 anywhere inside
// it — in a string, in a map key, or nested through arrays and objects.
//
// TWO EARLIER VERSIONS OF THIS WERE WRONG IN OPPOSITE DIRECTIONS, both because
// they inspected the ENCODED form instead of the value:
//
//   - searching the marshalled bytes for a raw NUL matched nothing, because
//     encoding/json writes U+0000 as the escape. Every one of these reached the
//     database and became a 500.
//   - searching them for the escape matched too much, because json ALSO escapes
//     a backslash: a caller sending the six ordinary characters `\u0000` — a
//     Windows path, a regex, any JSON round-tripped as a string — produced
//     `"\\u0000"`, which contains the escape at offset 1. It was refused, told
//     it contained a character that was not there, and the same six characters
//     were accepted in the `name` field of the same request, which checks the
//     value instead.
//
// So this walks the value. `Step.Config` and `Trigger.Filter` are
// map[string]any decoded by encoding/json, so the only shapes here are string,
// float64, bool, nil, map[string]any and []any. Of those, float64, bool and nil
// cannot hold one; map and slice can, which is why the switch recurses into
// them.
//
// THE INPUT MUST BE encoding/json OUTPUT, and the signature says so: hasNUL
// takes map[string]any, which is what both call sites already hold, so handing
// it a struct or a map[string]string is a compile error rather than a silent
// false. That matters because Save is exported — a typed config for a new step
// kind, or a duplicate-a-workflow path, is the kind of caller that arrives
// without anyone rereading this, and a false here is a NUL reaching the
// database as a 500.
//
// What the type cannot prevent is a wrong-shaped value INSIDE the map. The
// recursion returns false for those, deliberately: a default of true would
// reject every number and boolean, i.e. every real config.
func hasNUL(m map[string]any) bool { return hasNULValue(m) }

func hasNULValue(v any) bool {
	switch t := v.(type) {
	case string:
		return strings.ContainsRune(t, 0)
	case map[string]any:
		for k, val := range t {
			if strings.ContainsRune(k, 0) || hasNULValue(val) {
				return true
			}
		}
	case []any:
		for _, val := range t {
			if hasNULValue(val) {
				return true
			}
		}
	}
	return false
}

var eventShape = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)

// ErrInvalid marks a failure the CALLER caused and can fix by sending something
// else — an unknown step kind, a malformed trigger.
//
// It exists because the route's error mapping had no way to tell those from an
// infrastructure failure, so everything that came out of a save became
// `400 INVALID_WORKFLOW` with the raw error text. Once the read moved inside
// the transaction that included operational failures: two admins editing the
// same workflow gave the second `400 INVALID_WORKFLOW: "lock workflow: ERROR:
// canceling statement due to lock timeout (SQLSTATE 55P03)"` — a client will
// not retry a 400, the user is told their workflow is invalid when it is not,
// and a Postgres error string is on the wire.
var ErrInvalid = errors.New("workflow: invalid")

// invalidError carries the caller-facing sentence and NOTHING ELSE.
//
// invalidf used to be `fmt.Errorf("%w: %s", ErrInvalid, …)`, and saveFailed
// forwards the message verbatim — so the sentinel's own text landed on the wire
// and every validation message got worse, in the change whose point was to keep
// internals off it: `workflow: step 0 has unknown kind "x"` became
// `workflow: invalid: workflow: step 0 has unknown kind "x"`. Wrapping through
// a type keeps errors.Is working without the prefix.
type invalidError struct{ msg string }

func (e invalidError) Error() string { return e.msg }
func (e invalidError) Unwrap() error { return ErrInvalid }

func invalidf(format string, args ...any) error {
	return invalidError{msg: fmt.Sprintf(format, args...)}
}
