package mailbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	"github.com/Wick-Lim/SuperOps/backend/internal/mail"
	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
)

// Outbound delivery.
//
// AN AGENT'S REPLY WAS BEING STORED AND NEVER SENT. The handler wrote the row,
// flipped awaiting_reply, returned 201, and the customer received nothing —
// the agent was told it had worked. This is the path that makes the 201 true.
//
// The ROW is the source of truth, not the queue payload. The event carries only
// (workspace, message id); the consumer loads the row and renders from it. That
// is the opposite of the invitation mail path, which queues a rendered message,
// and the difference is deliberate: a reply is durable content an agent can see
// in the thread, so rendering from the row means what is sent is exactly what
// is displayed, and a redelivery cannot send a stale copy of an edited draft.

// OutboundFilter is the subject the worker's durable consumer binds to.
//
// FIVE tokens, where internal/mail's is four. A NATS '*' matches exactly one
// token, so `superops.*.mail.requested` cannot match
// `superops.<ws>.mail.outbound.requested` — the two consumers are genuinely
// separate and neither steals the other's messages.
const OutboundFilter = "superops.*.mail.outbound.requested"

// OutboundDurable names the consumer. Changing it orphans the old one, which
// keeps accumulating unacked messages until an operator deletes it.
const OutboundDurable = "mailer-conversation"

func outboundSubject(workspaceID string) string {
	return "superops." + workspaceID + ".mail.outbound.requested"
}

type outboundEvent struct {
	WorkspaceID string `json:"workspace_id"`
	MessageID   string `json:"mail_message_id"`
}

// Publisher queues a stored reply for delivery.
type Publisher struct {
	nats   *natspkg.Client
	logger *slog.Logger
}

func NewPublisher(nc *natspkg.Client, logger *slog.Logger) *Publisher {
	if logger == nil {
		logger = slog.Default()
	}
	return &Publisher{nats: nc, logger: logger}
}

// Queue asks for one stored message to be delivered.
//
// Deliberately does NOT fail the caller: the reply is committed and visible in
// the thread, and refusing the agent's request because a message bus was
// unreachable would destroy work to keep a delivery attempt honest. The
// consumer's own backstop — a sweep for outbound rows with a NULL sent_at —
// picks up anything this drops.
func (p *Publisher) Queue(ctx context.Context, workspaceID, messageID string) {
	if p == nil || p.nats == nil || workspaceID == "" || messageID == "" {
		return
	}
	// The message id is the dedupe key, so a retried publish collapses in the
	// stream's duplicate window rather than sending twice.
	if err := p.nats.PublishDurable(ctx, outboundSubject(workspaceID), "mail-out:"+messageID,
		natspkg.Event{
			Type: "mail.outbound.requested",
			Data: outboundEvent{WorkspaceID: workspaceID, MessageID: messageID},
		}); err != nil {
		p.logger.Warn("could not queue an outbound reply; the sweeper will retry it",
			"mail_message_id", messageID, "error", err)
	}
}

// ---------------------------------------------------------------------------
// Consumer
// ---------------------------------------------------------------------------

// Sender is internal/mail's transport.
type Sender interface {
	Send(ctx context.Context, msg *mail.Message) error
}

// Consumer delivers one stored reply.
type Consumer struct {
	pool   *pgxpool.Pool
	sender Sender
	logger *slog.Logger
}

func NewConsumer(pool *pgxpool.Pool, sender Sender, logger *slog.Logger) *Consumer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Consumer{pool: pool, sender: sender, logger: logger}
}

// Handle sends one message and stamps sent_at.
//
// THE STAMP IS THE IDEMPOTENCY GUARD. It is written conditionally on sent_at
// still being NULL, so a redelivery after a successful send finds the row
// already stamped and does nothing. The send happens BEFORE the stamp — the
// other order would mark a message sent that a transport outage then dropped,
// and a customer never hearing back is worse than an agent seeing a duplicate.
func (c *Consumer) Handle(ctx context.Context, msg *nats.Msg) error {
	var envelope struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		c.logger.Warn("outbound mail: malformed envelope", "error", err)
		return nil
	}
	var ev outboundEvent
	if err := json.Unmarshal(envelope.Data, &ev); err != nil || ev.MessageID == "" {
		c.logger.Warn("outbound mail: unusable payload", "error", err)
		return nil
	}
	return c.Deliver(ctx, ev.MessageID)
}

// Deliver sends one stored outbound message by id. Shared by the consumer and
// by the sweeper, so both take exactly the same path.
func (c *Consumer) Deliver(ctx context.Context, messageID string) error {
	_, err := c.deliver(ctx, messageID)
	return err
}

