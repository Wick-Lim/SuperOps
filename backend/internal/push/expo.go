package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// DefaultExpoEndpoint is Expo's push API. It is configurable only so a test can
// point at an httptest server.
const DefaultExpoEndpoint = "https://exp.host/--/api/v2/push/send"

// defaultTimeout bounds one HTTP round trip. Expo is a third party on the
// public internet; without a deadline a hung connection would hold a dispatcher
// worker indefinitely.
const defaultTimeout = 15 * time.Second

// maxResponseBytes bounds how much of Expo's answer is read. A batch of 100
// receipts is a few kilobytes; anything past this is a proxy error page or a
// hostile response, not a receipt list.
const maxResponseBytes = 1 << 20

// errDeviceNotRegistered is the receipt code meaning the token is permanently
// dead. Every other documented code is either transient (MessageRateExceeded)
// or an operator problem (InvalidCredentials, MismatchSenderId) — none of them
// justify deleting the token.
const errDeviceNotRegistered = "DeviceNotRegistered"

// errInvalidCredentials is what Expo answers when the project has no APNs key
// or FCM service account configured. It is the single most likely reason push
// "does not work" in a fresh deployment, so it is logged at Error rather than
// being folded in with the transient receipt failures.
const errInvalidCredentials = "InvalidCredentials"

// ExpoConfig configures ExpoSender. Only Logger is required.
type ExpoConfig struct {
	// Endpoint overrides DefaultExpoEndpoint.
	Endpoint string
	// AccessToken is an Expo access token, required only when the Expo project
	// has "enhanced push security" switched on.
	AccessToken string
	// Timeout bounds one request. Zero means defaultTimeout.
	Timeout time.Duration
	// HTTPClient overrides the client built from Timeout.
	HTTPClient *http.Client
	// OnInvalidTokens is called with tokens Expo reported as dead. Optional, but
	// without it dead tokens accumulate forever.
	OnInvalidTokens InvalidTokenFunc
	Logger          *slog.Logger
}

// ExpoSender posts batches of messages to Expo's push API using nothing but the
// standard library.
type ExpoSender struct {
	endpoint    string
	accessToken string
	client      *http.Client
	onInvalid   InvalidTokenFunc
	logger      *slog.Logger
}

// NewExpoSender builds a Sender for Expo's push service.
func NewExpoSender(cfg ExpoConfig) *ExpoSender {
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = DefaultExpoEndpoint
	}
	client := cfg.HTTPClient
	if client == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = defaultTimeout
		}
		client = &http.Client{Timeout: timeout}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &ExpoSender{
		endpoint:    endpoint,
		accessToken: cfg.AccessToken,
		client:      client,
		onInvalid:   cfg.OnInvalidTokens,
		logger:      logger,
	}
}

// expoMessage is one entry of the request array.
//
// The field names are Expo's, not this package's: `channelId` is an Android
// notification channel, nothing to do with a SuperOps channel, which is why the
// SuperOps channel id travels inside Data instead.
type expoMessage struct {
	To       string            `json:"to"`
	Title    string            `json:"title,omitempty"`
	Body     string            `json:"body,omitempty"`
	Data     map[string]string `json:"data,omitempty"`
	Sound    string            `json:"sound,omitempty"`
	Badge    *int              `json:"badge,omitempty"`
	Priority string            `json:"priority,omitempty"`
}

// expoTicket is one entry of the response `data` array. Entries are positional:
// ticket i answers message i of the request.
type expoTicket struct {
	Status  string `json:"status"`
	ID      string `json:"id"`
	Message string `json:"message"`
	Details struct {
		Error string `json:"error"`
	} `json:"details"`
}

// expoResponse is the whole body. `errors` is populated when the request as a
// whole was rejected (bad JSON, oversized batch, bad credentials); `data` when
// it was accepted and each message got its own verdict.
type expoResponse struct {
	Data   []expoTicket `json:"data"`
	Errors []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

// Send delivers msgs, splitting them into requests of at most MaxBatchSize.
//
// Every batch is attempted even if an earlier one failed — one over-large
// message must not silence the other ninety-nine — and the failures are joined
// into the returned error.
func (s *ExpoSender) Send(ctx context.Context, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}

	var (
		errs    []error
		invalid []string
	)
	for start := 0; start < len(msgs); start += MaxBatchSize {
		end := min(start+MaxBatchSize, len(msgs))
		batch := msgs[start:end]

		dead, err := s.sendBatch(ctx, batch)
		if err != nil {
			errs = append(errs, err)
		}
		invalid = append(invalid, dead...)
	}

	// Cleanup runs even when some batch failed: the dead tokens that were
	// identified are dead regardless of what happened to the rest.
	if len(invalid) > 0 && s.onInvalid != nil {
		s.onInvalid(ctx, invalid)
	}
	return errors.Join(errs...)
}

// sendBatch posts one request and reports the tokens it proved dead.
func (s *ExpoSender) sendBatch(ctx context.Context, batch []Message) ([]string, error) {
	payload := make([]expoMessage, len(batch))
	for i, m := range batch {
		payload[i] = expoMessage{
			To:    m.Token,
			Title: m.Title,
			Body:  m.Body,
			Data:  m.Data,
			Sound: "default",
			Badge: m.Badge,
			// A chat message is the case Apple and Google mean by "high": it is
			// user-visible and time-sensitive. The default would let the OS
			// batch it into the next maintenance window.
			Priority: "high",
		}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode push batch: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build push request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if s.accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.accessToken)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send push batch: %w", err)
	}
	defer func() {
		// Drain before closing so the connection can be reused; both errors are
		// irrelevant to the caller, which already has its verdict.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read push response: %w", err)
	}

	var parsed expoResponse
	// A non-JSON body is normal for a proxy 502; report the status rather than a
	// JSON syntax error, which says nothing useful.
	decodeErr := json.Unmarshal(raw, &parsed)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if decodeErr == nil && len(parsed.Errors) > 0 {
			return nil, fmt.Errorf("push service rejected batch (HTTP %d): %s: %s",
				resp.StatusCode, parsed.Errors[0].Code, parsed.Errors[0].Message)
		}
		return nil, fmt.Errorf("push service rejected batch: HTTP %d", resp.StatusCode)
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("decode push response: %w", decodeErr)
	}
	if len(parsed.Errors) > 0 {
		return nil, fmt.Errorf("push service rejected batch: %s: %s",
			parsed.Errors[0].Code, parsed.Errors[0].Message)
	}

	// Receipts are matched to messages by position and nothing else. A length
	// mismatch means the alignment is unknown, and acting on it would delete a
	// token that some *other* message's receipt condemned. Refuse instead.
	if len(parsed.Data) != len(batch) {
		return nil, fmt.Errorf("push service returned %d receipts for %d messages; cannot attribute them",
			len(parsed.Data), len(batch))
	}

	var (
		dead   []string
		failed int
	)
	for i, ticket := range parsed.Data {
		if ticket.Status == "ok" {
			continue
		}
		failed++
		switch ticket.Details.Error {
		case errDeviceNotRegistered:
			dead = append(dead, batch[i].Token)
			s.logger.Info("push: device no longer registered, dropping token",
				"reason", ticket.Message)
		case errInvalidCredentials:
			s.logger.Error("push: the Expo project has no valid APNs/FCM credentials; no push can be delivered",
				"reason", ticket.Message)
		default:
			s.logger.Warn("push: message rejected",
				"error", ticket.Details.Error, "reason", ticket.Message)
		}
	}
	if failed > 0 {
		s.logger.Warn("push: batch partially rejected", "failed", failed, "total", len(batch))
	}
	return dead, nil
}
