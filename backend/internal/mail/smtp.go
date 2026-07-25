package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// TLS modes for MAIL_SMTP_TLS.
const (
	// TLSStartTLS upgrades a plaintext connection on the submission port (587).
	// STARTTLS is *required*: a server that does not offer it fails the send
	// rather than silently downgrading, which is the whole point of naming the
	// mode explicitly.
	TLSStartTLS = "starttls"

	// TLSImplicit dials TLS directly, for port 465.
	TLSImplicit = "implicit"

	// TLSNone is plaintext throughout. Only reasonable for a relay on localhost
	// or a trusted private network, and credentials still need
	// MAIL_SMTP_ALLOW_INSECURE_AUTH to travel over it.
	TLSNone = "none"

	// tlsOpportunistic upgrades when the server offers STARTTLS and continues in
	// plaintext when it does not. Not selectable by an operator: it is what
	// smtp-direct does, because a receiving MX either supports STARTTLS or the
	// alternative is not delivering the mail at all.
	tlsOpportunistic = "opportunistic"
)

// defaultSMTPTimeout bounds one whole SMTP conversation — dial, handshake,
// AUTH, DATA. A relay that accepts the connection and then stops responding
// would otherwise hold a worker goroutine until the process restarts.
const defaultSMTPTimeout = 30 * time.Second

// SMTPConfig configures the authenticated-relay transport.
//
// One transport covers every major provider. SES, Postmark, SendGrid, Mailgun,
// Resend and a corporate Exchange relay all expose authenticated SMTP on 587 or
// 465; the only thing that changes between them is the host and the credential
// pair. There is no per-provider code path to add.
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string

	// TLSMode is TLSStartTLS (default), TLSImplicit or TLSNone.
	TLSMode string

	// AllowInsecureAuth permits sending credentials over an unencrypted
	// connection. Off by default: SMTP AUTH PLAIN is the password in base64,
	// and a relay that cannot do TLS is almost always a local one where this is
	// a deliberate choice rather than an accident.
	AllowInsecureAuth bool

	// InsecureSkipVerify disables certificate verification. For a relay with a
	// private CA that has not been installed in the image.
	InsecureSkipVerify bool

	// HELO is the name announced in EHLO. Empty means "localhost", which every
	// authenticated relay accepts because it authenticates the session instead.
	HELO string

	From    Address
	Timeout time.Duration
	Logger  *slog.Logger
}

// SMTPSender relays through one authenticated SMTP server.
type SMTPSender struct {
	target  smtpTarget
	from    Address
	timeout time.Duration
	logger  *slog.Logger
}

