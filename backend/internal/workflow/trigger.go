package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nats-io/nats.go"

	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
)

// TriggerSubjects is the ALLOWLIST of domain events a workflow may be triggered
// by, and it is an allowlist rather than `superops.>` for a reason that is not
// about taste.
//
// The SUPEROPS stream is InterestPolicy: a message is persisted only while some
// consumer has interest in its subject. Binding a wildcard would create
// interest in EVERY subject, so every presence transition and every typing
// indicator — today ephemeral, published and dropped — would start being
// written to disk for the benefit of a consumer that ignores them.
//
// Adding a subject here is therefore a real decision with a storage cost, and
// the list is short on purpose. Two things are deliberately absent:
//
//   - anything published with core NATS rather than PublishDurable. Those are
//     at-most-once and vanish while the worker restarts, so a workflow bound to
//     one would fire unpredictably — which is worse than not firing.
//   - presence and typing. They are high-volume, low-value, and nobody wants an
//     automation that runs every time somebody's status flickers.
var TriggerSubjects = []string{
	"superops.*.message.created",
	"superops.*.message.updated",
	"superops.*.channel.member_added",
	"superops.*.file.uploaded",
	"superops.*.file.updated",
}

// TriggerConsumer turns a domain event into workflow runs.
//
// It is the only caller of MatchingWorkflows and EnqueueRun. Its contract is
// the same as every other durable handler in this product: the returned error
// is the ack decision, and it must be nil for anything redelivery cannot fix.
type TriggerConsumer struct {
	repo   *Repository
	logger *slog.Logger
}

func NewTriggerConsumer(repo *Repository, logger *slog.Logger) *TriggerConsumer {
	if logger == nil {
		logger = slog.Default()
	}
	return &TriggerConsumer{repo: repo, logger: logger}
}

// envelope is natspkg.Event's shape. Decoded structurally rather than by
// importing the publisher's type, because five different packages publish into
// this stream and they do not share a payload type.
type envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// Handle fans one event out to every workflow that wants it.
//
// EVERY FAILURE MODE HERE IS A DECISION ABOUT ACKING:
//
//   - a malformed subject or payload is a nil error, because redelivering it
//     produces the same malformed thing forever;
//   - a database failure is a real error, because it is transient and the
//     effects table makes the retry free;
//   - a workflow that is already queued (ErrDuplicate) is a success, because
//     the work exists.
func (c *TriggerConsumer) Handle(ctx context.Context, msg *nats.Msg) error {
	workspaceID, eventType, ok := parseSubject(msg.Subject)
	if !ok {
		c.logger.Warn("workflow trigger: unusable subject", "subject", msg.Subject)
		return nil
	}

	var env envelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		c.logger.Warn("workflow trigger: malformed envelope", "subject", msg.Subject, "error", err)
		return nil
	}
	// THE SUBJECT IS THE CONTRACT, and the envelope's own type is deliberately
	// ignored.
	//
	// They do not agree, and letting the envelope win was a real bug: a message
	// published on `superops.<ws>.message.created` carries type "message.new",
	// so overriding made every trigger registered for "message.created" — the
	// name the catalogue advertises and the name TriggerSubjects binds — match
	// nothing at all. The envelope type is each publisher's internal wire name;
	// the subject is what this consumer subscribes to and what a user picks
	// from the catalogue, so it is the one that decides.

	matches, err := c.repo.MatchingWorkflows(ctx, workspaceID, eventType)
	if err != nil {
		return fmt.Errorf("match workflows for %s: %w", eventType, err)
	}
	if len(matches) == 0 {
		return nil
	}

	// THE IDEMPOTENCY KEY. JetStream's sequence identifies this delivery of
	// this message uniquely and stably across redeliveries, which is exactly
	// what the run's trigger_key needs — and it is not something this process
	// can invent, because a generated id would differ on every redelivery and
	// every retry would start another run.
	triggerKey := msgIdentity(msg)
	if triggerKey == "" {
		c.logger.Warn("workflow trigger: no stable message identity; refusing to enqueue",
			"subject", msg.Subject)
		return nil
	}

	payload := map[string]any{}
	if len(env.Data) > 0 {
		// A payload that is not an object is not a filter target and not a
		// template source. Kept as an empty map rather than failing: the
		// workflow may not reference it at all.
		_ = json.Unmarshal(env.Data, &payload)
	}
	payload["workspace_id"] = workspaceID

	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil
	}

	for _, m := range matches {
		if !Matches(m.Filter, payload) {
			continue
		}
		if m.OwnerID == "" {
			// A workflow with no owner cannot be authorized, so queueing it
			// would produce a run that fails at the first step. Recorded so the
			// author can see WHY nothing happened — the whole point of the
			// rejection table.
			c.repo.RecordRejection(ctx, m.WorkflowID, m.WorkspaceID, eventType, "workflow has no owner")
			continue
		}

		_, err := c.repo.EnqueueRun(ctx, m, eventType, triggerKey+":"+m.WorkflowID, encoded)
		switch {
		case errors.Is(err, ErrDuplicate):
			// Redelivery. The run exists; nothing to do.
		case err != nil:
			// Transient. Return it so the message comes back — every other
			// workflow in this loop that already enqueued will hit ErrDuplicate
			// on the retry, so the redelivery is free.
			return fmt.Errorf("enqueue run for workflow %s: %w", m.WorkflowID, err)
		default:
			c.logger.Debug("workflow queued", "workflow_id", m.WorkflowID, "event", eventType)
		}
	}
	return nil
}

// parseSubject splits "superops.<workspace>.<resource>.<action>".
func parseSubject(subject string) (workspaceID, eventType string, ok bool) {
	parts := strings.Split(subject, ".")
	if len(parts) < 4 || parts[0] != "superops" {
		return "", "", false
	}
	return parts[1], strings.Join(parts[2:], "."), true
}

// msgIdentity is the STABLE IDENTITY of a delivery: the same string on every
// redelivery of the same message, and different for every other message.
//
// It reads the stream sequence out of a header the worker sets, because
// nats.Msg.Metadata() does not work here — the worker converts a jetstream.Msg
// into a *nats.Msg to keep one handler signature for eleven consumers, and that
// conversion has no reply subject for Metadata to parse. This was not a
// theoretical gap: without the header the function returned "" for every
// message and the consumer refused to enqueue anything at all, which the first
// worker boot logged once per replayed event.
//
// Nats-Msg-Id is the fallback. Every PublishDurable call sets one; a core
// publish does not, which is the second reason the sequence is preferred.
func msgIdentity(msg *nats.Msg) string {
	if msg == nil || msg.Header == nil {
		return ""
	}
	if seq := msg.Header.Get(natspkg.HeaderStreamSequence); seq != "" {
		return "seq:" + seq
	}
	if id := msg.Header.Get("Nats-Msg-Id"); id != "" {
		return "mid:" + id
	}
	return ""
}
