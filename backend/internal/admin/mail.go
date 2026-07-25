package admin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Wick-Lim/SuperOps/backend/internal/audit"
	"github.com/Wick-Lim/SuperOps/backend/internal/mail"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

// mailTestTimeout bounds the synchronous send behind POST /admin/mail/test. It
// sits below the server's write timeout so a wedged relay answers 502 instead
// of the client seeing a truncated response.
const mailTestTimeout = 10 * time.Second

// mailPublishTimeout bounds the JetStream ack wait when queueing an invitation.
// Short on purpose: the invitation row is already committed and the URL already
// works, so a slow NATS must not slow down the HTTP response.
const mailPublishTimeout = 3 * time.Second

// MailQueue is the part of *mail.Publisher this handler uses. It is an
// interface so a test can assert that an invitation was queued without standing
// up JetStream, and so nothing here can reach for the NATS connection directly.
type MailQueue interface {
	Queue(ctx context.Context, workspaceID, kind string, msg *mail.Message) error
}

// MailDeps is everything the admin handler needs to deliver mail. The zero
// value disables delivery entirely — invitations still work, they just come
// back with `email_queued: false` and the operator copies the URL, which is
// exactly the behaviour that existed before this feature.
type MailDeps struct {
	// Publisher queues invitation mail for the worker. Sending must not block
	// an HTTP request and must survive a provider outage.
	Publisher MailQueue

	// Renderer turns typed data into a Message.
	Renderer *mail.Renderer

	// Sender is used ONLY by the configuration test, which is synchronous on
	// purpose: an operator verifying their settings needs the transport's real
	// error, and a queued message can only report "accepted for delivery".
	Sender mail.Sender

	// Secrets are scrubbed from any transport error shown to the caller.
	Secrets []string

	Logger *slog.Logger
}

func (d MailDeps) enabled() bool {
	return d.Publisher != nil && d.Renderer != nil
}

func (d MailDeps) logger() *slog.Logger {
	if d.Logger == nil {
		return slog.Default()
	}
	return d.Logger
}

// transportName reports the configured transport, or "" when mail is not wired.
func (d MailDeps) transportName() string {
	if d.Sender == nil {
		return ""
	}
	return d.Sender.Name()
}

// queueInvitationMail renders an invitation and hands it to the worker.
//
// It never returns an error to the caller's request path. The invitation row is
// the source of truth and the URL in the response still works; failing the
// whole request because NATS blinked would destroy a perfectly good invitation
// to avoid an email. The boolean is reported to the client instead, so the UI
// can stop implying that manual copy-paste is the only path.
func (h *Handler) queueInvitationMail(ctx context.Context, in invitationMail) bool {
	if !h.mail.enabled() {
		return false
	}

	log := h.mail.logger()

	workspaceName, inviterName := h.invitationContext(ctx, in.WorkspaceID, in.InviterID)

	msg, err := h.mail.Renderer.Invitation(
		mail.Address{Email: in.Email},
		mail.InvitationData{
			WorkspaceName: workspaceName,
			InviterName:   inviterName,
			Role:          in.Role,
			AcceptPath:    in.AcceptPath,
			ExpiresAt:     in.ExpiresAt,
		})
	if err != nil {
		log.Error("render invitation email", "invitation_id", in.InvitationID, "error", err)
		return false
	}
	// One key per invitation row: a retried publish collapses in JetStream's
	// duplicate window, and a provider that honours Idempotency-Key will not
	// send the same invitation twice after a lost response.
	msg.IdempotencyKey = "invitation:" + in.InvitationID

	// Deliberately not the request context: the response is about to be written
	// and cancelling the publish with it would drop the mail on a client that
	// hung up a millisecond early.
	pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mailPublishTimeout)
	defer cancel()

	if err := h.mail.Publisher.Queue(pubCtx, in.WorkspaceID, mail.KindInvitation, msg); err != nil {
		log.Error("queue invitation email; the invitation is still valid and its URL still works",
			"invitation_id", in.InvitationID, "error", err)
		return false
	}

	log.Info("invitation email queued",
		"invitation_id", in.InvitationID, "workspace_id", in.WorkspaceID, "role", in.Role)
	return true
}

// invitationMail is the data queueInvitationMail needs, gathered by the handler
// before it starts writing the response.
type invitationMail struct {
	InvitationID string
	WorkspaceID  string
	InviterID    string
	Email        string
	Role         string
	AcceptPath   string
	ExpiresAt    time.Time
}

