// Package mailbox is the shared inbox: email as a first-class surface.
//
// Two things here are load-bearing and everything else follows from them.
//
// THREADING IS BY RFC IDENTIFIER, NEVER BY SUBJECT. Subject-based threading
// merges every customer who ever wrote "Re: invoice" into one conversation,
// which is a cross-customer data leak dressed up as a feature. In-Reply-To and
// References are what actually identify a thread, and a message whose
// references match nothing starts a new conversation — that is the correct
// answer, not a fallback.
//
// INGEST IS IDEMPOTENT ON THE PROVIDER'S EVENT ID. Every mail provider delivers
// at least once and retries on any non-2xx, so without a dedupe key a retried
// delivery creates a second conversation with the same customer and an agent
// answers one of them.
package mailbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
)

var (
	ErrNotFound     = errors.New("mailbox: not found")
	ErrNoMailbox    = errors.New("mailbox: no mailbox serves that address")
	ErrDuplicate    = errors.New("mailbox: already processed")
	ErrNotVerified  = errors.New("mailbox: the sending domain is not verified")
	ErrAddressTaken = errors.New("mailbox: that address already exists")
)

// Conversation states, as a closed Go set mirroring the CHECK.
const (
	StateOpen    = "open"
	StatePending = "pending"
	StateClosed  = "closed"
	StateSpam    = "spam"
)

type Mailbox struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	Address     string     `json:"address"`
	DisplayName string     `json:"display_name"`
	Prefix      string     `json:"prefix"`
	ArchivedAt  *time.Time `json:"archived_at"`
	CreatedAt   time.Time  `json:"created_at"`
	AutoReply   bool       `json:"auto_reply_enabled"`
}

type Conversation struct {
	ID            string    `json:"id"`
	MailboxID     string    `json:"mailbox_id"`
	WorkspaceID   string    `json:"workspace_id"`
	Number        int64     `json:"number"`
	Reference     string    `json:"reference"` // "SUP-14", built from the mailbox prefix
	Subject       string    `json:"subject"`
	State         string    `json:"state"`
	Priority      string    `json:"priority"`
	AssigneeID    *string   `json:"assignee_id"`
	CustomerEmail string    `json:"customer_email"`
	CustomerName  string    `json:"customer_name"`
	LastMessageAt time.Time `json:"last_message_at"`
	AwaitingReply bool      `json:"awaiting_reply"`
	CreatedAt     time.Time `json:"created_at"`
}

type Message struct {
	ID             string     `json:"id"`
	ConversationID string     `json:"conversation_id"`
	Direction      string     `json:"direction"`
	MessageID      string     `json:"message_id"`
	InReplyTo      string     `json:"in_reply_to"`
	FromAddress    string     `json:"from_address"`
	FromName       string     `json:"from_name"`
	ToAddresses    []string   `json:"to_addresses"`
	CcAddresses    []string   `json:"cc_addresses"`
	Subject        string     `json:"subject"`
	BodyText       string     `json:"body_text"`
	BodyHTML       string     `json:"body_html"`
	AuthorID       *string    `json:"author_id"`
	SentAt         *time.Time `json:"sent_at"`
	CreatedAt      time.Time  `json:"created_at"`
	// Attachments are Drive files owned by this message. Non-nil even when
	// empty: a client that did `.map()` on a null would throw on every message
	// without one, which is most of them.
	Attachments []Attachment `json:"attachments"`
}

// Inbound is one parsed email, ready to be filed.
type Inbound struct {
	// ProviderEventID is the idempotency key. An empty one is REFUSED: an
	// at-least-once provider with no key produces duplicate conversations, and
	// there is no way to detect that after the fact.
	ProviderEventID string
	Recipient       string
	MessageID       string
	InReplyTo       string
	References      []string
	FromAddress     string
	FromName        string
	To              []string
	Cc              []string
	Subject         string
	BodyText        string
	BodyHTML        string
	RawKey          string
	ReceivedAt      time.Time
}

type Repository struct {
	pool  *pgxpool.Pool
	authz *authz.Checker
}

func NewRepository(pool *pgxpool.Pool, az *authz.Checker) *Repository {
	return &Repository{pool: pool, authz: az}
}

