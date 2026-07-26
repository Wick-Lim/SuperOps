//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/Wick-Lim/SuperOps/backend/internal/workflow"
	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
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

	// The run is constructed directly rather than claimed from the queue.
	//
	// The property under test is the EFFECTS TABLE — that a second execution of
	// the same run does not repeat its side effect — and claiming from a queue
	// this whole suite shares made that assertion depend on which test's run
	// came up first. It also must never t.Skip: a skip here is a vacuous pass
	// on the one property the package exists for.
	//
	// ClaimRun is covered by TestClaimRunTakesAPendingRunWithItsSteps and by
	// the stale-run test, which are about the queue rather than about this.
	run := &workflow.Run{
		ID: runID, WorkflowID: wf.ID, WorkspaceID: ws, OwnerID: me,
		Payload: []byte(`{"channel_id":"x"}`),
	}
	steps := match.Steps

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
	// OwnerID is set: an ownerless run now fails before reaching any step, so
	// without it this would assert the wrong refusal.
	run := &workflow.Run{ID: runID, WorkspaceID: ws, OwnerID: me, Payload: []byte(`{}`)}
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

// A step whose action this deployment does not have FAILS the run.
//
// It used to be recorded 'skipped' with the run still finishing 'succeeded',
// which is the worst available answer: an executor built with no adapters
// reported every run green having done nothing at all, and the run list — the
// only place anybody looks — was a wall of lies. This is the corrected
// behaviour, and TestAMissingAdapterFailsTheRun asserts it end to end.
func TestAnUnavailableActionFailsTheRun(t *testing.T) {
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
	if err := exec.Run(ctx, &workflow.Run{ID: runID, WorkspaceID: ws, OwnerID: me}, match.Steps); err != nil {
		t.Fatal(err)
	}

	var runStatus, stepStatus string
	if err := h.app.DB.QueryRow(ctx,
		`SELECT r.status, s.status FROM workflow_runs r
		   JOIN workflow_step_runs s ON s.run_id = r.id
		  WHERE r.id = $1`, runID).Scan(&runStatus, &stepStatus); err != nil {
		t.Fatal(err)
	}
	if runStatus != "failed" {
		t.Errorf("run status = %q; a run that performed no step has not succeeded", runStatus)
	}
	if stepStatus != "failed" {
		t.Errorf("step status = %q, want failed", stepStatus)
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

// A WORKFLOW RUNS AS ITS OWNER, and the owner's capability is what every action
// checks. Without this a `post_message` step reaches every channel in the
// tenant — including private ones its author cannot read — and "save a
// workflow" becomes a privilege escalation available to any member.
func TestAnActionIsRefusedWhenTheOwnerCouldNotDoItByHand(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	repo := wfRepo(t)
	ctx := context.Background()

	// A private channel the workflow's owner is NOT in.
	owner := h.newGuest(t, admin, ws, "wf-owner")
	private := h.createTypedChannel(t, admin, ws, uniqueSlug("wf-private"), "private")

	wf, err := repo.Save(ctx, ws, "", fmt.Sprintf("escalate-%d", time.Now().UnixNano()), "",
		[]workflow.Step{{Kind: workflow.StepPostMessage, Config: map[string]any{
			"channel_id": private, "body": "leaked into a private channel",
		}}},
		[]workflow.Trigger{{EventType: "wf.test.escalate"}}, true, owner.id)
	if err != nil {
		t.Fatal(err)
	}
	if wf.OwnerID != owner.id {
		t.Fatalf("owner_id = %q, want the saver %q", wf.OwnerID, owner.id)
	}

	matches, _ := repo.MatchingWorkflows(ctx, ws, "wf.test.escalate")
	var match workflow.Matching
	for _, m := range matches {
		if m.WorkflowID == wf.ID {
			match = m
		}
	}
	runID, err := repo.EnqueueRun(ctx, match, "wf.test.escalate",
		fmt.Sprintf("evt-esc-%d", time.Now().UnixNano()), nil)
	if err != nil {
		t.Fatal(err)
	}

	poster := &countingPoster{}
	exec := workflow.NewExecutor(repo, nil,
		workflow.NewMessageAction(h.app.Authz, &recordingCreator{poster: poster}))
	run := &workflow.Run{ID: runID, WorkspaceID: ws, OwnerID: owner.id, Payload: []byte(`{}`)}
	if err := exec.Run(ctx, run, match.Steps); err != nil {
		t.Fatal(err)
	}

	if poster.count() != 0 {
		t.Fatal("the workflow posted into a private channel its owner cannot read; saving a " +
			"workflow is a privilege escalation")
	}
	var status, errText string
	if err := h.app.DB.QueryRow(ctx,
		`SELECT status, error FROM workflow_runs WHERE id = $1`, runID).Scan(&status, &errText); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("run status = %q; an unauthorized step must fail the run", status)
	}

	// And once the owner CAN read the channel, the same workflow works — which
	// proves the refusal was the capability and not a broken action.
	h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/workspaces/"+ws+"/channels/"+private+"/members", admin,
		map[string]string{"user_id": owner.id})

	runID2, err := repo.EnqueueRun(ctx, match, "wf.test.escalate",
		fmt.Sprintf("evt-esc2-%d", time.Now().UnixNano()), nil)
	if err != nil {
		t.Fatal(err)
	}
	run2 := &workflow.Run{ID: runID2, WorkspaceID: ws, OwnerID: owner.id, Payload: []byte(`{}`)}
	if err := exec.Run(ctx, run2, match.Steps); err != nil {
		t.Fatal(err)
	}
	if poster.count() != 1 {
		t.Fatalf("after admitting the owner to the channel the workflow posted %d times, want 1",
			poster.count())
	}
	_ = me
}

