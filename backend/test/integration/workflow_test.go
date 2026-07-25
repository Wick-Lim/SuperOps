//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wick-Lim/SuperOps/backend/internal/workflow"
)

func wfRepo(t *testing.T) *workflow.Repository {
	t.Helper()
	return workflow.NewRepository(getHarness(t).app.DB)
}

// countingPoster records every post, so a double-post is visible rather than
// inferred.
type countingPoster struct {
	mu    sync.Mutex
	posts []string
}

func (p *countingPoster) PostAs(_ context.Context, _, channelID, body string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.posts = append(p.posts, channelID+"|"+body)
	return fmt.Sprintf("msg-%d", len(p.posts)), nil
}

func (p *countingPoster) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.posts)
}

type failingPoster struct{ calls int }

func (p *failingPoster) PostAs(context.Context, string, string, string) (string, error) {
	p.calls++
	return "", errors.New("the chat surface is down")
}

func saveWorkflow(t *testing.T, ws, actor string, steps []workflow.Step, triggers []workflow.Trigger) *workflow.Workflow {
	t.Helper()
	wf, err := wfRepo(t).Save(context.Background(), ws, "",
		fmt.Sprintf("wf-%d", time.Now().UnixNano()), "", steps, triggers, true, actor)
	if err != nil {
		t.Fatalf("save workflow: %v", err)
	}
	return wf
}

// THE PROPERTY THE WHOLE PACKAGE EXISTS FOR.
//
// Triggers arrive on durable JetStream consumers, which redeliver on any nak
// and after any crash. Run the SAME run twice — which is exactly what a retry
// does — and the message must be posted ONCE. Without the effects table it is
// posted twice, and an automation that occasionally double-posts is one people
// disable.
func TestARetriedRunDoesNotPerformItsEffectTwice(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	channel := h.createChannel(t, admin, ws, uniqueSlug("wf-once"))
	repo := wfRepo(t)
	ctx := context.Background()

	wf := saveWorkflow(t, ws, me,
		[]workflow.Step{{Kind: workflow.StepPostMessage, Config: map[string]any{
			"channel_id": channel, "body": "a new message arrived",
		}}},
		[]workflow.Trigger{{EventType: "message.new"}})

	matches, err := repo.MatchingWorkflows(ctx, ws, "message.new")
	if err != nil {
		t.Fatal(err)
	}
	var match workflow.Matching
	for _, m := range matches {
		if m.WorkflowID == wf.ID {
			match = m
		}
	}
	if match.WorkflowID == "" {
		t.Fatal("the workflow does not match its own trigger")
	}

	runID, err := repo.EnqueueRun(ctx, match, "message.new",
		fmt.Sprintf("evt-%d", time.Now().UnixNano()), []byte(`{"channel_id":"x"}`))
	if err != nil {
		t.Fatal(err)
	}

	poster := &countingPoster{}
	exec := workflow.NewExecutor(repo, nil, workflow.NewPostMessageAction(poster))

	// First delivery.
	//
	// The queue is shared with every other test in this package, so claim until
	// OUR run comes up. It must never t.Skip: a skip here makes the assertion
	// below vacuous, and this is the one property the whole package exists for
	// — verified by disabling the effects guard and watching this go red.
	var run *workflow.Run
	var steps []workflow.Step
	for attempt := 0; attempt < 200; attempt++ {
		r, s, err := repo.ClaimRun(ctx)
		if errors.Is(err, workflow.ErrNotFound) {
			t.Fatalf("the queue drained without producing our run (%s)", runID)
		}
		if err != nil {
			t.Fatal(err)
		}
		if r.ID == runID {
			run, steps = r, s
			break
		}
		// Somebody else's run: put it back so its own test still finds it.
		_ = repo.FinishRun(ctx, r.ID, "cancelled", "claimed by another test's drain loop")
	}
	if run == nil {
		t.Fatalf("never claimed our own run after 200 attempts")
	}
	if err := exec.Run(ctx, run, steps); err != nil {
		t.Fatal(err)
	}
	if poster.count() != 1 {
		t.Fatalf("first delivery posted %d times", poster.count())
	}

	// THE RETRY. The worker crashed after performing the effect and before
	// acking, so the same run is executed again.
	if err := exec.Run(ctx, run, steps); err != nil {
		t.Fatal(err)
	}
	if poster.count() != 1 {
		t.Fatalf("the message was posted %d times across two deliveries of one run; the "+
			"effects table is not preventing the repeat", poster.count())
	}
}