// deliver reports whether it ACTUALLY SENT, which the sweeper needs and the
// consumer does not.
//
// The distinction is not bookkeeping: Deliver returns nil for a message it
// REFUSED — an unverified sending domain, no recipient — because redelivering
// it produces the same refusal. A sweeper that counted those as delivered would
// log "delivered replies the queue had lost" on every pass of a deployment
// whose domain is simply not verified, telling an operator that customer
// replies went out when nothing did.
func (c *Consumer) deliver(ctx context.Context, messageID string) (bool, error) {
	var (
		fromAddr, fromName      string
		toAddr                  string
		subject, text, html     string
		rfcMessageID, inReplyTo string
		references              []string
		verified                bool
		alreadySent             bool
	)

	err := c.pool.QueryRow(ctx, `
		SELECT mb.address, COALESCE(NULLIF(mb.display_name, ''), mb.address),
		       conv.customer_email,
		       m.subject, m.body_text, m.body_html,
		       m.message_id, m.in_reply_to, m.references_ids,
		       -- A domain with no row at all is "not verified": the mailbox was
		       -- created before verification existed, and sending from it is
		       -- exactly what must not happen silently.
		       COALESCE(d.verified_at IS NOT NULL, FALSE),
		       m.sent_at IS NOT NULL
		  FROM mail_messages m
		  JOIN mail_conversations conv ON conv.id = m.conversation_id
		  JOIN mailboxes mb ON mb.id = conv.mailbox_id
		  LEFT JOIN mail_domains d ON d.id = mb.domain_id
		 WHERE m.id = $1 AND m.direction = 'outbound'`, messageID,
	).Scan(&fromAddr, &fromName, &toAddr, &subject, &text, &html,
		&rfcMessageID, &inReplyTo, &references, &verified, &alreadySent)
	if errors.Is(err, pgx.ErrNoRows) {
		// Not ours, or deleted. Acked: redelivering will not make it exist.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load outbound message %s: %w", messageID, err)
	}
	if alreadySent {
		return false, nil
	}
	if toAddr == "" {
		c.logger.Warn("outbound mail has no recipient", "mail_message_id", messageID)
		return false, nil
	}
	if !verified {
		// REFUSED, not retried. Sending as a domain the deployment does not
		// control is how its IP gets burned, and the migration's header says
		// so. The row keeps its NULL sent_at, so the reply is visibly
		// undelivered in the thread rather than silently lost — and it goes out
		// the moment the domain is verified.
		c.logger.Warn("refusing to send from an unverified domain",
			"mail_message_id", messageID, "from", fromAddr)
		return false, nil
	}

	out := &mail.Message{
		To:             []mail.Address{{Email: toAddr}},
		Subject:        subject,
		Text:           text,
		HTML:           html,
		From:           &mail.Address{Email: fromAddr, Name: fromName},
		MessageID:      rfcMessageID,
		InReplyTo:      inReplyTo,
		References:     references,
		IdempotencyKey: "mail-out:" + messageID,
		// The customer replies TO THE MAILBOX, which is also the From — stated
		// explicitly so a transport that rewrites the envelope sender still
		// lands the reply in the right thread.
		ReplyTo: fromAddr,
	}
	if err := c.sender.Send(ctx, out); err != nil {
		// Returned, so the consumer naks and JetStream redelivers. sent_at is
		// still NULL, so the retry is a fresh attempt rather than a duplicate.
		return false, fmt.Errorf("send outbound message %s: %w", messageID, err)
	}

	if _, err := c.pool.Exec(ctx,
		`UPDATE mail_messages SET sent_at = NOW() WHERE id = $1 AND sent_at IS NULL`,
		messageID); err != nil {
		// The mail is GONE and the stamp failed. Returning the error would make
		// the consumer redeliver and send it a second time, so this logs
		// loudly and acks: one visible inconsistency beats a duplicate email to
		// a customer.
		c.logger.Error("outbound mail was SENT but could not be stamped; it may be re-sent "+
			"by the sweeper", "mail_message_id", messageID, "error", err)
	}
	return true, nil
}

// SweepUnsent delivers outbound messages that were never queued or whose
// delivery was dropped.
//
// It exists because the publish is best effort by design — an agent's reply
// must not fail because a message bus was unreachable — so something has to
// notice what the queue lost. It is also what drains everything written before
// the delivery path existed at all.
func (c *Consumer) SweepUnsent(ctx context.Context, limit int) (int, error) {
	// ONLY MESSAGES THAT CAN ACTUALLY BE SENT.
	//
	// A reply from an unverified domain is refused every time and keeps its
	// NULL sent_at forever. Selecting those oldest-first meant a few hundred of
	// them starved every newer reply — including replies from a mailbox whose
	// domain IS verified, which the sweeper would then never reach. The join
	// filters them at the source rather than discovering the refusal per row.
	rows, err := c.pool.Query(ctx, `
		SELECT m.id::text
		  FROM mail_messages m
		  JOIN mail_conversations conv ON conv.id = m.conversation_id
		  JOIN mailboxes mb ON mb.id = conv.mailbox_id
		  JOIN mail_domains d ON d.id = mb.domain_id AND d.verified_at IS NOT NULL
		 WHERE m.direction = 'outbound'
		   AND m.sent_at IS NULL
		   AND conv.customer_email <> ''
		   -- A grace period, so this never races the consumer for a message
		   -- queued a moment ago.
		   AND m.created_at < NOW() - INTERVAL '2 minutes'
		 ORDER BY m.created_at
		 LIMIT $1`, limit)
	if err != nil {
		return 0, fmt.Errorf("list unsent mail: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan unsent mail: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	sent := 0
	for _, id := range ids {
		delivered, err := c.deliver(ctx, id)
		if err != nil {
			// One bad message must not stop the sweep. It stays unsent and the
			// next pass tries again.
			c.logger.Warn("sweep could not deliver an outbound message",
				"mail_message_id", id, "error", err)
			continue
		}
		// Counted only when something actually went out. A refusal returns nil
		// and must not be reported as a delivery.
		if delivered {
			sent++
		}
	}
	return sent, nil
}
