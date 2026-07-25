package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nats-io/nats.go"

	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
)

// EventMailRequested is the envelope type on the wire. It matches the subject's
// last two elements, as every other domain event in this system does.
const EventMailRequested = "mail.requested"

// ConsumerFilter is the subject filter the worker's durable consumer binds to.
const ConsumerFilter = "superops.*.mail.requested"

// ConsumerDurable names the durable consumer. Changing it orphans the old one,
// which keeps accumulating unacked messages until an operator deletes it.
const ConsumerDurable = "mailer"

// SubjectFor builds the publish subject for a workspace.
func SubjectFor(workspaceID string) string {
	return "superops." + workspaceID + ".mail.requested"
}

// Request is what travels on the queue.
//
// It carries a fully rendered Message rather than a template name plus
// arguments. Three reasons, in order of weight:
//
//  1. The worker then needs no database access to send. A rendered invitation
//     names its workspace and its inviter; a {"template":"invitation",
//     "invitation_id":...} payload would make the worker re-read rows that may
//     have been revoked or deleted by the time it runs, and decide what to do
//     about it.
//  2. A template that fails to execute fails inside the HTTP request that
//     created it, where it is visible and testable, instead of five redeliveries
//     later in a worker log.
//  3. The privacy argument for the alternative does not survive contact: the
//     payload would still have to carry the raw invitation token, because only
//     its hash is stored. Both shapes put the same secret in JetStream.
//
// The cost is that a template change does not apply to already-queued mail.
// Queue depth here is seconds, so that is a change that lands on the next
// invitation rather than a migration problem.
type Request struct {
	WorkspaceID string `json:"workspace_id"`

	// Kind is the template that produced Message. Carried for logs and metrics
	// only — the worker does not render.
	Kind string `json:"kind"`

	Message *Message `json:"message"`
}

// Publisher queues mail requests for the worker.
type Publisher struct {
	nats   *natspkg.Client
	logger *slog.Logger
}

// NewPublisher returns a publisher, or nil when there is no NATS client. A nil
// *Publisher is usable: Queue reports that mail is not configured rather than
// panicking, so a caller does not need a branch for it.
func NewPublisher(nc *natspkg.Client, logger *slog.Logger) *Publisher {
	if nc == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Publisher{nats: nc, logger: logger}
}

// Queue publishes a send request durably.
//
// PublishDurable, not Publish: the request must survive the gap between the API
// accepting an invitation and the worker being ready to send it. A core-NATS
// publish is dropped while the worker restarts, and nothing in this system would
// ever notice that an invitation email was never sent.
//
// Message.IdempotencyKey doubles as the JetStream Nats-Msg-Id, so a caller that
// retries the publish collapses into the stream's duplicate window instead of
// sending twice.
func (p *Publisher) Queue(ctx context.Context, workspaceID, kind string, msg *Message) error {
	if p == nil {
		return fmt.Errorf("mail: not configured")
	}
	if strings.TrimSpace(workspaceID) == "" {
		return fmt.Errorf("mail: queue %s: workspace id is required for the subject", kind)
	}
	if err := msg.Validate(); err != nil {
		return fmt.Errorf("mail: queue %s: %w", kind, err)
	}

	req := Request{WorkspaceID: workspaceID, Kind: kind, Message: msg}
	return p.nats.PublishDurable(ctx, SubjectFor(workspaceID), msg.IdempotencyKey,
		natspkg.Event{Type: EventMailRequested, Data: req})
}

// Consumer sends the messages the API queued. It runs in cmd/worker behind a
// durable JetStream consumer.
type Consumer struct {
	sender Sender
	logger *slog.Logger
}

func NewConsumer(sender Sender, logger *slog.Logger) *Consumer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Consumer{sender: sender, logger: logger}
}

// HandleRequest is the durable consumer's callback, and its return value is the
// ack decision:
//
//	nil                → Ack. The transport accepted the message.
//	*PermanentError    → Term. A malformed payload, a rejected recipient, bad
//	                     credentials. Redelivery produces the same answer.
//	anything else      → Nak with backoff. A provider outage, a timeout, a 421.
//
// Nothing is logged and swallowed: an ack has to mean the mail was sent, or a
// provider outage would silently consume every invitation issued during it.
func (c *Consumer) HandleRequest(ctx context.Context, msg *nats.Msg) error {
	var envelope struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		return permanent("malformed event envelope on "+msg.Subject, err)
	}
	if envelope.Type != EventMailRequested {
		// The filter is superops.*.mail.requested, so this should not happen —
		// but an unknown type is not this consumer's to retry.
		c.logger.Debug("mail consumer ignoring unrelated event", "type", envelope.Type, "subject", msg.Subject)
		return nil
	}

	var req Request
	if err := json.Unmarshal(envelope.Data, &req); err != nil {
		return permanent("malformed mail request on "+msg.Subject, err)
	}
	if req.Message == nil {
		return permanent("mail request on "+msg.Subject+" carries no message", nil)
	}
	if err := req.Message.Validate(); err != nil {
		return permanent("queued message is not sendable", err)
	}

	if err := c.sender.Send(ctx, req.Message); err != nil {
		return err
	}

	c.logger.Info("mail sent",
		"transport", c.sender.Name(),
		"kind", req.Kind,
		"workspace_id", req.WorkspaceID,
		"recipients", len(req.Message.To),
		"idempotency_key", req.Message.IdempotencyKey,
	)
	return nil
}
