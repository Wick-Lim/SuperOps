// Package mail delivers outbound email through a transport chosen at boot.
//
// Why this is pluggable rather than "just SMTP":
//
// Almost every cloud provider blocks outbound port 25 by default — AWS, GCP,
// Azure, DigitalOcean and most VPS hosts — so a SuperOps deployed to a cloud VM
// physically cannot deliver mail itself no matter how correct its SMTP code is.
// It has to hand off to a provider. On-premises, or on a host that allows port
// 25, direct delivery is possible and avoids sending company mail through a
// third party.
//
// Both are legitimate, and which one applies is a property of the deployment,
// not of the code. So the transport is config.
//
// Deliberately absent: a provider SDK for each service. Every major provider
// (SES, Postmark, SendGrid, Mailgun, Resend) exposes an authenticated SMTP
// endpoint, so `smtp` covers all of them with the standard library. The HTTP
// transports exist for the cases where SMTP egress is also blocked, or where
// per-message provider IDs are wanted for bounce correlation.
package mail

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Transport names, as they appear in MAIL_TRANSPORT.
const (
	// TransportLog writes the message to the log instead of sending it. The
	// default, so a fresh deployment cannot silently leak mail to real people
	// before an operator has chosen a transport on purpose.
	TransportLog = "log"

	// TransportSMTP relays through an authenticated SMTP server. Covers every
	// major provider plus a corporate relay.
	TransportSMTP = "smtp"

	// TransportSMTPDirect delivers straight to the recipient's MX. On-premises
	// only — see the package comment. Requires DKIM signing and correct
	// SPF/PTR records or the mail will be treated as spam.
	TransportSMTPDirect = "smtp-direct"

	// TransportResend posts to the Resend HTTP API, for hosts where even
	// submission ports are blocked.
	TransportResend = "resend"
)

// Address is a recipient or sender. Name is optional.
type Address struct {
	Name  string
	Email string
}

func (a Address) String() string {
	if a.Name == "" {
		return a.Email
	}
	// Quoted to survive commas and non-ASCII display names.
	return fmt.Sprintf("%q <%s>", a.Name, a.Email)
}

// Message is one email. Both bodies are set for every message this product
// sends: HTML for readability, text because some clients and most spam filters
// want it, and a message with only HTML scores worse.
type Message struct {
	To      []Address
	Subject string
	Text    string
	HTML    string

	// ReplyTo is optional and usually empty — these are transactional messages
	// that nobody should reply to.
	ReplyTo string

	// IdempotencyKey deduplicates retries. The worker redelivers on failure,
	// and a provider that honours this will not send twice. Transports that
	// cannot express it ignore it.
	IdempotencyKey string
}

// Validate reports whether the message can be sent at all. Called before the
// transport so a malformed message fails as a permanent error rather than
// burning the retry budget.
func (m *Message) Validate() error {
	if len(m.To) == 0 {
		return errors.New("mail: no recipients")
	}
	for _, a := range m.To {
		if !strings.Contains(a.Email, "@") {
			return fmt.Errorf("mail: invalid recipient %q", a.Email)
		}
	}
	if strings.TrimSpace(m.Subject) == "" {
		return errors.New("mail: empty subject")
	}
	if strings.TrimSpace(m.Text) == "" && strings.TrimSpace(m.HTML) == "" {
		return errors.New("mail: empty body")
	}
	// A header injected through a subject line is the classic mail bug.
	if strings.ContainsAny(m.Subject, "\r\n") {
		return errors.New("mail: subject contains a line break")
	}
	return nil
}

// Sender delivers a message. Implementations must be safe for concurrent use.
//
// The error contract matters because the worker acts on it: return a
// *PermanentError for anything retrying cannot fix (a rejected recipient, a
// malformed message, bad credentials), and a plain error for anything
// transient (connection refused, timeout, 4xx greylisting, provider 5xx).
type Sender interface {
	Send(ctx context.Context, msg *Message) error

	// Name identifies the transport in logs and in the admin health check.
	Name() string
}

// PermanentError marks a failure that redelivery cannot fix, so the worker
// terminates the message instead of retrying it five times. Mirrors the
// convention already used by internal/search and internal/notification.
type PermanentError struct {
	Reason string
	Err    error
}

func (e *PermanentError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("mail: %s: %v", e.Reason, e.Err)
	}
	return "mail: " + e.Reason
}

func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent lets the worker detect this structurally, without importing this
// package's error type.
func (e *PermanentError) Permanent() bool { return true }

func permanent(reason string, err error) error {
	return &PermanentError{Reason: reason, Err: err}
}