// CreateMailbox registers an address and its ACL object in one commit.
//
// The acl_object row is written HERE rather than by a backfill, for the same
// reason a Drive folder's is: a mailbox that existed without one would be
// invisible to every list query and un-grantable, and "the backfill will catch
// it" is a window in which the feature is broken.
func (r *Repository) CreateMailbox(ctx context.Context, workspaceID, address, displayName, prefix, actorID string) (*Mailbox, error) {
	address = strings.ToLower(strings.TrimSpace(address))
	prefix = strings.ToUpper(strings.TrimSpace(prefix))

	var mb Mailbox
	err := database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO mailboxes (workspace_id, address, display_name, prefix, created_by)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING id::text, workspace_id::text, address, display_name, prefix,
			          archived_at, created_at, auto_reply_enabled`,
			workspaceID, address, displayName, prefix, actorID,
		).Scan(&mb.ID, &mb.WorkspaceID, &mb.Address, &mb.DisplayName, &mb.Prefix,
			&mb.ArchivedAt, &mb.CreatedAt, &mb.AutoReply)
		if err != nil {
			if strings.Contains(err.Error(), "mailboxes_address_key") {
				return ErrAddressTaken
			}
			return fmt.Errorf("insert mailbox: %w", err)
		}

		// ACL-native, under the workspace. Agents are granted here and every
		// conversation inherits.
		if err := r.authz.RegisterTx(ctx, tx, authz.ObjectRef{Type: "mailbox", ID: mb.ID},
			authz.WorkspaceObject(workspaceID)); err != nil {
			return fmt.Errorf("register mailbox object: %w", err)
		}
		// The creator administers it, or nobody could grant anybody else.
		return r.authz.GrantTx(ctx, tx, authz.UserSubject(actorID), authz.UserSubject(actorID),
			authz.ObjectRef{Type: "mailbox", ID: mb.ID}, authz.CapAdmin)
	})
	if err != nil {
		return nil, err
	}
	return &mb, nil
}

// Ingest files one inbound email.
//
// EVERYTHING IN ONE TRANSACTION: the dedupe row, the conversation, the message
// and the counter. A crash halfway through must leave nothing, because the
// provider will retry and a partial state would make the retry produce a second
// conversation.
func (r *Repository) Ingest(ctx context.Context, in Inbound) (*Conversation, *Message, error) {
	if in.ProviderEventID == "" {
		return nil, nil, errors.New("mailbox: inbound event has no provider id, so redelivery " +
			"cannot be made safe")
	}
	recipient := strings.ToLower(strings.TrimSpace(in.Recipient))
	// A nil slice becomes SQL NULL, which BYPASSES the column default and
	// violates NOT NULL — the default only applies when the column is omitted,
	// not when NULL is supplied. An email with no Cc is the common case, so
	// without this every ordinary message fails to file.
	in.References = nonNil(in.References)
	in.To = nonNil(in.To)
	in.Cc = nonNil(in.Cc)

	var (
		conv Conversation
		msg  Message
	)
	err := database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO mail_inbound_events (provider_event_id, recipient, raw_key, status)
			VALUES ($1, $2, $3, 'received')
			ON CONFLICT (provider_event_id) DO NOTHING`,
			in.ProviderEventID, recipient, in.RawKey)
		if err != nil {
			return fmt.Errorf("record inbound event: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrDuplicate
		}

		// Which mailbox. Case-insensitive, because addresses are, and an
		// archived mailbox does not accept mail — silently filing into one
		// nobody reads is worse than bouncing.
		var mailboxID, workspaceID string
		err = tx.QueryRow(ctx, `
			SELECT id::text, workspace_id::text FROM mailboxes
			 WHERE lower(address) = $1 AND archived_at IS NULL`, recipient,
		).Scan(&mailboxID, &workspaceID)
		if errors.Is(err, pgx.ErrNoRows) {
			_, _ = tx.Exec(ctx,
				`UPDATE mail_inbound_events SET status = 'rejected', error = 'no mailbox',
				        processed_at = NOW() WHERE provider_event_id = $1`, in.ProviderEventID)
			return ErrNoMailbox
		}
		if err != nil {
			return fmt.Errorf("resolve mailbox: %w", err)
		}

		// THREADING. In-Reply-To first, then References newest-first — the
		// order a reply chain is actually built in. Subject is never consulted.
		convID, err := threadOf(ctx, tx, mailboxID, in)
		if err != nil {
			return err
		}

		if convID == "" {
			// A new conversation. The number comes from the mailbox row under
			// its own lock, so two simultaneous arrivals cannot take the same
			// one — and a rollback does not burn it, which a sequence would.
			var number int64
			if err := tx.QueryRow(ctx,
				`UPDATE mailboxes SET next_number = next_number + 1
				  WHERE id = $1 RETURNING next_number - 1`, mailboxID).Scan(&number); err != nil {
				return fmt.Errorf("take conversation number: %w", err)
			}
			err = tx.QueryRow(ctx, `
				INSERT INTO mail_conversations
				       (mailbox_id, workspace_id, number, subject, customer_email, customer_name,
				        last_message_at, awaiting_reply)
				VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7, NOW()), TRUE)
				RETURNING id::text`,
				mailboxID, workspaceID, number, in.Subject, in.FromAddress, in.FromName,
				nullTime(in.ReceivedAt)).Scan(&convID)
			if err != nil {
				return fmt.Errorf("insert conversation: %w", err)
			}
			// ACL-native, under the mailbox: an agent granted on the mailbox
			// reaches this conversation through the path, with no per-thread
			// grant to keep in sync.
			if err := r.authz.RegisterTx(ctx, tx, authz.ObjectRef{Type: "conversation", ID: convID},
				authz.ObjectRef{Type: "mailbox", ID: mailboxID}); err != nil {
				return fmt.Errorf("register conversation object: %w", err)
			}
		} else {
			// A reply re-opens a closed thread. A customer writing back to a
			// closed conversation is the single most common way an inbox loses
			// a request.
			if _, err := tx.Exec(ctx, `
				UPDATE mail_conversations
				   SET last_message_at = COALESCE($2, NOW()),
				       awaiting_reply  = TRUE,
				       state = CASE WHEN state = 'closed' THEN 'open' ELSE state END,
				       updated_at = NOW()
				 WHERE id = $1`, convID, nullTime(in.ReceivedAt)); err != nil {
				return fmt.Errorf("touch conversation: %w", err)
			}
		}

		err = tx.QueryRow(ctx, `
			INSERT INTO mail_messages
			       (conversation_id, direction, message_id, in_reply_to, references_ids,
			        from_address, from_name, to_addresses, cc_addresses, subject,
			        body_text, body_html, raw_key, sent_at)
			VALUES ($1, 'inbound', $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, COALESCE($13, NOW()))
			RETURNING id::text, created_at, sent_at`,
			convID, in.MessageID, in.InReplyTo, in.References, in.FromAddress, in.FromName,
			in.To, in.Cc, in.Subject, in.BodyText, in.BodyHTML, in.RawKey, nullTime(in.ReceivedAt),
		).Scan(&msg.ID, &msg.CreatedAt, &msg.SentAt)
		if err != nil {
			return fmt.Errorf("insert message: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE mail_inbound_events
			   SET status = 'processed', processed_at = NOW(), message_id = $2, workspace_id = $3
			 WHERE provider_event_id = $1`, in.ProviderEventID, msg.ID, workspaceID); err != nil {
			return fmt.Errorf("mark event processed: %w", err)
		}

		return tx.QueryRow(ctx, `
			SELECT c.id::text, c.mailbox_id::text, c.workspace_id::text, c.number,
			       m.prefix || '-' || c.number::text, c.subject, c.state, c.priority,
			       c.assignee_id::text, c.customer_email, c.customer_name,
			       c.last_message_at, c.awaiting_reply, c.created_at
			  FROM mail_conversations c JOIN mailboxes m ON m.id = c.mailbox_id
			 WHERE c.id = $1`, convID,
		).Scan(&conv.ID, &conv.MailboxID, &conv.WorkspaceID, &conv.Number, &conv.Reference,
			&conv.Subject, &conv.State, &conv.Priority, &conv.AssigneeID, &conv.CustomerEmail,
			&conv.CustomerName, &conv.LastMessageAt, &conv.AwaitingReply, &conv.CreatedAt)
	})
	if err != nil {
		return nil, nil, err
	}
	msg.ConversationID = conv.ID
	msg.Direction = "inbound"
	msg.MessageID = in.MessageID
	msg.FromAddress = in.FromAddress
	msg.Subject = in.Subject
	return &conv, &msg, nil
}

// threadOf finds the conversation an inbound message belongs to, by RFC
// identifier and by nothing else.
//
// SCOPED TO THE MAILBOX. A message id is supposed to be globally unique, but it
// is attacker-supplied: without the scope, forging an In-Reply-To that matches
// another tenant's message would file the email into their conversation. The
// join to mail_conversations is what makes that impossible.
func threadOf(ctx context.Context, tx pgx.Tx, mailboxID string, in Inbound) (string, error) {
	candidates := make([]string, 0, len(in.References)+1)
	if in.InReplyTo != "" {
		candidates = append(candidates, in.InReplyTo)
	}
	// Newest last in the header, so walk it backwards: the most recent
	// ancestor is the most likely thread.
	for i := len(in.References) - 1; i >= 0; i-- {
		if in.References[i] != "" {
			candidates = append(candidates, in.References[i])
		}
	}
	if len(candidates) == 0 {
		return "", nil
	}

	var convID string
	err := tx.QueryRow(ctx, `
		SELECT c.id::text
		  FROM mail_messages m
		  JOIN mail_conversations c ON c.id = m.conversation_id
		 WHERE c.mailbox_id = $1
		   AND m.message_id = ANY($2)
		 ORDER BY m.created_at DESC
		 LIMIT 1`, mailboxID, candidates).Scan(&convID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve thread: %w", err)
	}
	return convID, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
