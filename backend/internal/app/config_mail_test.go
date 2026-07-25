package app

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Wick-Lim/SuperOps/backend/internal/mail"
)

func discardTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestMailDefaultsToTheLogTransport(t *testing.T) {
	validEnv(t)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	// A fresh deployment must not be able to mail real people before an operator
	// has chosen a transport on purpose.
	if cfg.Mail.Transport != mail.TransportLog {
		t.Fatalf("MAIL_TRANSPORT defaulted to %q, want %q", cfg.Mail.Transport, mail.TransportLog)
	}
	if cfg.Mail.PublicBaseURL == "" {
		t.Error("PUBLIC_BASE_URL has no default; links in messages would be relative")
	}
}

// Selecting a transport and forgetting its one required setting must stop the
// boot. Discovering it at the first invitation means a user who never receives
// their mail and no log line that says why.
func TestMailMisconfigurationFailsTheBoot(t *testing.T) {
	tests := []struct {
		name   string
		env    map[string]string
		wantIn string
	}{
		{
			name:   "unknown transport",
			env:    map[string]string{"MAIL_TRANSPORT": "sendmail"},
			wantIn: "MAIL_TRANSPORT",
		},
		{
			name: "smtp with no host",
			env: map[string]string{
				"MAIL_TRANSPORT":  mail.TransportSMTP,
				"MAIL_FROM":       "no-reply@superops.example",
				"PUBLIC_BASE_URL": "https://chat.example.com",
			},
			wantIn: "MAIL_SMTP_HOST",
		},
		{
			name: "resend with no api key",
			env: map[string]string{
				"MAIL_TRANSPORT":  mail.TransportResend,
				"MAIL_FROM":       "no-reply@superops.example",
				"PUBLIC_BASE_URL": "https://chat.example.com",
			},
			wantIn: "MAIL_RESEND_API_KEY",
		},
		{
			name: "smtp-direct with no HELO",
			env: map[string]string{
				"MAIL_TRANSPORT":  mail.TransportSMTPDirect,
				"MAIL_FROM":       "no-reply@superops.example",
				"PUBLIC_BASE_URL": "https://chat.example.com",
			},
			wantIn: "MAIL_DIRECT_HELO",
		},
		{
			name: "sending transport with no from address",
			env: map[string]string{
				"MAIL_TRANSPORT":  mail.TransportSMTP,
				"MAIL_SMTP_HOST":  "smtp.example.com",
				"PUBLIC_BASE_URL": "https://chat.example.com",
			},
			wantIn: "MAIL_FROM",
		},
		{
			name: "credentials in the clear without an explicit opt-in",
			env: map[string]string{
				"MAIL_TRANSPORT":     mail.TransportSMTP,
				"MAIL_FROM":          "no-reply@superops.example",
				"MAIL_SMTP_HOST":     "relay.internal",
				"MAIL_SMTP_TLS":      mail.TLSNone,
				"MAIL_SMTP_USERNAME": "svc",
				"MAIL_SMTP_PASSWORD": "hunter2",
				"PUBLIC_BASE_URL":    "https://chat.example.com",
			},
			wantIn: "MAIL_SMTP_ALLOW_INSECURE_AUTH",
		},
		{
			name: "half-configured DKIM",
			env: map[string]string{
				"MAIL_TRANSPORT":     mail.TransportSMTPDirect,
				"MAIL_FROM":          "no-reply@superops.example",
				"MAIL_DIRECT_HELO":   "mail.superops.example",
				"MAIL_DKIM_DOMAIN":   "superops.example",
				"MAIL_DKIM_SELECTOR": "s1",
				"PUBLIC_BASE_URL":    "https://chat.example.com",
			},
			wantIn: "MAIL_DKIM_PRIVATE_KEY",
		},
		{
			name: "relative public base URL",
			env: map[string]string{
				"PUBLIC_BASE_URL": "/app",
			},
			wantIn: "PUBLIC_BASE_URL",
		},
		{
			name: "localhost base URL with a real transport",
			env: map[string]string{
				"MAIL_TRANSPORT":  mail.TransportSMTP,
				"MAIL_FROM":       "no-reply@superops.example",
				"MAIL_SMTP_HOST":  "smtp.example.com",
				"PUBLIC_BASE_URL": "http://localhost:8080",
			},
			wantIn: "PUBLIC_BASE_URL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			_, err := LoadConfig()
			if err == nil {
				t.Fatal("LoadConfig accepted a mail configuration that cannot work")
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("error %q does not name %s, so an operator cannot act on it", err, tt.wantIn)
			}
		})
	}
}

