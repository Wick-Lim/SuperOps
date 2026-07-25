package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultResendEndpoint is Resend's send endpoint. Configurable only so a test
// can point at an httptest server.
const DefaultResendEndpoint = "https://api.resend.com/emails"

// defaultResendTimeout bounds one HTTP round trip to the provider.
const defaultResendTimeout = 20 * time.Second

// maxResendResponseBytes bounds how much of the reply is read. A send response
// is a few dozen bytes; anything larger is a proxy error page.
const maxResendResponseBytes = 1 << 16

// ResendConfig configures the HTTP transport.
type ResendConfig struct {
	APIKey string

	// Endpoint overrides DefaultResendEndpoint.
	Endpoint string

	From    Address
	Timeout time.Duration

	// HTTPClient overrides the client built from Timeout.
	HTTPClient *http.Client

	Logger *slog.Logger
}

// ResendSender posts messages to Resend's HTTP API.
//
// This exists for hosts where even the submission ports (587/465) are blocked
// outbound, which some hardened networks and a few PaaS runtimes do. It is also
// the proof that Sender is not accidentally SMTP-shaped: nothing in the
// interface assumes an envelope, a conversation or a status code.
type ResendSender struct {
	apiKey   string
	endpoint string
	from     Address
	client   *http.Client
	logger   *slog.Logger
}

func NewResendSender(cfg ResendConfig) (*ResendSender, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("mail: the resend transport requires MAIL_RESEND_API_KEY")
	}
	if err := checkAddress("sender", cfg.From); err != nil {
		return nil, fmt.Errorf("mail: MAIL_FROM is unusable: %w", err)
	}

	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = DefaultResendEndpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("mail: MAIL_RESEND_ENDPOINT must be an absolute http(s) URL (got %q)", endpoint)
	}

	client := cfg.HTTPClient
	if client == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = defaultResendTimeout
		}
		client = &http.Client{Timeout: timeout}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &ResendSender{apiKey: cfg.APIKey, endpoint: endpoint, from: cfg.From, client: client, logger: logger}, nil
}

func (s *ResendSender) Name() string { return TransportResend }

type resendRequest struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	Text    string   `json:"text,omitempty"`
	HTML    string   `json:"html,omitempty"`
	ReplyTo string   `json:"reply_to,omitempty"`
}

type resendResponse struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Message string `json:"message"`
}

func (s *ResendSender) Send(ctx context.Context, msg *Message) error {
	if err := msg.Validate(); err != nil {
		return permanent("message is not sendable", err)
	}
	if err := checkAddress("sender", s.from); err != nil {
		return err
	}

	to := make([]string, 0, len(msg.To))
	for _, a := range msg.To {
		if err := checkAddress("recipient", a); err != nil {
			return err
		}
		to = append(to, formatAddress(a))
	}

	payload, err := json.Marshal(resendRequest{
		From:    formatAddress(s.from),
		To:      to,
		Subject: msg.Subject,
		Text:    msg.Text,
		HTML:    msg.HTML,
		ReplyTo: msg.ReplyTo,
	})
	if err != nil {
		return permanent("encode resend request", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(payload))
	if err != nil {
		return permanent("build resend request", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if msg.IdempotencyKey != "" {
		// The worker redelivers on any transient failure, including one where
		// the provider accepted the message and the response was lost. This
		// header is what stops that from sending an invitation twice.
		req.Header.Set("Idempotency-Key", msg.IdempotencyKey)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		// Connection refused, DNS failure, timeout: all transient.
		return fmt.Errorf("resend: send request: %w", err)
	}
	defer func() {
		// Drain before closing so the connection can be reused; neither error
		// changes the verdict already derived from the status code.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResendResponseBytes))
	if err != nil {
		return fmt.Errorf("resend: read response: %w", err)
	}

	var parsed resendResponse
	// A non-JSON body is normal for a proxy 502; report the status instead of a
	// JSON syntax error, which says nothing useful.
	_ = json.Unmarshal(raw, &parsed)

	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		s.logger.Debug("mail accepted by provider", "transport", TransportResend,
			"provider_id", parsed.ID, "recipients", len(to), "subject", msg.Subject)
		return nil
	}

	detail := strings.TrimSpace(parsed.Message)
	if detail == "" {
		detail = strings.TrimSpace(string(raw))
	}
	summary := fmt.Sprintf("resend rejected the message: HTTP %d %s", resp.StatusCode, detail)

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		// Rate limited. Retrying after a backoff is the documented remedy.
		return errors.New(summary)
	case resp.StatusCode >= 500:
		// Provider outage. The message is fine.
		return errors.New(summary)
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		// Bad or revoked API key. Retrying is how a key gets rate-limited into
		// a lockout, and it will never start working on its own.
		return permanent("resend rejected the credentials (check MAIL_RESEND_API_KEY)", errors.New(summary))
	default:
		// 400/404/422: a malformed request, an unverified sending domain, a
		// rejected recipient. All properties of this message or this account.
		return permanent("resend rejected the message", errors.New(summary))
	}
}