// invitationContext reads the names that make the email readable.
//
// Best effort by design: a failure here degrades the wording, and refusing to
// send an invitation because a display name could not be read would be absurd.
func (h *Handler) invitationContext(ctx context.Context, workspaceID, inviterID string) (workspace, inviter string) {
	workspace, inviter = "your workspace", "An administrator"

	var (
		wsName      string
		inviterName string
	)
	err := h.pool.QueryRow(ctx,
		`SELECT w.name, COALESCE(NULLIF(u.full_name, ''), u.username)
		   FROM workspaces w, users u
		  WHERE w.id = $1 AND u.id = $2`,
		workspaceID, inviterID,
	).Scan(&wsName, &inviterName)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return workspace, inviter
	case err != nil:
		h.mail.logger().Warn("read invitation email context", "workspace_id", workspaceID, "error", err)
		return workspace, inviter
	}

	if wsName != "" {
		workspace = wsName
	}
	if inviterName != "" {
		inviter = inviterName
	}
	return workspace, inviter
}

// SendTestMail sends a message to the caller's own address and reports what
// happened.
//
// Synchronous, unlike every other send in this system. That is the point: mail
// misconfiguration is silent, and an operator verifying their settings needs
// the transport's real error — "550 sender address rejected", "535 authentication
// failed" — not a 202 and a promise. Queueing it would move the only useful
// information into a worker log the operator may not be able to read.
//
// It sends only to the caller's own address, so it cannot be turned into a
// relay, and it is rate limited in the composition root because triggering
// outbound mail is exactly the kind of thing worth limiting.
func (h *Handler) SendTestMail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	workspaces, actor, ok := h.scope(w, r)
	if !ok {
		return
	}

	if !h.mail.enabled() || h.mail.Sender == nil {
		httputil.JSONError(w, http.StatusServiceUnavailable, "MAIL_NOT_CONFIGURED",
			"outbound email is not configured on this deployment")
		return
	}

	var email, name string
	err := h.pool.QueryRow(ctx,
		`SELECT email, COALESCE(NULLIF(full_name, ''), username) FROM users WHERE id = $1`, actor,
	).Scan(&email, &name)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	transport := h.mail.transportName()

	msg, err := h.mail.Renderer.ConfigTest(
		mail.Address{Name: name, Email: email},
		mail.ConfigTestData{Transport: transport, RequestedBy: name, SentAt: time.Now()},
	)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	// Each test is a distinct message: an operator pressing the button twice
	// after fixing a setting must actually see two attempts.
	msg.IdempotencyKey = "mail-test:" + actor + ":" + time.Now().UTC().Format(time.RFC3339Nano)

	sendCtx, cancel := context.WithTimeout(ctx, mailTestTimeout)
	defer cancel()

	if err := h.mail.Sender.Send(sendCtx, msg); err != nil {
		h.mail.logger().Warn("mail configuration test failed",
			"transport", transport, "actor", actor, "error", err)

		code := "MAIL_TRANSPORT_UNAVAILABLE"
		if mail.IsPermanent(err) {
			code = "MAIL_CONFIG_REJECTED"
		}
		// The one place this package deliberately returns a transport error
		// verbatim. It is admin-only, rate limited, and a diagnostic that hides
		// the reason is not a diagnostic — but the credentials are scrubbed
		// first, because providers quote the request back often enough.
		httputil.JSONError(w, http.StatusBadGateway, code,
			mail.Redact(err.Error(), h.mail.Secrets...))
		return
	}

	if h.audit != nil {
		// Workspace scope is only used to attribute the action; the test itself
		// is instance-wide.
		//
		// ResourceID is empty rather than the transport name: audit_logs.resource_id
		// is a UUID column, so a value like "smtp" fails the INSERT. Combined with
		// the error being discarded, that meant this event was never recorded even
		// once. The transport belongs in the metadata, which is where a value that
		// is not an identifier goes.
		//
		// Try, not Log: the mail has already been sent by this point, so losing the
		// trail entry must be visible in the logs without failing the request.
		h.audit.Try(ctx, audit.Entry{
			WorkspaceID:  workspaces[0],
			ActorID:      actor,
			Action:       "mail.test_sent",
			ResourceType: "mail",
			Metadata:     map[string]interface{}{"to": email, "transport": transport},
		})
	}

	httputil.JSON(w, http.StatusOK, map[string]interface{}{
		"sent":      true,
		"transport": transport,
		"to":        email,
		"base_url":  h.mail.Renderer.BaseURL(),
		"note": "the transport accepted the message; whether it reaches an inbox additionally depends on " +
			"SPF, DKIM and DMARC alignment for the sending domain",
	})
}
