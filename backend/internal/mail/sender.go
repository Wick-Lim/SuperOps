package mail

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// SenderConfig is everything NewSender needs. The per-transport blocks are read
// only when Transport names them, so an operator does not have to supply SMTP
// settings to run with `log`.
type SenderConfig struct {
	Transport string
	From      Address

	// Timeout bounds one send attempt, whichever transport is selected. Zero
	// means the transport's own default.
	Timeout time.Duration

	SMTP   SMTPConfig
	Direct DirectConfig
	Resend ResendConfig
}

// NewSender builds the transport named by cfg.Transport.
//
// Every failure here is a boot failure, on purpose. Mail misconfiguration is
// otherwise silent: the first symptom is a user who never received an
// invitation, discovered days later. Selecting `smtp` with no host, or `resend`
// with no API key, has to stop the process.
func NewSender(cfg SenderConfig, logger *slog.Logger) (Sender, error) {
	if logger == nil {
		logger = slog.Default()
	}

	transport := strings.TrimSpace(strings.ToLower(cfg.Transport))
	if transport == "" {
		transport = TransportLog
	}

	if transport != TransportLog {
		if err := checkAddress("sender", cfg.From); err != nil {
			return nil, fmt.Errorf("mail: MAIL_FROM is required by the %q transport: %w", transport, err)
		}
	}

	switch transport {
	case TransportLog:
		return NewLogSender(logger), nil

	case TransportSMTP:
		smtpCfg := cfg.SMTP
		smtpCfg.From = cfg.From
		smtpCfg.Logger = logger
		if smtpCfg.Timeout == 0 {
			smtpCfg.Timeout = cfg.Timeout
		}
		return NewSMTPSender(smtpCfg)

	case TransportSMTPDirect:
		directCfg := cfg.Direct
		directCfg.From = cfg.From
		directCfg.Logger = logger
		if directCfg.Timeout == 0 {
			directCfg.Timeout = cfg.Timeout
		}
		return NewDirectSender(directCfg)

	case TransportResend:
		resendCfg := cfg.Resend
		resendCfg.From = cfg.From
		resendCfg.Logger = logger
		if resendCfg.Timeout == 0 {
			resendCfg.Timeout = cfg.Timeout
		}
		return NewResendSender(resendCfg)

	default:
		return nil, fmt.Errorf("mail: MAIL_TRANSPORT must be one of %q, %q, %q, %q (got %q)",
			TransportLog, TransportSMTP, TransportSMTPDirect, TransportResend, cfg.Transport)
	}
}

// IsPermanent reports whether err is one that redelivery cannot fix.
//
// It matches structurally — any error implementing `Permanent() bool` — for the
// same reason cmd/worker does: the decision must not depend on importing every
// package that can produce one.
func IsPermanent(err error) bool {
	var p interface{ Permanent() bool }
	return errors.As(err, &p) && p.Permanent()
}

// Redact removes secrets from a string that is about to be shown to a human.
//
// The admin configuration test deliberately returns the transport's real error,
// because a diagnostic that hides the reason is not a diagnostic. Provider
// errors quote the request back often enough that the credential has to be
// scrubbed on the way out.
func Redact(s string, secrets ...string) string {
	for _, secret := range secrets {
		if len(secret) < 4 {
			continue
		}
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}
	return s
}