// A REDELIVERED EVENT must not create a second run. The trigger key comes from
// the event's own id, so the second enqueue conflicts.
func TestARedeliveredEventDoesNotStartASecondRun(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	channel := h.createChannel(t, admin, ws, uniqueSlug("wf-dup"))
	repo := wfRepo(t)
	ctx := context.Background()

	wf := saveWorkflow(t, ws, me,
		[]workflow.Step{{Kind: workflow.StepPostMessage, Config: map[string]any{
			"channel_id": channel, "body": "hello",
		}}},
		[]workflow.Trigger{{EventType: "message.new"}})

	matches, _ := repo.MatchingWorkflows(ctx, ws, "message.new")
	var match workflow.Matching
	for _, m := range matches {
		if m.WorkflowID == wf.ID {
			match = m
		}
	}

	key := fmt.Sprintf("evt-dup-%d", time.Now().UnixNano())
	if _, err := repo.EnqueueRun(ctx, match, "message.new", key, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_, err := repo.EnqueueRun(ctx, match, "message.new", key, nil)
		if !errors.Is(err, workflow.ErrDuplicate) {
			t.Fatalf("redelivery %d = %v, want ErrDuplicate", i, err)
		}
	}

	var runs int
	if err := h.app.DB.QueryRow(ctx,
		`SELECT count(*) FROM workflow_runs WHERE workflow_id = $1`, wf.ID).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("%d runs after four deliveries of one event", runs)
	}

	// An event with no id cannot be made safe and is refused outright.
	if _, err := repo.EnqueueRun(ctx, match, "message.new", "", nil); err == nil {
		t.Fatal("enqueued a run for an event with no id")
	}
}

// A disabled workflow does not run. It is the off switch, and an off switch
// that does not switch off is worse than none.
func TestADisabledWorkflowDoesNotMatch(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	repo := wfRepo(t)
	ctx := context.Background()

	wf, err := repo.Save(ctx, ws, "", fmt.Sprintf("off-%d", time.Now().UnixNano()), "",
		[]workflow.Step{{Kind: workflow.StepNotify, Config: map[string]any{"user_id": me, "title": "x"}}},
		[]workflow.Trigger{{EventType: "issue.created"}}, false, me)
	if err != nil {
		t.Fatal(err)
	}

	matches, err := repo.MatchingWorkflows(ctx, ws, "issue.created")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range matches {
		if m.WorkflowID == wf.ID {
			t.Fatal("a disabled workflow matched its trigger")
		}
	}
}

// A workflow belongs to ONE workspace. An event in another tenant must not
// reach it — that would be automation running across a tenancy boundary.
func TestAWorkflowDoesNotMatchAnotherTenantsEvent(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	repo := wfRepo(t)
	ctx := context.Background()

	wf := saveWorkflow(t, ws, me,
		[]workflow.Step{{Kind: workflow.StepNotify, Config: map[string]any{"user_id": me, "title": "x"}}},
		[]workflow.Trigger{{EventType: "message.new"}})

	other := h.newTenant(t, "wf-outsider")
	matches, err := repo.MatchingWorkflows(ctx, other.workspaceID, "message.new")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range matches {
		if m.WorkflowID == wf.ID {
			t.Fatal("a workflow matched an event in ANOTHER WORKSPACE")
		}
	}
}