func NewSMTPSender(cfg SMTPConfig) (*SMTPSender, error) {
	if strings.TrimSpace(cfg.Host) == "" {
		return nil, errors.New("mail: the smtp transport requires MAIL_SMTP_HOST")
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return nil, fmt.Errorf("mail: MAIL_SMTP_PORT must be between 1 and 65535 (got %d)", cfg.Port)
	}
	if err := checkAddress("sender", cfg.From); err != nil {
		return nil, fmt.Errorf("mail: MAIL_FROM is unusable: %w", err)
	}

	mode := cfg.TLSMode
	if mode == "" {
		mode = TLSStartTLS
	}
	switch mode {
	case TLSStartTLS, TLSImplicit, TLSNone:
	default:
		return nil, fmt.Errorf("mail: MAIL_SMTP_TLS must be %q, %q or %q (got %q)",
			TLSStartTLS, TLSImplicit, TLSNone, cfg.TLSMode)
	}

	if cfg.Username != "" && cfg.Password == "" {
		return nil, errors.New("mail: MAIL_SMTP_USERNAME is set but MAIL_SMTP_PASSWORD is empty")
	}
	if cfg.Password != "" && cfg.Username == "" {
		return nil, errors.New("mail: MAIL_SMTP_PASSWORD is set but MAIL_SMTP_USERNAME is empty")
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultSMTPTimeout
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	helo := cfg.HELO
	if helo == "" {
		helo = "localhost"
	}

	target := smtpTarget{
		addr:              net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		serverName:        cfg.Host,
		helo:              helo,
		tlsMode:           mode,
		tlsInsecure:       cfg.InsecureSkipVerify,
		allowInsecureAuth: cfg.AllowInsecureAuth,
		username:          cfg.Username,
		password:          cfg.Password,
		logger:            logger,
	}

	if mode == TLSNone && cfg.Username != "" && !cfg.AllowInsecureAuth {
		return nil, errors.New("mail: MAIL_SMTP_TLS=none with credentials set requires MAIL_SMTP_ALLOW_INSECURE_AUTH=true; " +
			"SMTP AUTH sends the password in the clear")
	}

	return &SMTPSender{target: target, from: cfg.From, timeout: timeout, logger: logger}, nil
}

func (s *SMTPSender) Name() string { return TransportSMTP }

func (s *SMTPSender) Send(ctx context.Context, msg *Message) error {
	rm, err := render(s.from, msg, time.Now())
	if err != nil {
		return err
	}

	rcpts := make([]string, 0, len(msg.To))
	for _, a := range msg.To {
		rcpts = append(rcpts, a.Email)
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	if err := deliverSMTP(ctx, s.target, s.from.Email, rcpts, rm.Bytes()); err != nil {
		return err
	}
	s.logger.Debug("mail relayed", "transport", TransportSMTP, "relay", s.target.addr,
		"recipients", len(rcpts), "subject", msg.Subject)
	return nil
}

// smtpTarget is one server to talk to. Shared by the relay transport and by
// smtp-direct, which builds one per MX host it tries.
type smtpTarget struct {
	addr              string // host:port
	serverName        string // TLS SNI / certificate name, and the EHLO peer name
	helo              string
	tlsMode           string
	tlsInsecure       bool
	allowInsecureAuth bool
	username          string
	password          string
	logger            *slog.Logger
}

// deliverSMTP runs one complete SMTP conversation.
//
// The whole permanent/transient contract of this package is decided here, in
// classifySMTP: a 5xx is the server saying "not this message, ever" and a 4xx
// is it saying "not right now". Getting that backwards means either an
// invitation retried into a rate limit forever, or a greylisted message dropped
// on its first attempt — greylisting works precisely by returning 451 to a
// first attempt.
func deliverSMTP(ctx context.Context, t smtpTarget, from string, rcpts []string, raw []byte) error {
	conn, err := dialSMTP(ctx, t)
	if err != nil {
		return err
	}

	// net/smtp has no context-aware API, so ctx is enforced two ways: a deadline
	// on the socket, and a closer that fires on cancellation. Without the closer
	// a cancelled request would still block on the deadline.
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	defer close(done)

	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			_ = conn.Close()
			return fmt.Errorf("smtp: set deadline for %s: %w", t.addr, err)
		}
	}

	client, err := smtp.NewClient(conn, t.serverName)
	if err != nil {
		_ = conn.Close()
		return classifySMTP("greeting from "+t.addr, err)
	}
	defer func() {
		// Belt and braces after Quit, and the only cleanup on every error path.
		// Whether the goodbye succeeded changes nothing about the verdict.
		_ = client.Close()
	}()

	if err := client.Hello(t.helo); err != nil {
		return classifySMTP("EHLO to "+t.addr, err)
	}

	encrypted := t.tlsMode == TLSImplicit
	if !encrypted && (t.tlsMode == TLSStartTLS || t.tlsMode == tlsOpportunistic) {
		if ok, _ := client.Extension("STARTTLS"); ok {
			tlsCfg := &tls.Config{
				ServerName: t.serverName,
				MinVersion: tls.VersionTLS12,
				// Verification is on for a configured relay and off for
				// opportunistic delivery to an arbitrary MX, where a mismatched
				// or self-signed certificate is the norm and refusing it would
				// mean falling back to plaintext anyway.
				InsecureSkipVerify: t.tlsInsecure,
			}
			if err := client.StartTLS(tlsCfg); err != nil {
				return classifySMTP("STARTTLS with "+t.addr, err)
			}
			encrypted = true
		} else if t.tlsMode == TLSStartTLS {
			return permanent(fmt.Sprintf("%s does not offer STARTTLS but MAIL_SMTP_TLS is %q", t.addr, TLSStartTLS), nil)
		}
	}

	if t.username != "" {
		if !encrypted && !t.allowInsecureAuth {
			return permanent(fmt.Sprintf(
				"refusing to send SMTP credentials to %s over an unencrypted connection; "+
					"use MAIL_SMTP_TLS=starttls/implicit, or set MAIL_SMTP_ALLOW_INSECURE_AUTH=true for a local relay", t.addr), nil)
		}
		ok, mechs := client.Extension("AUTH")
		if !ok {
			return permanent(fmt.Sprintf("%s does not offer AUTH but credentials are configured", t.addr), nil)
		}
		auth, err := chooseAuth(mechs, t.username, t.password)
		if err != nil {
			return err
		}
		if err := client.Auth(auth); err != nil {
			// A 5xx here is bad credentials, and retrying bad credentials is how
			// an account gets locked out. classifySMTP terms it.
			return classifySMTP("AUTH to "+t.addr, err)
		}
	}

	if err := client.Mail(from); err != nil {
		return classifySMTP("MAIL FROM:<"+from+"> at "+t.addr, err)
	}
	for _, rcpt := range rcpts {
		if err := client.Rcpt(rcpt); err != nil {
			return classifySMTP("RCPT TO:<"+rcpt+"> at "+t.addr, err)
		}
	}

	w, err := client.Data()
	if err != nil {
		return classifySMTP("DATA at "+t.addr, err)
	}
	if _, err := w.Write(raw); err != nil {
		// A write failure is the socket, not the message.
		return fmt.Errorf("smtp: write message to %s: %w", t.addr, err)
	}
	if err := w.Close(); err != nil {
		// The response to the final "." is the acceptance verdict; this is the
		// single most important status code in the conversation.
		return classifySMTP("end of DATA at "+t.addr, err)
	}

	if err := client.Quit(); err != nil {
		// The message was accepted before QUIT was sent. Reporting a failure
		// here would cause a redelivery of mail that has already been queued by
		// the server — a duplicate, not a fix.
		t.logger.Debug("smtp: QUIT failed after the message was accepted", "server", t.addr, "error", err)
	}
	return nil
}