// A run with no owner has no authority to check, so it must not execute at all.
func TestAnOwnerlessRunFails(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	channel := h.createChannel(t, admin, ws, uniqueSlug("wf-noowner"))
	repo := wfRepo(t)
	ctx := context.Background()

	wf := saveWorkflow(t, ws, me,
		[]workflow.Step{{Kind: workflow.StepPostMessage, Config: map[string]any{
			"channel_id": channel, "body": "x",
		}}},
		[]workflow.Trigger{{EventType: "wf.test.noowner"}})
	matches, _ := repo.MatchingWorkflows(ctx, ws, "wf.test.noowner")
	var match workflow.Matching
	for _, m := range matches {
		if m.WorkflowID == wf.ID {
			match = m
		}
	}
	runID, err := repo.EnqueueRun(ctx, match, "wf.test.noowner",
		fmt.Sprintf("evt-no-%d", time.Now().UnixNano()), nil)
	if err != nil {
		t.Fatal(err)
	}

	poster := &countingPoster{}
	exec := workflow.NewExecutor(repo, nil,
		workflow.NewMessageAction(h.app.Authz, &recordingCreator{poster: poster}))
	// OwnerID deliberately empty.
	if err := exec.Run(ctx, &workflow.Run{ID: runID, WorkspaceID: ws}, match.Steps); err != nil {
		t.Fatal(err)
	}
	if poster.count() != 0 {
		t.Fatal("an ownerless run performed a step; nothing authorized it")
	}
	var status string
	if err := h.app.DB.QueryRow(ctx,
		`SELECT status FROM workflow_runs WHERE id = $1`, runID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("ownerless run status = %q, want failed", status)
	}
}

