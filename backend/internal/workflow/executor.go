package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// Action performs one step's side effect.
//
// An interface per capability rather than one fat one, so a deployment without
// a chat surface simply has no post_message action and a workflow that uses it
// is REJECTED at save time rather than failing at run time.
type Action interface {
	Kind() string
	// Perform runs the effect.
	//
	// It MUST authorize against run.OwnerID before doing anything. The rule the
	// whole engine rests on is "you cannot automate what you could not do by
	// hand": a workflow is a saved intention, not a privilege escalation, and
	// an action that skipped this would let anybody who can save a workflow
	// post into every private channel in the tenant.
	//
	// Its error is the run's error; a nil error with a result is a success
	// worth recording.
	Perform(ctx context.Context, run *Run, config map[string]any) (any, error)
}

// Executor runs a claimed run's steps in order.
type Executor struct {
	repo    *Repository
	actions map[string]Action
	logger  *slog.Logger
}

func NewExecutor(repo *Repository, logger *slog.Logger, actions ...Action) *Executor {
	if logger == nil {
		logger = slog.Default()
	}
	byKind := make(map[string]Action, len(actions))
	for _, a := range actions {
		byKind[a.Kind()] = a
	}
	return &Executor{repo: repo, actions: byKind, logger: logger}
}

// Run executes every step, in order, stopping at the first failure.
//
// STOPPING IS THE RIGHT ANSWER. A workflow is a sequence somebody wrote as a
// sequence: continuing past a failed step runs the later ones against a state
// the author never anticipated, and "notify the customer" after "check the
// customer exists" failed is worse than doing nothing.
//
// Every step claims its effect FIRST. A run that is retried after a crash skips
// the steps that already ran, which is what makes the worker free to nak
// whenever it is unsure.
func (e *Executor) Run(ctx context.Context, run *Run, steps []Step) error {
	// A RUN WITH NO OWNER MUST NOT EXECUTE. Every action authorizes against the
	// owner, so an ownerless run is one whose steps have no authority to check
	// and would reach every object in the tenant. Failing it is the fail-closed
	// reading; skipping the check is the authorization bypass.
	if run.OwnerID == "" {
		_ = e.repo.FinishRun(ctx, run.ID, "failed",
			"this run has no owner, so no step can be authorized")
		return nil
	}

	payload := map[string]any{}
	if len(run.Payload) > 0 {
		if err := json.Unmarshal(run.Payload, &payload); err != nil {
			// A payload we cannot read is a run we cannot execute, and it will
			// never become readable. Fail it permanently rather than retrying
			// forever.
			_ = e.repo.FinishRun(ctx, run.ID, "failed", "unreadable trigger payload")
			return nil
		}
	}

	for i, step := range steps {
		action, ok := e.actions[step.Kind]
		if !ok {
			// A step whose action this deployment does not have. FAILS the run.
			//
			// It used to be recorded 'skipped' with the run still finishing
			// 'succeeded', which is the worst available answer: an executor
			// built with no adapters would report every run a success having
			// done nothing at all, and the run list — the only place anybody
			// looks — would be a wall of green. A workflow that cannot perform
			// its steps has not succeeded.
			_ = e.repo.FinishStep(ctx, run.ID, i, step.Kind, "failed",
				"this deployment has no action for step kind "+step.Kind, nil)
			_ = e.repo.FinishRun(ctx, run.ID, "failed",
				fmt.Sprintf("step %d: no action registered for kind %q", i, step.Kind))
			return nil
		}

		fresh, err := e.repo.ClaimEffect(ctx, run.ID, i, step.Kind)
		if err != nil {
			return fmt.Errorf("claim effect for step %d: %w", i, err)
		}
		if !fresh {
			// A previous delivery already performed this. Skipping is the
			// entire point of the effects table.
			e.logger.Debug("workflow step already performed", "run_id", run.ID, "step", i)
			_ = e.repo.FinishStep(ctx, run.ID, i, step.Kind, "succeeded", "",
				map[string]any{"deduplicated": true})
			continue
		}

		result, err := action.Perform(ctx, run, mergeConfig(step.Config, payload))
		if err != nil {
			_ = e.repo.FinishStep(ctx, run.ID, i, step.Kind, "failed", err.Error(), nil)
			_ = e.repo.FinishRun(ctx, run.ID, "failed",
				fmt.Sprintf("step %d (%s): %v", i, step.Kind, err))
			return nil
		}
		e.repo.RecordEffectResult(ctx, run.ID, i, result)
		_ = e.repo.FinishStep(ctx, run.ID, i, step.Kind, "succeeded", "", result)
	}

	return e.repo.FinishRun(ctx, run.ID, "succeeded", "")
}

// mergeConfig substitutes {{event.field}} references in a step's string values.
//
// Deliberately the smallest template that is useful: top-level fields of the
// triggering event, by exact name, with no expressions. A template language
// here would need its own parser, its own error reporting and its own escaping
// story — and an escaping bug in a template that renders into a chat message is
// a content-injection bug.
func mergeConfig(config map[string]any, payload map[string]any) map[string]any {
	out := make(map[string]any, len(config))
	for k, v := range config {
		s, ok := v.(string)
		if !ok {
			out[k] = v
			continue
		}
		for field, value := range payload {
			token := "{{event." + field + "}}"
			if strings.Contains(s, token) {
				s = strings.ReplaceAll(s, token, fmt.Sprint(value))
			}
		}
		// An unresolved reference is left VISIBLE rather than blanked. A
		// message reading "Hi {{event.name}}" tells the author exactly what is
		// wrong; one reading "Hi " tells them nothing.
		out[k] = s
	}
	return out
}

// The action implementations live in adapters.go, and there is deliberately no
// second set here.
//
// There WAS one: three constructors that called their port directly with no
// capability check, written before workflows had an owner to check against.
// They survived the migration that added one, and every happy-path integration
// test used them — so the tests exercised an unauthorized variant that
// production never wires, while the shipped adapters were covered only by the
// one test that tried to escalate. Two implementations of an interface whose
// doc comment says "MUST authorize" is a comment that is false half the time.
//
// Deleted rather than fixed: adapters.go already had the correct versions, and
// keeping a second set would have re-created exactly the drift that let the
// tests point at the wrong one.
