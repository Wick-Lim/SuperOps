package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"
)

// safeBuffer is a bytes.Buffer that survives slog's concurrent use under -race.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// stubSender records what it was asked to send and answers with a fixed error.
type stubSender struct {
	mu   sync.Mutex
	sent []*Message
	err  error
}

func (s *stubSender) Name() string { return "stub" }

func (s *stubSender) Send(_ context.Context, msg *Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, msg)
	return nil
}

func (s *stubSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

func natsMsg(t *testing.T, subject string, payload any) *nats.Msg {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return &nats.Msg{Subject: subject, Data: data}
}

func validRequestEvent(t *testing.T) *nats.Msg {
	t.Helper()
	msg := testMessage()
	msg.IdempotencyKey = "invitation:1"
	return natsMsg(t, SubjectFor("ws-1"), map[string]any{
		"type": EventMailRequested,
		"data": Request{WorkspaceID: "ws-1", Kind: KindInvitation, Message: msg},
	})
}

func TestSubjectForMatchesTheConsumerFilter(t *testing.T) {
	subject := SubjectFor("11111111-2222-3333-4444-555555555555")
	if subject != "superops.11111111-2222-3333-4444-555555555555.mail.requested" {
		t.Fatalf("SubjectFor = %q", subject)
	}
	// The worker binds ConsumerFilter; if the two ever drift, nothing consumes.
	parts := strings.Split(subject, ".")
	filter := strings.Split(ConsumerFilter, ".")
	if len(parts) != len(filter) {
		t.Fatalf("subject %q does not have the shape of filter %q", subject, ConsumerFilter)
	}
	for i := range filter {
		if filter[i] != "*" && filter[i] != parts[i] {
			t.Fatalf("subject %q does not match filter %q at element %d", subject, ConsumerFilter, i)
		}
	}
}

// The consumer's return value IS the ack decision — see bindDurable in
// cmd/worker. nil acks, a *PermanentError terms, anything else naks.
func TestConsumerAcksWhenTheTransportAccepts(t *testing.T) {
	sender := &stubSender{}
	consumer := NewConsumer(sender, discardLogger())

	if err := consumer.HandleRequest(context.Background(), validRequestEvent(t)); err != nil {
		t.Fatalf("HandleRequest returned %v, so the worker would nak a message that was sent", err)
	}
	if sender.count() != 1 {
		t.Fatalf("transport was called %d times, want 1", sender.count())
	}
}

func TestConsumerTermsAnUnprocessablePayload(t *testing.T) {
	cases := []struct {
		name string
		msg  *nats.Msg
	}{
		{"not JSON at all", &nats.Msg{Subject: SubjectFor("ws-1"), Data: []byte("{{{")}},
		{"data is not a request", natsMsg(t, SubjectFor("ws-1"), map[string]any{
			"type": EventMailRequested, "data": "a string, not an object",
		})},
		{"no message", natsMsg(t, SubjectFor("ws-1"), map[string]any{
			"type": EventMailRequested, "data": Request{WorkspaceID: "ws-1"},
		})},
		{"message does not validate", natsMsg(t, SubjectFor("ws-1"), map[string]any{
			"type": EventMailRequested,
			"data": Request{WorkspaceID: "ws-1", Message: &Message{Subject: "no recipients", Text: "x"}},
		})},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := &stubSender{}
			err := NewConsumer(sender, discardLogger()).HandleRequest(context.Background(), tc.msg)
			if err == nil {
				t.Fatal("HandleRequest acked an unprocessable message")
			}
			if !IsPermanent(err) {
				t.Fatalf("error is transient (%v); the worker would redeliver it five times first", err)
			}
			if sender.count() != 0 {
				t.Error("the transport was called with an unprocessable message")
			}
		})
	}
}

func TestConsumerIgnoresAnUnrelatedEventType(t *testing.T) {
	sender := &stubSender{}
	msg := natsMsg(t, SubjectFor("ws-1"), map[string]any{"type": "message.created", "data": map[string]any{}})

	if err := NewConsumer(sender, discardLogger()).HandleRequest(context.Background(), msg); err != nil {
		t.Fatalf("HandleRequest = %v; an event this consumer has no opinion about must ack", err)
	}
	if sender.count() != 0 {
		t.Error("an unrelated event reached the transport")
	}
}

func TestConsumerPropagatesTheTransportVerdict(t *testing.T) {
	t.Run("permanent stays permanent", func(t *testing.T) {
		sender := &stubSender{err: permanent("550 user unknown", nil)}
		err := NewConsumer(sender, discardLogger()).HandleRequest(context.Background(), validRequestEvent(t))
		if err == nil {
			t.Fatal("HandleRequest acked a permanently rejected message")
		}
		if !IsPermanent(err) {
			t.Fatalf("the permanent verdict was lost on the way out: %v", err)
		}
	})

	t.Run("transient stays transient", func(t *testing.T) {
		sender := &stubSender{err: errors.New("dial tcp: connection refused")}
		err := NewConsumer(sender, discardLogger()).HandleRequest(context.Background(), validRequestEvent(t))
		if err == nil {
			t.Fatal("HandleRequest acked a message the transport could not send")
		}
		if IsPermanent(err) {
			t.Fatalf("a provider outage was terminated instead of retried: %v", err)
		}
	})
}

func TestRequestSurvivesAJSONRoundTrip(t *testing.T) {
	original := Request{
		WorkspaceID: "ws-1",
		Kind:        KindInvitation,
		Message: &Message{
			To:             []Address{{Name: "Dana", Email: "dana@example.org"}},
			Subject:        "You have been invited",
			Text:           "text",
			HTML:           "<p>html</p>",
			ReplyTo:        "support@superops.example",
			IdempotencyKey: "invitation:7",
		},
	}

	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Request
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.WorkspaceID != original.WorkspaceID || got.Kind != original.Kind {
		t.Errorf("envelope fields changed: %+v", got)
	}
	if got.Message == nil || got.Message.Subject != original.Message.Subject ||
		got.Message.Text != original.Message.Text || got.Message.HTML != original.Message.HTML ||
		got.Message.ReplyTo != original.Message.ReplyTo ||
		got.Message.IdempotencyKey != original.Message.IdempotencyKey {
		t.Errorf("message changed across the queue: %+v", got.Message)
	}
	if len(got.Message.To) != 1 || got.Message.To[0] != original.Message.To[0] {
		t.Errorf("recipients changed across the queue: %+v", got.Message.To)
	}
}

func TestNilPublisherReportsRatherThanPanics(t *testing.T) {
	var p *Publisher
	if err := p.Queue(context.Background(), "ws-1", KindInvitation, testMessage()); err == nil {
		t.Fatal("a nil publisher reported success")
	}
}