// A step this deployment has no adapter for FAILS the run.
//
// It used to be recorded 'skipped' with the run still 'succeeded', which meant
// an executor built with no adapters reported every run green having done
// nothing — and the run list, the only place anybody looks, was a wall of lies.
func TestAMissingAdapterFailsTheRun(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	repo := wfRepo(t)
	ctx := context.Background()

	wf := saveWorkflow(t, ws, me,
		[]workflow.Step{{Kind: workflow.StepNotify, Config: map[string]any{"user_id": me, "title": "x"}}},
		[]workflow.Trigger{{EventType: "wf.test.noadapter"}})
	matches, _ := repo.MatchingWorkflows(ctx, ws, "wf.test.noadapter")
	var match workflow.Matching
	for _, m := range matches {
		if m.WorkflowID == wf.ID {
			match = m
		}
	}
	runID, err := repo.EnqueueRun(ctx, match, "wf.test.noadapter",
		fmt.Sprintf("evt-na-%d", time.Now().UnixNano()), nil)
	if err != nil {
		t.Fatal(err)
	}

	exec := workflow.NewExecutor(repo, nil) // no actions at all
	if err := exec.Run(ctx, &workflow.Run{ID: runID, WorkspaceID: ws, OwnerID: me}, match.Steps); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := h.app.DB.QueryRow(ctx,
		`SELECT status FROM workflow_runs WHERE id = $1`, runID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("run status = %q with no adapter registered; reporting success for a run that "+
			"did nothing makes the history worthless", status)
	}
}

// The authoring surface exists and is admin-gated: saving a workflow decides
// that an action will be taken under somebody's authority, repeatedly, without
// them present.
func TestTheWorkflowAPIIsReachableAndAdminGated(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	channel := h.createChannel(t, admin, ws, uniqueSlug("wf-api"))

	body := map[string]any{
		"name":    fmt.Sprintf("api-%d", time.Now().UnixNano()),
		"enabled": true,
		"steps": []map[string]any{{
			"kind": "post_message", "config": map[string]any{"channel_id": channel, "body": "hi"},
		}},
		"triggers": []map[string]any{{"event_type": "message.created"}},
	}

	resp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/workspaces/"+ws+"/workflows", admin, body)
	var created struct {
		ID      string `json:"id"`
		OwnerID string `json:"owner_id"`
		Version int    `json:"version"`
	}
	decodeInto(t, resp.Data, &created)
	if created.ID == "" || created.Version != 1 {
		t.Fatalf("create returned %+v", created)
	}
	if created.OwnerID == "" {
		t.Fatal("the created workflow has no owner, so no step could ever be authorized")
	}

	// Reachable by id, listed, and its (empty) run history is readable.
	h.req(t, http.StatusOK, http.MethodGet, "/api/v1/workflows/"+created.ID, admin, nil)
	h.req(t, http.StatusOK, http.MethodGet, "/api/v1/workspaces/"+ws+"/workflows", admin, nil)
	h.req(t, http.StatusOK, http.MethodGet, "/api/v1/workflows/"+created.ID+"/runs", admin, nil)
	h.req(t, http.StatusOK, http.MethodGet, "/api/v1/workflows/"+created.ID+"/rejections", admin, nil)

	// The catalogue tells a client what THIS deployment can do.
	var cat struct {
		Steps []struct {
			Kind string `json:"kind"`
		} `json:"steps"`
		Triggers []string `json:"triggers"`
	}
	decodeInto(t, h.req(t, http.StatusOK, http.MethodGet, "/api/v1/workflow-steps", admin, nil).Data, &cat)
	if len(cat.Steps) == 0 || len(cat.Triggers) == 0 {
		t.Fatal("the catalogue is empty, so a client has nothing to offer")
	}

	// A non-admin may READ but not create.
	member := h.newUser(t, admin, ws, "wf-member")
	h.req(t, http.StatusOK, http.MethodGet, "/api/v1/workspaces/"+ws+"/workflows", member.token, nil)
	code, _ := h.do(t, http.MethodPost, "/api/v1/workspaces/"+ws+"/workflows", member.token, body)
	if code != http.StatusForbidden {
		t.Fatalf("a non-admin creating a workflow = %d, want 403", code)
	}

	// And another tenant reaches nothing.
	stranger := h.newTenant(t, "wf-stranger")
	code, _ = h.do(t, http.MethodGet, "/api/v1/workflows/"+created.ID, stranger.token, nil)
	if code != http.StatusNotFound {
		t.Fatalf("a stranger reading another tenant's workflow = %d, want 404", code)
	}

	// Archiving disables it, so a hidden workflow cannot keep firing.
	h.req(t, http.StatusNoContent, http.MethodDelete, "/api/v1/workflows/"+created.ID, admin, nil)
	var enabled bool
	var archived *time.Time
	if err := h.app.DB.QueryRow(context.Background(),
		`SELECT enabled, archived_at FROM workflows WHERE id = $1`, created.ID).Scan(&enabled, &archived); err != nil {
		t.Fatal(err)
	}
	if enabled || archived == nil {
		t.Fatalf("archived workflow: enabled=%v archived_at=%v — a hidden workflow that still "+
			"fires is the worst combination", enabled, archived)
	}
}

