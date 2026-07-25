package nats

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// These tests run against the live NATS server the deployment uses (JetStream
// enabled), not a fake. What is worth pinning here — that Publish is
// fire-and-forget core NATS, that PublishDurable waits for a storage ack and
// collapses retries through Nats-Msg-Id, and that Drain lets in-flight handlers
// finish instead of severing the socket — is behaviour of the server, and a
// mock would assert only that the wrapper calls a method.
//
// Reachability comes from NATS_URL. Unreachable means skip, unless
// SUPEROPS_REQUIRE_INFRA=1 forces a failure.

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

func requireInfra() bool {
	b, err := strconv.ParseBool(os.Getenv("SUPEROPS_REQUIRE_INFRA"))
	return err == nil && b
}

func testURL() string { return env("NATS_URL", "nats://127.0.0.1:4222") }

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

var (
	probeOnce sync.Once
	probeErr  error
)

// requireNATS skips (or fails) when the configured server is unusable. It
// probes with a plain nats.Connect — NewClient retries on a failed connect by
// design, so it is not itself a reachability check.
func requireNATS(t *testing.T) {
	t.Helper()
	probeOnce.Do(func() {
		nc, err := nats.Connect(testURL(), nats.Timeout(2*time.Second))
		probeErr = err
		if err == nil {
			nc.Close()
		}
	})
	if probeErr != nil {
		if requireInfra() {
			t.Fatalf("SUPEROPS_REQUIRE_INFRA=1 but NATS is unusable: %v", probeErr)
		}
		t.Skipf("nats unavailable, skipping: %v", probeErr)
	}
}

func newTestClient(t *testing.T) *Client {
	t.Helper()
	requireNATS(t)
	c, err := NewClient(Config{URL: testURL(), DrainTimeout: 5 * time.Second}, discard())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// subject keeps every test on its own subject tree. The prefix is deliberately
// NOT "superops.": the application's own JetStream stream captures that whole
// tree, so a test publishing there would both pollute it and see an ack from a
// stream it never created — which would quietly invalidate the "no stream
// covers this subject" assertion below.
func subject(t *testing.T) string {
	t.Helper()
	return "sotest." + strconv.FormatInt(time.Now().UnixNano(), 10)
}

func TestNewClient(t *testing.T) {
	c := newTestClient(t)

	if c.Conn == nil || !c.Conn.IsConnected() {
		t.Fatal("NewClient returned a client that is not connected")
	}
	if c.JetStream == nil {
		// The API and the worker both publish with PublishDurable; a client
		// without a JetStream context makes every one of those calls fail.
		t.Error("NewClient returned no JetStream context")
	}
}

func TestDrainTimeoutDefault(t *testing.T) {
	requireNATS(t)

	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"zero uses the default", 0, DefaultDrainTimeout},
		{"negative uses the default", -time.Second, DefaultDrainTimeout},
		{"explicit value wins", 3 * time.Second, 3 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := NewClient(Config{URL: testURL(), DrainTimeout: tt.in}, discard())
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			defer c.Close()
			if c.drainTimeout != tt.want {
				t.Errorf("drainTimeout = %v, want %v", c.drainTimeout, tt.want)
			}
		})
	}
}