func TestMailAcceptsAWellFormedSMTPConfiguration(t *testing.T) {
	validEnv(t)
	t.Setenv("MAIL_TRANSPORT", mail.TransportSMTP)
	t.Setenv("MAIL_FROM", "no-reply@superops.example")
	t.Setenv("MAIL_FROM_NAME", "Acme Chat")
	t.Setenv("MAIL_SMTP_HOST", "email-smtp.eu-west-1.amazonaws.com")
	t.Setenv("MAIL_SMTP_PORT", "587")
	t.Setenv("MAIL_SMTP_USERNAME", "AKIA...")
	t.Setenv("MAIL_SMTP_PASSWORD", "secret")
	t.Setenv("PUBLIC_BASE_URL", "https://chat.example.com/")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Mail.SMTP.TLS != mail.TLSStartTLS {
		t.Errorf("MAIL_SMTP_TLS defaulted to %q, want %q", cfg.Mail.SMTP.TLS, mail.TLSStartTLS)
	}

	// The whole feature hangs off the sender and renderer constructing here.
	sender, err := NewMailSender(cfg, discardTestLogger())
	if err != nil {
		t.Fatalf("NewMailSender: %v", err)
	}
	if sender.Name() != mail.TransportSMTP {
		t.Errorf("sender is %q, want the smtp transport", sender.Name())
	}
	if _, err := NewMailRenderer(cfg); err != nil {
		t.Fatalf("NewMailRenderer: %v", err)
	}
}

func TestMailTransportNameIsCaseInsensitive(t *testing.T) {
	validEnv(t)
	t.Setenv("MAIL_TRANSPORT", "  SMTP  ")
	t.Setenv("MAIL_FROM", "no-reply@superops.example")
	t.Setenv("MAIL_SMTP_HOST", "smtp.example.com")
	t.Setenv("PUBLIC_BASE_URL", "https://chat.example.com")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig rejected a transport name with stray case and whitespace: %v", err)
	}
	if cfg.Mail.Transport != mail.TransportSMTP {
		t.Errorf("transport = %q", cfg.Mail.Transport)
	}
}

func TestDKIMConfiguredIsAllOrNothing(t *testing.T) {
	full := MailDirectConfig{DKIMDomain: "d.test", DKIMSelector: "s", DKIMPrivateKey: "-----BEGIN..."}
	if !full.DKIMConfigured() {
		t.Error("a fully configured DKIM block reported as not configured")
	}

	viaFile := MailDirectConfig{DKIMDomain: "d.test", DKIMSelector: "s", DKIMPrivateKeyFile: "/run/secrets/dkim"}
	if !viaFile.DKIMConfigured() {
		t.Error("the file form of the key reported as not configured")
	}

	partial := MailDirectConfig{DKIMDomain: "d.test", DKIMSelector: "s"}
	if partial.DKIMConfigured() {
		t.Error("two of the three settings reported as configured; signing would be attempted with no key")
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for _, host := range []string{"localhost", "LOCALHOST", "127.0.0.1", "::1", "0.0.0.0"} {
		if !isLoopbackHost(host) {
			t.Errorf("isLoopbackHost(%q) = false", host)
		}
	}
	for _, host := range []string{"chat.example.com", "10.0.0.4", "192.168.1.10"} {
		if isLoopbackHost(host) {
			t.Errorf("isLoopbackHost(%q) = true", host)
		}
	}
}
