package audit

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Off-box anchoring: the one layer of audit protection that the local
// administrator does not control, and therefore the only one that makes the hash
// chain more than theatre.
//
// This is a §3c deployment-dependent capability, and it follows that section's
// rules exactly, the same way internal/mail's Sender does:
//
//   - ONE interface, transport chosen by configuration.
//   - Validated AT BOOT. A transport named without its credentials is a boot
//     failure, not a first-use failure — an operator who sets AUDIT_SINK=http and
//     forgets the URL must find out at deploy time, not the first time a chain
//     needed anchoring.
//   - The DEFAULT IS SAFE RATHER THAN CONVENIENT, and it is useful rather than a
//     placeholder: 'log' writes the anchor to the process log, which in most
//     deployments is already shipped off-box by the operator's existing
//     pipeline. That is a real anchor in a real place, not a no-op wearing an
//     interface.
//   - There is an admin-triggered test that ships a REAL anchor and reports the
//     transport's REAL error — the same shape as POST /api/v1/admin/mail/test.
//
// # What is shipped, and why it is not the entries
//
// The plan's sketch was `Ship(ctx, entries []Entry)`. What is shipped here is
// the ANCHOR — (workspace, head_seq, head_hash, at) — and the deviation is
// deliberate. Shipping every entry off-box is log shipping: a much larger
// feature, with its own retry, ordering and back-pressure story, and one most
// operators already have. Shipping the head is what the tamper-evidence property
// actually needs, it is a few hundred bytes per workspace per interval, and it
// cannot itself become a second copy of the data with its own retention problem.
//
// # What is NOT here
//
// An `s3` transport. The plan lists one (a bucket with object lock and a
// write-only credential distinct from the files bucket). It is cut: object-lock
// retention modes and a genuinely write-only credential are deployment
// configuration that this code cannot verify from inside, and a transport that
// silently writes to an unlocked bucket would be exactly the "claim the system
// cannot support" this whole design is trying to avoid. `file` onto an immutable
// volume and `http` to a SIEM cover the same ground honestly.

// Anchor is one workspace's chain head at a moment in time.
type Anchor struct {
	WorkspaceID string    `json:"workspace_id"`
	HeadSeq     int64     `json:"head_seq"`
	HeadHash    string    `json:"head_hash"` // hex; empty for an empty chain
	At          time.Time `json:"at"`
}

// Sink ships anchors somewhere the local administrator cannot rewrite.
type Sink interface {
	Ship(ctx context.Context, anchors []Anchor) error
	// Name is the transport's identifier, for logs, /ready and the admin test.
	Name() string
}

// Transport names.
const (
	SinkLog  = "log"
	SinkFile = "file"
	SinkHTTP = "http"
)

// SinkConfig is the deployment's choice.
type SinkConfig struct {
	// Transport is one of SinkLog (default), SinkFile, SinkHTTP.
	Transport string
	// Path is the append-only file for SinkFile.
	Path string
	// Endpoint is the webhook for SinkHTTP.
	Endpoint string
	// Secret keys the HMAC header SinkHTTP sends. Required for SinkHTTP: an
	// anchor a receiver cannot authenticate is an anchor anybody can forge, which
	// defeats the point of shipping it.
	Secret  string
	Timeout time.Duration
	Logger  *slog.Logger
}

