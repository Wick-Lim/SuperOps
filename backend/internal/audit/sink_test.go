package audit

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ROADMAP §3c: a transport named without its credentials is a BOOT failure, not
// a first-use failure. An operator who sets AUDIT_SINK=http and forgets the
// secret must find out at deploy time, not the first time a chain needed
// anchoring — by which point nothing has been anchored for however long the
// deployment has been up.
func TestSinkConstructionFailuresAreBootFailures(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name    string
		cfg     SinkConfig
		wantErr string
	}{
		{"unknown transport", SinkConfig{Transport: "kafka"}, "unknown AUDIT_SINK"},
		{"file without a path", SinkConfig{Transport: SinkFile}, "AUDIT_SINK_PATH"},
		{"file onto an unwritable path", SinkConfig{Transport: SinkFile, Path: filepath.Join(dir, "nope", "x", "y")},
			""},
		{"http without an endpoint", SinkConfig{Transport: SinkHTTP, Secret: "s"}, "AUDIT_SINK_ENDPOINT"},
		{"http with a relative endpoint", SinkConfig{Transport: SinkHTTP, Endpoint: "/anchors", Secret: "s"},
			"absolute http(s) URL"},
		{"http without a secret", SinkConfig{Transport: SinkHTTP, Endpoint: "https://siem.example.com/a"},
			"AUDIT_SINK_SECRET"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.cfg.Logger = quietLogger()
			s, err := NewSink(tt.cfg)
			if tt.name == "file onto an unwritable path" {
				// MkdirAll creates the tree, so this one legitimately succeeds;
				// keep it as a documented non-failure rather than deleting it,
				// because "the directory is created for you" is the behaviour.
				if err != nil {
					t.Skipf("environment refused the path: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("NewSink(%+v) = %v, nil — a misconfigured sink must not boot", tt.cfg, s)
			}
			if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not name the missing setting %q", err, tt.wantErr)
			}
		})
	}
}

// The default has to be USEFUL rather than a placeholder: in most deployments
// the operator's log pipeline already ships off-box, which is exactly the
// property an anchor needs.
func TestDefaultSinkIsLog(t *testing.T) {
	for _, transport := range []string{"", SinkLog} {
		s, err := NewSink(SinkConfig{Transport: transport, Logger: quietLogger()})
		if err != nil {
			t.Fatalf("NewSink(%q): %v", transport, err)
		}
		if s.Name() != SinkLog {
			t.Fatalf("default sink = %q, want %q", s.Name(), SinkLog)
		}
		if err := s.Ship(context.Background(), []Anchor{{WorkspaceID: "w", HeadSeq: 3}}); err != nil {
			t.Fatalf("log sink Ship: %v", err)
		}
	}
}

func TestFileSinkAppendsNDJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anchors", "audit.ndjson")
	s, err := NewSink(SinkConfig{Transport: SinkFile, Path: path, Logger: quietLogger()})
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	for i := 1; i <= 2; i++ {
		if err := s.Ship(context.Background(), []Anchor{
			{WorkspaceID: "w", HeadSeq: int64(i), HeadHash: "ab", At: now},
		}); err != nil {
			t.Fatalf("Ship: %v", err)
		}
	}

	raw, err := os.ReadFile(path) //nolint:gosec // the path is this test's own temp dir
	if err != nil {
		t.Fatalf("read sink file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines in the sink file, want 2 — the second Ship must APPEND", len(lines))
	}
	var a Anchor
	if err := json.Unmarshal([]byte(lines[1]), &a); err != nil {
		t.Fatalf("sink line is not JSON: %v", err)
	}
	if a.HeadSeq != 2 || a.WorkspaceID != "w" {
		t.Fatalf("anchor = %+v", a)
	}
}

// An anchor a receiver cannot authenticate is an anchor anybody can forge, which
// defeats the point of shipping it off-box.
func TestHTTPSinkSignsItsPayload(t *testing.T) {
	var gotSig, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSig = r.Header.Get("X-SuperOps-Signature")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	s, err := NewSink(SinkConfig{Transport: SinkHTTP, Endpoint: srv.URL, Secret: "shh", Logger: quietLogger()})
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}
	if err := s.Ship(context.Background(), []Anchor{{WorkspaceID: "w", HeadSeq: 9, HeadHash: "ff"}}); err != nil {
		t.Fatalf("Ship: %v", err)
	}

	if !strings.HasPrefix(gotSig, "sha256=") || len(gotSig) != len("sha256=")+64 {
		t.Fatalf("signature header = %q, want sha256=<64 hex>", gotSig)
	}
	if !strings.Contains(gotBody, `"head_seq":9`) {
		t.Fatalf("body = %q", gotBody)
	}
}

// A rejected anchor must be an error, not a silent success: anchored_seq only
// advances when Ship returns nil, and that invariant is what "everything at or
// below anchored_seq is off-box" rests on.
func TestHTTPSinkReportsRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	s, err := NewSink(SinkConfig{Transport: SinkHTTP, Endpoint: srv.URL, Secret: "shh", Logger: quietLogger()})
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}
	if err := s.Ship(context.Background(), []Anchor{{WorkspaceID: "w"}}); err == nil {
		t.Fatal("a 403 from the sink must be reported, not swallowed")
	}
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