// recordingCreator is a MessageCreator that records instead of writing, so the
// authorization tests do not depend on the chat schema.
type recordingCreator struct{ poster *countingPoster }

func (c *recordingCreator) CreateAs(ctx context.Context, workspaceID, channelID, userID, body string) (string, error) {
	return c.poster.PostAs(ctx, workspaceID, channelID, body)
}

// THE TRIGGER CONSUMER, end to end over its real Handle path.
//
// This is the seam that was missing entirely — nothing translated a domain
// event into a run — and then, once written, silently refused every message
// because the worker's jetstream.Msg → *nats.Msg conversion dropped the
// metadata it derives its idempotency key from. Both failures were invisible to
// the type system and to every unit test; the first worker boot found the
// second. This asserts the whole path.
func TestTheTriggerConsumerEnqueuesARunAndDeduplicates(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	channel := h.createChannel(t, admin, ws, uniqueSlug("wf-trigger"))
	repo := wfRepo(t)
	ctx := context.Background()

	wf := saveWorkflow(t, ws, me,
		[]workflow.Step{{Kind: workflow.StepPostMessage, Config: map[string]any{
			"channel_id": channel, "body": "from {{event.user_id}}",
		}}},
		[]workflow.Trigger{{EventType: "message.created", Filter: map[string]any{"channel_id": channel}}})

	consumer := workflow.NewTriggerConsumer(repo, nil)
	seq := time.Now().UnixNano()
	event := func(channelID string) *nats.Msg {
		payload, _ := json.Marshal(map[string]any{
			"type": "message.new",
			"data": map[string]any{"id": "m1", "channel_id": channelID, "user_id": me},
		})
		return &nats.Msg{
			Subject: "superops." + ws + ".message.created",
			Data:    payload,
			// The header the worker sets from the JetStream metadata. Without
			// it the consumer has no stable identity and refuses — which is
			// exactly what it did in production before this was carried across.
			Header: nats.Header{natspkg.HeaderStreamSequence: []string{fmt.Sprint(seq)}},
		}
	}

	if err := consumer.Handle(ctx, event(channel)); err != nil {
		t.Fatal(err)
	}

	var runs int
	if err := h.app.DB.QueryRow(ctx,
		`SELECT count(*) FROM workflow_runs WHERE workflow_id = $1`, wf.ID).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("the trigger consumer produced %d runs from one event, want 1", runs)
	}

	// REDELIVERY. Same stream sequence, so the same trigger key: no second run.
	for i := 0; i < 3; i++ {
		if err := consumer.Handle(ctx, event(channel)); err != nil {
			t.Fatalf("redelivery %d: %v", i, err)
		}
	}
	if err := h.app.DB.QueryRow(ctx,
		`SELECT count(*) FROM workflow_runs WHERE workflow_id = $1`, wf.ID).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("%d runs after four deliveries of one message; the stream sequence is not "+
			"reaching the consumer, so every retry starts another run", runs)
	}

	// THE FILTER NARROWS. An event in a different channel matches the trigger's
	// event type and not its filter, so it queues nothing.
	other := h.createChannel(t, admin, ws, uniqueSlug("wf-trigger-other"))
	seq++
	if err := consumer.Handle(ctx, event(other)); err != nil {
		t.Fatal(err)
	}
	if err := h.app.DB.QueryRow(ctx,
		`SELECT count(*) FROM workflow_runs WHERE workflow_id = $1`, wf.ID).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("%d runs after an event the filter should have excluded", runs)
	}

	// And the queued run carries the event payload, so the template resolves.
	var payload []byte
	if err := h.app.DB.QueryRow(ctx,
		`SELECT payload FROM workflow_runs WHERE workflow_id = $1 LIMIT 1`, wf.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), me) {
		t.Fatalf("the run payload does not carry the event: %s", payload)
	}
}

