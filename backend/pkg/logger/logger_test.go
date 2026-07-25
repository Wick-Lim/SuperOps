package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
)

// TestNewLevels pins the string -> slog.Level mapping, including the fallback.
// A typo'd LOG_LEVEL must degrade to Info rather than silencing the process.
func TestNewLevels(t *testing.T) {
	tests := []struct {
		level      string
		enabled    []slog.Level
		suppressed []slog.Level
	}{
		{"debug", []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelError}, nil},
		{"info", []slog.Level{slog.LevelInfo, slog.LevelWarn}, []slog.Level{slog.LevelDebug}},
		{"warn", []slog.Level{slog.LevelWarn, slog.LevelError}, []slog.Level{slog.LevelInfo, slog.LevelDebug}},
		{"error", []slog.Level{slog.LevelError}, []slog.Level{slog.LevelWarn, slog.LevelInfo}},
		{"", []slog.Level{slog.LevelInfo}, []slog.Level{slog.LevelDebug}},
		{"nonsense", []slog.Level{slog.LevelInfo}, []slog.Level{slog.LevelDebug}},
		// The switch matches exact lowercase; anything else is the default.
		{"DEBUG", []slog.Level{slog.LevelInfo}, []slog.Level{slog.LevelDebug}},
	}

	for _, tt := range tests {
		t.Run("level="+tt.level, func(t *testing.T) {
			restoreDefault(t)
			l := New(tt.level)
			for _, lvl := range tt.enabled {
				if !l.Enabled(context.Background(), lvl) {
					t.Errorf("level %s: %v should be enabled", tt.level, lvl)
				}
			}
			for _, lvl := range tt.suppressed {
				if l.Enabled(context.Background(), lvl) {
					t.Errorf("level %s: %v should be suppressed", tt.level, lvl)
				}
			}
		})
	}
}

// TestNewInstallsTheProcessDefault is the reason New calls slog.SetDefault:
// without it every slog.Default() call — including FromContext's fallback and
// any library that logs through slog — writes plain text to stderr at Info
// level, bypassing this handler's format and level entirely.
func TestNewInstallsTheProcessDefault(t *testing.T) {
	restoreDefault(t)

	l := New("error")
	if slog.Default() != l {
		t.Error("New did not install its logger as the process default")
	}
	if slog.Default().Enabled(context.Background(), slog.LevelInfo) {
		t.Error("the process default did not adopt the configured level")
	}
}

func TestFromContext(t *testing.T) {
	restoreDefault(t)
	fallback := New("info")

	t.Run("falls back to the process default", func(t *testing.T) {
		// A handler that forgot the logging middleware must still log, not
		// dereference nil.
		if got := FromContext(context.Background()); got != fallback {
			t.Error("FromContext did not fall back to the process default")
		}
	})

	t.Run("returns the request-scoped logger", func(t *testing.T) {
		var buf bytes.Buffer
		scoped := slog.New(slog.NewJSONHandler(&buf, nil)).With("request_id", "abc123")
		ctx := WithContext(context.Background(), scoped)

		got := FromContext(ctx)
		if got != scoped {
			t.Fatal("FromContext returned a different logger")
		}

		// The request attributes must survive the round trip, which is the
		// entire point of carrying a logger on the context.
		got.Info("hello")
		var record map[string]any
		if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
			t.Fatalf("log line is not JSON: %q", buf.String())
		}
		if record["request_id"] != "abc123" {
			t.Errorf("request_id = %v, want abc123", record["request_id"])
		}
	})

	t.Run("a non-logger value falls back rather than panicking", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), ctxKey{}, "not a logger")
		if got := FromContext(ctx); got != fallback {
			t.Error("FromContext did not fall back on a value of the wrong type")
		}
	})
}

// TestKeyTypeIsPrivate: the context key is an unexported empty struct type, so
// no other package can overwrite the request logger by accident.
func TestKeyTypeIsPrivate(t *testing.T) {
	restoreDefault(t)
	fallback := New("info")

	//nolint:staticcheck // using a plain string key is the point of this test.
	ctx := context.WithValue(context.Background(), "logger", slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)))
	if got := FromContext(ctx); got != fallback {
		t.Error("FromContext picked up a value stored under a plain string key")
	}
}

// TestOutputShape pins what actually reaches the wire. The deployment ships
// these lines to a log aggregator that parses them as JSON on stdout, and
// source locations are meant to appear only at debug — they are expensive and
// they change the shape of every record.
func TestOutputShape(t *testing.T) {
	tests := []struct {
		level      string
		logAt      func(*slog.Logger)
		wantLevel  string
		wantSource bool
	}{
		{"info", func(l *slog.Logger) { l.Info("hello", "component", "test") }, "INFO", false},
		{"debug", func(l *slog.Logger) { l.Debug("hello", "component", "test") }, "DEBUG", true},
		{"warn", func(l *slog.Logger) { l.Warn("hello", "component", "test") }, "WARN", false},
	}

	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			restoreDefault(t)
			out := captureStdout(t, func() { tt.logAt(New(tt.level)) })

			var record map[string]any
			if err := json.Unmarshal([]byte(out), &record); err != nil {
				t.Fatalf("stdout is not JSON: %q", out)
			}
			if record["msg"] != "hello" || record["component"] != "test" {
				t.Errorf("record = %v, want the message and attribute", record)
			}
			if record["level"] != tt.wantLevel {
				t.Errorf("level = %v, want %s", record["level"], tt.wantLevel)
			}
			if _, ok := record["source"]; ok != tt.wantSource {
				t.Errorf("source present = %v, want %v", ok, tt.wantSource)
			}
		})
	}
}

// captureStdout redirects os.Stdout for the duration of f. New binds the
// handler to os.Stdout at construction time, so f must be where New is called.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	prev := os.Stdout
	os.Stdout = w
	f()
	os.Stdout = prev
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return string(out)
}

// restoreDefault puts slog's process default back when the test finishes. New
// mutates global state, and leaving a level=error logger installed would
// silence unrelated packages' tests in the same binary.
func restoreDefault(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
}