func dialSMTP(ctx context.Context, t smtpTarget) (net.Conn, error) {
	dialer := &net.Dialer{}

	if t.tlsMode == TLSImplicit {
		tlsDialer := &tls.Dialer{
			NetDialer: dialer,
			Config: &tls.Config{
				ServerName:         t.serverName,
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: t.tlsInsecure,
			},
		}
		conn, err := tlsDialer.DialContext(ctx, "tcp", t.addr)
		if err != nil {
			// Transient: a refused connection, a DNS blip or a handshake failure
			// against a load balancer mid-rollout all look like this, and none of
			// them are a property of the message.
			return nil, fmt.Errorf("smtp: TLS dial %s: %w", t.addr, err)
		}
		return conn, nil
	}

	conn, err := dialer.DialContext(ctx, "tcp", t.addr)
	if err != nil {
		return nil, fmt.Errorf("smtp: dial %s: %w", t.addr, err)
	}
	return conn, nil
}

// classifySMTP maps a server reply onto the package's error contract.
//
//	5xx  → permanent. The server has made a final decision: unknown mailbox
//	       (550), message refused (554), bad credentials (535). Redelivering
//	       cannot change any of them, and redelivering 535 locks the account.
//	4xx  → transient. Explicitly including 421 (service closing) and 451/452
//	       (local error / insufficient storage), which is what greylisting and
//	       rate limiting look like from here.
//	other→ transient. A network error, a closed socket, a deadline.
func classifySMTP(stage string, err error) error {
	var protoErr *textproto.Error
	if errors.As(err, &protoErr) {
		if protoErr.Code >= 500 && protoErr.Code <= 599 {
			return permanent(stage+" rejected", err)
		}
		return fmt.Errorf("smtp: %s deferred: %w", stage, err)
	}
	return fmt.Errorf("smtp: %s: %w", stage, err)
}

// chooseAuth picks a mechanism from the server's advertised AUTH list.
//
// PLAIN is preferred and LOGIN is the fallback, which between them cover every
// provider and every corporate relay this transport targets. CRAM-MD5 is
// deliberately absent: it requires the server to store the password reversibly
// and is weaker than PLAIN inside TLS.
func chooseAuth(mechanisms, username, password string) (smtp.Auth, error) {
	offered := strings.Fields(strings.ToUpper(mechanisms))
	for _, m := range offered {
		switch m {
		case "PLAIN":
			return plainAuth{username: username, password: password}, nil
		}
	}
	for _, m := range offered {
		switch m {
		case "LOGIN":
			return &loginAuth{username: username, password: password}, nil
		}
	}
	return nil, permanent(fmt.Sprintf("server offers AUTH %s, none of which is supported (PLAIN, LOGIN)", mechanisms), nil)
}

// plainAuth is AUTH PLAIN without net/smtp.PlainAuth's built-in refusal to run
// over an unencrypted connection.
//
// That refusal is a good default and a bad policy engine: it hardcodes an
// exemption for localhost and offers no way to say "this relay is on a trusted
// private network". deliverSMTP makes the encryption decision explicitly, from
// configuration, before this type is ever constructed — see the
// allowInsecureAuth branch. Nothing is relaxed by accident.
type plainAuth struct {
	username string
	password string
}

func (a plainAuth) Start(_ *smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.username + "\x00" + a.password), nil
}

func (a plainAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if more {
		return nil, fmt.Errorf("mail: unexpected server challenge during AUTH PLAIN: %q", fromServer)
	}
	return nil, nil
}

// loginAuth is the non-standard but widely deployed AUTH LOGIN, needed by older
// Exchange relays. net/smtp decodes each base64 challenge before handing it
// over, so the steps are counted rather than string-matched — servers spell the
// prompts differently ("Username:", "User Name", "V1NlcTpN...").
type loginAuth struct {
	username string
	password string
	step     int
}

func (a *loginAuth) Start(_ *smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	a.step++
	switch a.step {
	case 1:
		return []byte(a.username), nil
	case 2:
		return []byte(a.password), nil
	default:
		return nil, fmt.Errorf("mail: unexpected third challenge during AUTH LOGIN: %q", fromServer)
	}
}