func TestPublish(t *testing.T) {
	c := newTestClient(t)
	subj := subject(t)

	received := make(chan []byte, 1)
	sub, err := c.Conn.Subscribe(subj, func(m *nats.Msg) { received <- m.Data })
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := c.Conn.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// The envelope is the contract every consumer decodes: {type, data}.
	err = c.Publish(subj, Event{
		Type: "message.created",
		Data: map[string]any{"id": "m1", "channel_id": "c1"},
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case raw := <-received:
		var got struct {
			Type string         `json:"type"`
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("payload is not the event envelope: %q", raw)
		}
		if got.Type != "message.created" {
			t.Errorf("type = %q, want message.created", got.Type)
		}
		if got.Data["id"] != "m1" || got.Data["channel_id"] != "c1" {
			t.Errorf("data = %v, want the published payload", got.Data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no message arrived within 5s")
	}
}

// TestPublishIsFireAndForget documents the deliberate difference from
// PublishDurable: core NATS accepts a publish nobody is listening to, and the
// message is simply gone. That is correct for typing indicators and presence,
// and wrong for anything a consumer must not miss.
func TestPublishIsFireAndForget(t *testing.T) {
	c := newTestClient(t)
	if err := c.Publish(subject(t), Event{Type: "presence.changed"}); err != nil {
		t.Errorf("Publish to a subject with no subscriber = %v, want nil", err)
	}
}

func TestPublishMarshalFailure(t *testing.T) {
	c := newTestClient(t)

	// A channel is not encodable. The error must be reported rather than a
	// half-formed payload reaching the wire.
	err := c.Publish(subject(t), Event{Type: "bad", Data: make(chan int)})
	if err == nil {
		t.Fatal("Publish accepted an unmarshalable payload")
	}
	if got := err.Error(); !strings.Contains(got, "marshal event") {
		t.Errorf("error = %q, want it to mention marshal event", got)
	}
}

func TestPublishDurable(t *testing.T) {
	c := newTestClient(t)
	if c.JetStream == nil {
		t.Skip("JetStream is not enabled on this server")
	}

	base := subject(t)
	streamName := "SUPEROPS_TEST_" + strconv.FormatInt(time.Now().UnixNano(), 10)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	stream, err := c.JetStream.CreateStream(ctx, jetstream.StreamConfig{
		Name:       streamName,
		Subjects:   []string{base + ".>"},
		Storage:    jetstream.MemoryStorage,
		Duplicates: time.Minute,
	})
	if err != nil {
		t.Fatalf("create stream (is JetStream enabled?): %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		_ = c.JetStream.DeleteStream(cctx, streamName)
	})

	subj := base + ".created"

	t.Run("waits for the storage ack", func(t *testing.T) {
		if err := c.PublishDurable(ctx, subj, "", Event{Type: "message.created"}); err != nil {
			t.Fatalf("PublishDurable: %v", err)
		}
		// The publish is durable by the time the call returns, so the count is
		// already visible without any polling.
		if got := streamMessages(t, stream); got != 1 {
			t.Errorf("stream holds %d messages, want 1", got)
		}
	})

	t.Run("the message id collapses a retry", func(t *testing.T) {
		const msgID = "dedupe-me"
		for i := 0; i < 3; i++ {
			if err := c.PublishDurable(ctx, subj, msgID, Event{Type: "message.created"}); err != nil {
				t.Fatalf("PublishDurable (attempt %d): %v", i, err)
			}
		}
		// Three publishes of the same logical event, one stored message — this
		// is what makes at-least-once delivery safe to retry into.
		if got := streamMessages(t, stream); got != 2 {
			t.Errorf("stream holds %d messages, want 2 (one plus a single deduped copy)", got)
		}
	})

	t.Run("a subject no stream covers is an error", func(t *testing.T) {
		// Without the ack wait this would look like a success and the event
		// would simply never be delivered.
		octx, ocancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ocancel()
		if err := c.PublishDurable(octx, subject(t)+".orphan", "", Event{Type: "x"}); err == nil {
			t.Error("PublishDurable succeeded on a subject no stream covers")
		}
	})

	t.Run("an unmarshalable payload is an error", func(t *testing.T) {
		if err := c.PublishDurable(ctx, subj, "", Event{Type: "bad", Data: make(chan int)}); err == nil {
			t.Error("PublishDurable accepted an unmarshalable payload")
		}
	})
}

// TestPublishDurableWithoutJetStream covers the guard that keeps a nil
// JetStream context from panicking: app.New tolerates a server without
// JetStream, so this path is reachable in production.
func TestPublishDurableWithoutJetStream(t *testing.T) {
	c := &Client{}
	err := c.PublishDurable(context.Background(), "superops.x.message.created", "", Event{Type: "x"})
	if err == nil {
		t.Fatal("PublishDurable succeeded with no JetStream context")
	}
	if !strings.Contains(err.Error(), "JetStream is not configured") {
		t.Errorf("error = %q, want it to name the missing JetStream context", err)
	}
}

// TestDrainWaitsForHandlers is the reason Close does not just call Conn.Close():
// a hard close severs the socket, discarding whatever the indexer and notifier
// subscriptions were part-way through.
func TestDrainWaitsForHandlers(t *testing.T) {
	requireNATS(t)

	c, err := NewClient(Config{URL: testURL(), DrainTimeout: 5 * time.Second}, discard())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	subj := subject(t)
	started := make(chan struct{})
	finished := make(chan struct{})

	sub, err := c.Conn.Subscribe(subj, func(_ *nats.Msg) {
		close(started)
		// Long enough that an immediate Close would cut it off.
		time.Sleep(300 * time.Millisecond)
		close(finished)
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	if err := c.Publish(subj, Event{Type: "slow"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never started")
	}

	if err := c.Drain(); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	select {
	case <-finished:
	default:
		t.Error("Drain returned before the in-flight handler finished")
	}
	if !c.Conn.IsClosed() {
		// Conn.Drain is asynchronous — it returns as soon as draining has
		// started — so Drain has to wait for the connection to actually close.
		t.Error("Drain returned while the connection was still open")
	}
}

func TestDrainIsIdempotent(t *testing.T) {
	requireNATS(t)

	c, err := NewClient(Config{URL: testURL()}, discard())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := c.Drain(); err != nil {
		t.Fatalf("first Drain: %v", err)
	}
	// Shutdown paths call Close after an explicit Drain; the second call must
	// be a no-op rather than an error or a panic.
	if err := c.Drain(); err != nil {
		t.Errorf("second Drain = %v, want nil", err)
	}
	c.Close()

	t.Run("nil connection", func(t *testing.T) {
		empty := &Client{}
		if err := empty.Drain(); err != nil {
			t.Errorf("Drain on a client with no connection = %v, want nil", err)
		}
		empty.Close()
	})
}

func TestCloseDrains(t *testing.T) {
	requireNATS(t)

	c, err := NewClient(Config{URL: testURL()}, discard())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	c.Close()
	if !c.Conn.IsClosed() {
		t.Error("Close left the connection open")
	}
}

func streamMessages(t *testing.T, stream jetstream.Stream) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	return info.State.Msgs
}