// NewSink builds the configured transport, or fails.
//
// Every failure here is a BOOT failure. That is the §3c rule and it is what
// separates this from a capability that looks configured and is not.
func NewSink(cfg SinkConfig) (Sink, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	switch strings.TrimSpace(cfg.Transport) {
	case "", SinkLog:
		return &logSink{logger: logger}, nil

	case SinkFile:
		if strings.TrimSpace(cfg.Path) == "" {
			return nil, errors.New("audit: AUDIT_SINK=file requires AUDIT_SINK_PATH")
		}
		// Fail at boot rather than at the first anchor: a directory that does
		// not exist, or is not writable, is a deployment mistake and must look
		// like one.
		if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o750); err != nil {
			return nil, fmt.Errorf("audit: AUDIT_SINK_PATH directory: %w", err)
		}
		f, err := os.OpenFile(cfg.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
		if err != nil {
			return nil, fmt.Errorf("audit: AUDIT_SINK_PATH is not appendable: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("audit: AUDIT_SINK_PATH probe: %w", err)
		}
		return &fileSink{path: cfg.Path}, nil

	case SinkHTTP:
		if strings.TrimSpace(cfg.Endpoint) == "" {
			return nil, errors.New("audit: AUDIT_SINK=http requires AUDIT_SINK_ENDPOINT")
		}
		if !strings.HasPrefix(cfg.Endpoint, "http://") && !strings.HasPrefix(cfg.Endpoint, "https://") {
			return nil, fmt.Errorf("audit: AUDIT_SINK_ENDPOINT must be an absolute http(s) URL (got %q)", cfg.Endpoint)
		}
		if strings.TrimSpace(cfg.Secret) == "" {
			return nil, errors.New("audit: AUDIT_SINK=http requires AUDIT_SINK_SECRET; " +
				"an anchor the receiver cannot authenticate is an anchor anybody can forge")
		}
		return &httpSink{
			endpoint: cfg.Endpoint,
			secret:   []byte(cfg.Secret),
			client:   &http.Client{Timeout: timeout},
		}, nil

	default:
		return nil, fmt.Errorf("audit: unknown AUDIT_SINK %q (want log, file or http)", cfg.Transport)
	}
}

// logSink writes anchors to the process log.
//
// The default, and useful rather than a placeholder: in most deployments the
// operator's log pipeline already ships off-box, which is precisely the property
// an anchor needs. It is written at INFO with a stable message so it can be
// grepped for, and it is the only Sink that cannot fail.
type logSink struct{ logger *slog.Logger }

func (s *logSink) Name() string { return SinkLog }

func (s *logSink) Ship(_ context.Context, anchors []Anchor) error {
	for _, a := range anchors {
		s.logger.Info("audit chain anchor",
			"workspace_id", a.WorkspaceID, "head_seq", a.HeadSeq,
			"head_hash", a.HeadHash, "at", a.At.UTC().Format(time.RFC3339Nano))
	}
	return nil
}

// fileSink appends NDJSON to a path, for a host with an immutable log volume.
//
// O_APPEND on every Ship rather than a held handle: the point of this transport
// is a volume the application cannot rewrite, and a long-lived write handle on
// one is a thing to reason about at rotation time.
type fileSink struct{ path string }

func (s *fileSink) Name() string { return SinkFile }

func (s *fileSink) Ship(_ context.Context, anchors []Anchor) error {
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("audit sink: open %s: %w", s.path, err)
	}
	defer func() { _ = f.Close() }()

	enc := json.NewEncoder(f)
	for _, a := range anchors {
		if err := enc.Encode(a); err != nil {
			return fmt.Errorf("audit sink: append anchor: %w", err)
		}
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("audit sink: sync %s: %w", s.path, err)
	}
	return nil
}

// httpSink POSTs anchors to a SIEM webhook, HMAC-signed.
type httpSink struct {
	endpoint string
	secret   []byte
	client   *http.Client
}

func (s *httpSink) Name() string { return SinkHTTP }

func (s *httpSink) Ship(ctx context.Context, anchors []Anchor) error {
	body, err := json.Marshal(map[string]any{"anchors": anchors})
	if err != nil {
		return fmt.Errorf("audit sink: encode anchors: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("audit sink: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	mac := hmac.New(sha256.New, s.secret)
	mac.Write(body)
	req.Header.Set("X-SuperOps-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("audit sink: post to %s: %w", s.endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("audit sink: %s returned %s", s.endpoint, resp.Status)
	}
	return nil
}