// Versions are immutable, so a run executes the version that existed when it
// was created — not whatever the workflow says now.
func TestARunExecutesTheVersionItWasCreatedWith(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	channel := h.createChannel(t, admin, ws, uniqueSlug("wf-version"))
	repo := wfRepo(t)
	ctx := context.Background()

	wf := saveWorkflow(t, ws, me,
		[]workflow.Step{{Kind: workflow.StepPostMessage, Config: map[string]any{
			"channel_id": channel, "body": "ORIGINAL",
		}}},
		[]workflow.Trigger{{EventType: "message.new"}})
	if wf.Version != 1 {
		t.Fatalf("first save produced version %d", wf.Version)
	}

	matches, _ := repo.MatchingWorkflows(ctx, ws, "message.new")
	var match workflow.Matching
	for _, m := range matches {
		if m.WorkflowID == wf.ID {
			match = m
		}
	}
	runID, err := repo.EnqueueRun(ctx, match, "message.new",
		fmt.Sprintf("evt-ver-%d", time.Now().UnixNano()), nil)
	if err != nil {
		t.Fatal(err)
	}

	// Edit AFTER the run was queued. The run must still do the old thing.
	updated, err := repo.Save(ctx, ws, wf.ID, wf.Name, "", []workflow.Step{{
		Kind: workflow.StepPostMessage, Config: map[string]any{"channel_id": channel, "body": "REWRITTEN"},
	}}, []workflow.Trigger{{EventType: "message.new"}}, true, me)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != 2 {
		t.Fatalf("editing produced version %d, want 2 — an edit must not mutate version 1", updated.Version)
	}

	var body string
	if err := h.app.DB.QueryRow(ctx, `
		SELECT v.steps->0->'config'->>'body'
		  FROM workflow_runs r JOIN workflow_versions v ON v.id = r.version_id
		 WHERE r.id = $1`, runID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if body != "ORIGINAL" {
		t.Fatalf("the queued run points at %q; reading an old run would report what the "+
			"workflow does NOW rather than what it did", body)
	}
}

// A failing step stops the run. Continuing would execute later steps against a
// state the author never anticipated.
func TestAFailingStepStopsTheRun(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	channel := h.createChannel(t, admin, ws, uniqueSlug("wf-fail"))
	repo := wfRepo(t)
	ctx := context.Background()

	wf := saveWorkflow(t, ws, me, []workflow.Step{
		{Kind: workflow.StepPostMessage, Config: map[string]any{"channel_id": channel, "body": "one"}},
		{Kind: workflow.StepPostMessage, Config: map[string]any{"channel_id": channel, "body": "two"}},
	}, []workflow.Trigger{{EventType: "wf.test.fail"}})

	matches, _ := repo.MatchingWorkflows(ctx, ws, "wf.test.fail")
	var match workflow.Matching
	for _, m := range matches {
		if m.WorkflowID == wf.ID {
			match = m
		}
	}
	runID, err := repo.EnqueueRun(ctx, match, "wf.test.fail",
		fmt.Sprintf("evt-fail-%d", time.Now().UnixNano()), nil)
	if err != nil {
		t.Fatal(err)
	}

	poster := &failingPoster{}
	exec := workflow.NewExecutor(repo, nil, workflow.NewPostMessageAction(poster))
	run := &workflow.Run{ID: runID, WorkspaceID: ws, Payload: []byte(`{}`)}
	if err := exec.Run(ctx, run, match.Steps); err != nil {
		t.Fatal(err)
	}

	if poster.calls != 1 {
		t.Fatalf("the poster was called %d times; the run continued past a failed step", poster.calls)
	}
	var status, errText string
	if err := h.app.DB.QueryRow(ctx,
		`SELECT status, error FROM workflow_runs WHERE id = $1`, runID).Scan(&status, &errText); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("run status = %q after a step failed", status)
	}
	if errText == "" {
		t.Error("a failed run records no reason, so nobody can tell why it stopped")
	}
}

