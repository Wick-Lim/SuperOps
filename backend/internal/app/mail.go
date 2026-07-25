package app

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/Wick-Lim/SuperOps/backend/internal/mail"
)

// This file is the single place the API and the worker agree on how a mail
// transport is built. They must agree: the API's admin configuration test would
// otherwise verify a different transport from the one that actually sends, and
// "the test passed but nothing arrives" is precisely the failure this feature
// exists to eliminate.

// NewMailSender builds the transport named by MAIL_TRANSPORT.
//
// A failure is fatal in both binaries. LoadConfig has already rejected the
// obvious omissions (see MailConfig.validate); what is left here is a key that
// does not parse or a value that only the transport can judge, and neither
// improves by starting up and discovering it at the first invitation.
func NewMailSender(cfg *Config, logger *slog.Logger) (mail.Sender, error) {
	sc := mail.SenderConfig{
		Transport: cfg.Mail.Transport,
		From:      mail.Address{Name: cfg.Mail.FromName, Email: cfg.Mail.From},
		Timeout:   cfg.Mail.Timeout,
		SMTP: mail.SMTPConfig{
			Host:               cfg.Mail.SMTP.Host,
			Port:               cfg.Mail.SMTP.Port,
			Username:           cfg.Mail.SMTP.Username,
			Password:           cfg.Mail.SMTP.Password,
			TLSMode:            cfg.Mail.SMTP.TLS,
			AllowInsecureAuth:  cfg.Mail.SMTP.AllowInsecureAuth,
			InsecureSkipVerify: cfg.Mail.SMTP.InsecureSkipVerify,
			HELO:               cfg.Mail.SMTP.HELO,
		},
		Direct: mail.DirectConfig{
			HELO: cfg.Mail.Direct.HELO,
			Port: cfg.Mail.Direct.Port,
		},
		Resend: mail.ResendConfig{
			APIKey:   cfg.Mail.Resend.APIKey,
			Endpoint: cfg.Mail.Resend.Endpoint,
		},
	}

	if cfg.Mail.Transport == mail.TransportSMTPDirect && cfg.Mail.Direct.DKIMConfigured() {
		pem, err := loadDKIMKey(cfg.Mail.Direct)
		if err != nil {
			return nil, err
		}
		signer, err := mail.NewDKIMSigner(cfg.Mail.Direct.DKIMDomain, cfg.Mail.Direct.DKIMSelector, pem)
		if err != nil {
			return nil, err
		}
		sc.Direct.DKIM = signer
	}

	return mail.NewSender(sc, logger)
}

// NewMailRenderer builds the template renderer. Templates are parsed here, so a
// broken template fails the boot rather than the first invitation.
func NewMailRenderer(cfg *Config) (*mail.Renderer, error) {
	return mail.NewRenderer(mail.RendererConfig{
		BaseURL:     cfg.Mail.PublicBaseURL,
		ProductName: cfg.Mail.ProductName,
	})
}

// loadDKIMKey resolves the private key from a file or from the variable.
//
// Both shapes exist because neither works everywhere: a Kubernetes Secret
// mounts cleanly as a file, while a docker-compose `.env` cannot carry the
// newlines a PEM block needs — hence the base64 fallback.
func loadDKIMKey(cfg MailDirectConfig) ([]byte, error) {
	if path := strings.TrimSpace(cfg.DKIMPrivateKeyFile); path != "" {
		pem, err := os.ReadFile(path) //nolint:gosec // operator-supplied path, by design
		if err != nil {
			return nil, fmt.Errorf("read MAIL_DKIM_PRIVATE_KEY_FILE: %w", err)
		}
		return pem, nil
	}

	raw := strings.TrimSpace(cfg.DKIMPrivateKey)
	if raw == "" {
		return nil, errors.New("MAIL_DKIM_PRIVATE_KEY is empty")
	}
	if strings.HasPrefix(raw, "-----BEGIN") {
		return []byte(raw), nil
	}

	decoded, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(raw), ""))
	if err != nil {
		return nil, fmt.Errorf("MAIL_DKIM_PRIVATE_KEY is neither a PEM block nor its base64 encoding: %w", err)
	}
	return decoded, nil
}
