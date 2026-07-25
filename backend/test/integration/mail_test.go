//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Wick-Lim/SuperOps/backend/internal/app"
	"github.com/Wick-Lim/SuperOps/backend/internal/mail"
)

// Creating an invitation must put a fully-rendered message on the stream the
// worker drains. The unit tests prove the handler calls the publisher; this
// proves the publish is durable, lands on the subject cmd/worker's durable
// consumer filters on, and survives the JSON round trip with a usable link.
func TestInvitationMailReachesTheStream(t *testing.T) {
	h := getHarness(t)

	admin := h.login(t, adminEmail, adminPass)
	workspaceID := h.firstWorkspace(t, admin)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stream, err := h.app.NATS.JetStream.Stream(ctx, app.EventStreamName)
	if err != nil {
		t.Fatalf("open stream %s: %v", app.EventStreamName, err)
	}

	// An ephemeral consumer over the same filter the worker binds, starting from
	// now so it only sees this test's invitation.
	consumer, err := stream.CreateConsumer(ctx, jetstream.ConsumerConfig{
		FilterSubject: mail.ConsumerFilter,
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverNewPolicy,
	})
	if err != nil {
		t.Fatalf("create consumer on %s: %v", mail.ConsumerFilter, err)
	}
	t.Cleanup(func() {
		_ = stream.DeleteConsumer(context.Background(), consumer.CachedInfo().Name)
	})

	email := uniqueSlug("invitee") + "@example.test"
	resp := h.req(t, http.StatusCreated, "POST", "/api/v1/admin/invitations", admin,
		map[string]string{"workspace_id": workspaceID, "email": email, "role": "member"})

	var created struct {
		ID          string `json:"id"`
		InviteURL   string `json:"invite_url"`
		EmailQueued bool   `json:"email_queued"`
	}
	decodeInto(t, resp.Data, &created)

	if !created.EmailQueued {
		t.Fatal("email_queued is false; nothing was published")
	}

	msgs, err := consumer.Fetch(1, jetstream.FetchMaxWait(10*time.Second))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	var got *mail.Request
	for msg := range msgs.Messages() {
		if !strings.HasSuffix(msg.Subject(), ".mail.requested") {
			t.Errorf("message arrived on %q", msg.Subject())
		}
		if want := mail.SubjectFor(workspaceID); msg.Subject() != want {
			t.Errorf("subject = %q, want %q", msg.Subject(), want)
		}

		var envelope struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(msg.Data(), &envelope); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		if envelope.Type != mail.EventMailRequested {
			t.Errorf("envelope type = %q, want %q", envelope.Type, mail.EventMailRequested)
		}
		var req mail.Request
		if err := json.Unmarshal(envelope.Data, &req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		got = &req
		_ = msg.Ack()
	}
	if err := msgs.Error(); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got == nil {
		t.Fatal("no mail request arrived on the stream within 10s")
	}

	if got.Kind != mail.KindInvitation {
		t.Errorf("kind = %q, want %q", got.Kind, mail.KindInvitation)
	}
	if got.Message == nil {
		t.Fatal("the queued request carries no message")
	}
	if err := got.Message.Validate(); err != nil {
		t.Errorf("queued message does not validate: %v", err)
	}
	if len(got.Message.To) != 1 || got.Message.To[0].Email != email {
		t.Errorf("recipients = %+v, want %q", got.Message.To, email)
	}
	if got.Message.IdempotencyKey != "invitation:"+created.ID {
		t.Errorf("idempotency key = %q, want it derived from the invitation id", got.Message.IdempotencyKey)
	}

	// The worker needs no database access: everything required to send is on the
	// queue, including an absolute link carrying the token the API returned once.
	token := strings.TrimPrefix(created.InviteURL, "/invite/")
	wantLink := "/invite/" + token
	for name, body := range map[string]string{"text": got.Message.Text, "html": got.Message.HTML} {
		if body == "" {
			t.Errorf("%s body is empty", name)
		}
		if !strings.Contains(body, wantLink) {
			t.Errorf("%s body does not carry the invitation link", name)
		}
		if !strings.Contains(body, "http://") && !strings.Contains(body, "https://") {
			t.Errorf("%s body has no absolute URL", name)
		}
	}

	// And the worker's consumer sends exactly this, unchanged.
	sender := &countingSender{}
	if err := mail.NewConsumer(sender, h.app.Logger).HandleRequest(ctx, natsMsgFrom(mail.SubjectFor(workspaceID), got)); err != nil {
		t.Fatalf("the worker consumer refused the message the API queued: %v", err)
	}
	if sender.sent != 1 {
		t.Errorf("consumer delivered %d messages, want 1", sender.sent)
	}
}

// countingSender stands in for a transport so the consumer's decision can be
// checked without a relay.
type countingSender struct{ sent int }

func (s *countingSender) Name() string { return "counting" }

func (s *countingSender) Send(_ context.Context, _ *mail.Message) error {
	s.sent++
	return nil
}

// natsMsgFrom rebuilds the wire message the worker's callback receives.
func natsMsgFrom(subject string, req *mail.Request) *nats.Msg {
	data, err := json.Marshal(map[string]any{"type": mail.EventMailRequested, "data": req})
	if err != nil {
		panic(err)
	}
	return &nats.Msg{Subject: subject, Data: data}
}

// The configuration test endpoint sends to the caller's own address and names
// the transport, so an operator can tell mail works before inviting anyone.
func TestMailConfigurationTestEndpoint(t *testing.T) {
	h := getHarness(t)

	admin := h.login(t, adminEmail, adminPass)
	resp := h.req(t, http.StatusOK, "POST", "/api/v1/admin/mail/test", admin, nil)

	var result struct {
		Sent      bool   `json:"sent"`
		Transport string `json:"transport"`
		To        string `json:"to"`
	}
	decodeInto(t, resp.Data, &result)

	if !result.Sent {
		t.Error("sent = false")
	}
	if result.Transport != mail.TransportLog {
		t.Errorf("transport = %q, want the configured %q", result.Transport, mail.TransportLog)
	}
	if result.To != adminEmail {
		t.Errorf("to = %q, want the caller's own address %q", result.To, adminEmail)
	}

	// It is admin-only, like every other /admin/* route.
	member := h.newUser(t, admin, h.firstWorkspace(t, admin), "mailtest")
	h.denied(t, http.StatusForbidden, "POST", "/api/v1/admin/mail/test", member.token, nil)
}