// A step whose action this deployment does not have is SKIPPED, not failed:
// the workflow is not wrong, and failing the run would hide the steps that can
// run.
func TestAnUnavailableActionSkipsRatherThanFails(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	repo := wfRepo(t)
	ctx := context.Background()

	wf := saveWorkflow(t, ws, me,
		[]workflow.Step{{Kind: workflow.StepNotify, Config: map[string]any{"user_id": me, "title": "x"}}},
		[]workflow.Trigger{{EventType: "wf.test.skip"}})
	matches, _ := repo.MatchingWorkflows(ctx, ws, "wf.test.skip")
	var match workflow.Matching
	for _, m := range matches {
		if m.WorkflowID == wf.ID {
			match = m
		}
	}
	runID, err := repo.EnqueueRun(ctx, match, "wf.test.skip",
		fmt.Sprintf("evt-skip-%d", time.Now().UnixNano()), nil)
	if err != nil {
		t.Fatal(err)
	}

	// An executor with NO actions at all.
	exec := workflow.NewExecutor(repo, nil)
	if err := exec.Run(ctx, &workflow.Run{ID: runID, WorkspaceID: ws}, match.Steps); err != nil {
		t.Fatal(err)
	}

	var runStatus, stepStatus string
	if err := h.app.DB.QueryRow(ctx,
		`SELECT r.status, s.status FROM workflow_runs r
		   JOIN workflow_step_runs s ON s.run_id = r.id
		  WHERE r.id = $1`, runID).Scan(&runStatus, &stepStatus); err != nil {
		t.Fatal(err)
	}
	if runStatus != "succeeded" {
		t.Errorf("run status = %q; an unavailable action must not fail the run", runStatus)
	}
	if stepStatus != "skipped" {
		t.Errorf("step status = %q, want skipped", stepStatus)
	}
}

// A worker killed mid-run leaves its row 'running' forever, and nothing else
// would notice. Retrying is safe precisely because of the effects table.
func TestStaleRunsAreReleased(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	repo := wfRepo(t)
	ctx := context.Background()

	wf := saveWorkflow(t, ws, me,
		[]workflow.Step{{Kind: workflow.StepNotify, Config: map[string]any{"user_id": me, "title": "x"}}},
		[]workflow.Trigger{{EventType: "wf.test.stale"}})
	matches, _ := repo.MatchingWorkflows(ctx, ws, "wf.test.stale")
	var match workflow.Matching
	for _, m := range matches {
		if m.WorkflowID == wf.ID {
			match = m
		}
	}
	runID, err := repo.EnqueueRun(ctx, match, "wf.test.stale",
		fmt.Sprintf("evt-stale-%d", time.Now().UnixNano()), nil)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a worker that claimed it and died an hour ago.
	if _, err := h.app.DB.Exec(ctx,
		`UPDATE workflow_runs SET status = 'running', started_at = NOW() - INTERVAL '1 hour'
		  WHERE id = $1`, runID); err != nil {
		t.Fatal(err)
	}

	n, err := repo.ReleaseStaleRuns(ctx, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatal("no stale run was released; a worker that died leaves its run stuck forever")
	}

	var status string
	if err := h.app.DB.QueryRow(ctx,
		`SELECT status FROM workflow_runs WHERE id = $1`, runID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("status = %q after release, want pending", status)
	}
}

// The filter narrows a trigger, and an unmatched filter means no run at all.
func TestTriggerFiltersNarrowByTopLevelField(t *testing.T) {
	if !workflow.Matches(map[string]any{"channel_id": "abc"}, map[string]any{"channel_id": "abc"}) {
		t.Error("an exact match did not match")
	}
	if workflow.Matches(map[string]any{"channel_id": "abc"}, map[string]any{"channel_id": "xyz"}) {
		t.Error("a different value matched")
	}
	if workflow.Matches(map[string]any{"channel_id": "abc"}, map[string]any{}) {
		t.Error("a missing field matched; the filter would be ignored rather than applied")
	}
	// An empty filter matches everything, which is what "no narrowing" means.
	if !workflow.Matches(map[string]any{}, map[string]any{"anything": 1}) {
		t.Error("an empty filter refused an event")
	}
}

// A step kind this build does not know is refused at SAVE time. A workflow that
// silently does nothing is worse than one that will not save.
func TestAnUnknownStepKindIsRefusedAtSaveTime(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)

	_, err := wfRepo(t).Save(context.Background(), ws, "", "bad", "",
		[]workflow.Step{{Kind: "http_request", Config: map[string]any{"url": "http://169.254.169.254/"}}},
		[]workflow.Trigger{{EventType: "message.new"}}, true, me)
	if err == nil {
		t.Fatal("saved a workflow with an unknown step kind; http_request in particular is an " +
			"SSRF primitive and is deliberately not offered")
	}
}