// A message with no stable identity is REFUSED rather than enqueued. Enqueuing
// it would start a fresh run on every redelivery, which is the failure the
// trigger key exists to prevent.
func TestATriggerWithNoStableIdentityIsRefused(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	channel := h.createChannel(t, admin, ws, uniqueSlug("wf-noid"))
	repo := wfRepo(t)
	ctx := context.Background()

	wf := saveWorkflow(t, ws, me,
		[]workflow.Step{{Kind: workflow.StepPostMessage, Config: map[string]any{
			"channel_id": channel, "body": "x",
		}}},
		[]workflow.Trigger{{EventType: "message.created"}})

	payload, _ := json.Marshal(map[string]any{"type": "message.new", "data": map[string]any{"id": "m"}})
	err := workflow.NewTriggerConsumer(repo, nil).Handle(ctx, &nats.Msg{
		Subject: "superops." + ws + ".message.created",
		Data:    payload,
		// No identity header at all.
	})
	if err != nil {
		t.Fatalf("a message with no identity must be acked, not retried forever: %v", err)
	}
	var runs int
	if err := h.app.DB.QueryRow(ctx,
		`SELECT count(*) FROM workflow_runs WHERE workflow_id = $1`, wf.ID).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Fatal("a message with no stable identity was enqueued; every redelivery would start " +
			"another run of the same workflow")
	}
}

// ClaimRun hands back a pending run together with the steps of the version it
// was queued with. Separated from the idempotency test because it is about the
// QUEUE, and mixing the two made the more important assertion depend on which
// test's run came up first.
func TestClaimRunTakesAPendingRunWithItsSteps(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	channel := h.createChannel(t, admin, ws, uniqueSlug("wf-claim"))
	repo := wfRepo(t)
	ctx := context.Background()

	wf := saveWorkflow(t, ws, me,
		[]workflow.Step{{Kind: workflow.StepPostMessage, Config: map[string]any{
			"channel_id": channel, "body": "claimed",
		}}},
		[]workflow.Trigger{{EventType: "wf.test.claim"}})
	matches, _ := repo.MatchingWorkflows(ctx, ws, "wf.test.claim")
	var match workflow.Matching
	for _, m := range matches {
		if m.WorkflowID == wf.ID {
			match = m
		}
	}
	if _, err := repo.EnqueueRun(ctx, match, "wf.test.claim",
		fmt.Sprintf("evt-claim-%d", time.Now().UnixNano()), nil); err != nil {
		t.Fatal(err)
	}

	// SOME run comes back — the queue is shared with the rest of the suite, so
	// asserting it is ours would be asserting the scheduler's order.
	run, steps, err := repo.ClaimRun(ctx)
	if err != nil {
		t.Fatalf("nothing could be claimed from a non-empty queue: %v", err)
	}
	if run.ID == "" || run.WorkspaceID == "" {
		t.Fatalf("claimed run is incomplete: %+v", run)
	}
	if run.OwnerID == "" {
		t.Fatal("the claimed run carries no owner, so no step could be authorized")
	}
	if steps == nil {
		t.Fatal("the claim returned no steps; the run would execute nothing")
	}
	// It is marked running, so a second worker does not take it.
	var status string
	if err := h.app.DB.QueryRow(ctx,
		`SELECT status FROM workflow_runs WHERE id = $1`, run.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "running" {
		t.Fatalf("claimed run status = %q, want running — two workers would take it", status)
	}
	_ = repo.FinishRun(ctx, run.ID, "cancelled", "claimed by a test")
}
