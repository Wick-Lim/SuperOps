package admin

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/mail"
)

// invite_url stays in the response whatever happens to the mail: an admin still
// needs it when mail is disabled, when the address bounces, or when the
// recipient cannot find the message. What is new is that the response says
// whether delivery was queued, so the UI can stop implying that copy-paste is
// the only path.
func TestCreateInvitationQueuesTheEmail(t *testing.T) {
	e := setup(t)

	email := newEmail()
	resp := e.expect(t, http.StatusCreated, http.MethodPost, "/api/v1/admin/invitations", e.f.alphaOwner,
		map[string]any{"workspace_id": e.f.alphaWS, "email": email, "role": authz.RoleMember})

	var created struct {
		ID          string `json:"id"`
		InviteURL   string `json:"invite_url"`
		EmailQueued bool   `json:"email_queued"`
	}
	decode(t, resp.Data, &created)

	if !created.EmailQueued {
		t.Fatal("email_queued is false; the UI has no way to know the invitation was actually sent")
	}
	if len(created.InviteURL) <= len("/invite/") {
		t.Errorf("invite_url = %q; the manual path must survive alongside delivery", created.InviteURL)
	}

	queued := e.mail.all()
	if len(queued) != 1 {
		t.Fatalf("%d messages queued, want 1", len(queued))
	}
	got := queued[0]

	if got.workspaceID != e.f.alphaWS {
		t.Errorf("queued on workspace %q, want %q — the subject is built from it", got.workspaceID, e.f.alphaWS)
	}
	if got.kind != mail.KindInvitation {
		t.Errorf("kind = %q, want %q", got.kind, mail.KindInvitation)
	}
	if len(got.msg.To) != 1 || got.msg.To[0].Email != email {
		t.Errorf("recipients = %+v, want just %q", got.msg.To, email)
	}
	// One key per invitation row: a retried publish collapses in JetStream's
	// duplicate window instead of sending the same invitation twice.
	if got.msg.IdempotencyKey != "invitation:"+created.ID {
		t.Errorf("idempotency key = %q, want it derived from the invitation id", got.msg.IdempotencyKey)
	}

	// The link has to be absolute and has to carry the same token the API
	// returned, or the recipient gets a URL that goes nowhere.
	token := strings.TrimPrefix(created.InviteURL, "/invite/")
	wantLink := "https://chat.example.com/invite/" + token
	for name, body := range map[string]string{"text": got.msg.Text, "html": got.msg.HTML} {
		if body == "" {
			t.Errorf("%s body is empty; both are required", name)
		}
		if !strings.Contains(body, wantLink) {
			t.Errorf("%s body does not contain %q:\n%s", name, wantLink, body)
		}
	}
	if err := got.msg.Validate(); err != nil {
		t.Errorf("queued message does not validate: %v", err)
	}
}

// The invitation row is the source of truth. A mail queue that is down must not
// destroy an invitation whose URL still works perfectly.
func TestCreateInvitationSurvivesAFailedQueuePublish(t *testing.T) {
	e := setup(t)
	e.mail.err = errors.New("nats: no responders available for request")

	resp := e.expect(t, http.StatusCreated, http.MethodPost, "/api/v1/admin/invitations", e.f.alphaOwner,
		map[string]any{"workspace_id": e.f.alphaWS, "email": newEmail()})

	var created struct {
		InviteURL   string `json:"invite_url"`
		EmailQueued bool   `json:"email_queued"`
	}
	decode(t, resp.Data, &created)

	if created.EmailQueued {
		t.Error("email_queued is true even though the publish failed")
	}
	if len(created.InviteURL) <= len("/invite/") {
		t.Errorf("invite_url = %q; the invitation must remain usable", created.InviteURL)
	}
}

// With no mail wired at all the endpoint behaves exactly as it did before this
// feature existed.
func TestCreateInvitationWithMailDisabled(t *testing.T) {
	e := setup(t)
	e.h.mail = MailDeps{}

	resp := e.expect(t, http.StatusCreated, http.MethodPost, "/api/v1/admin/invitations", e.f.alphaOwner,
		map[string]any{"workspace_id": e.f.alphaWS, "email": newEmail()})

	var created struct {
		InviteURL      string `json:"invite_url"`
		EmailQueued    bool   `json:"email_queued"`
		EmailTransport string `json:"email_transport"`
	}
	decode(t, resp.Data, &created)

	if created.EmailQueued {
		t.Error("email_queued is true with no publisher configured")
	}
	if created.EmailTransport != "" {
		t.Errorf("email_transport = %q, want it empty when mail is not configured", created.EmailTransport)
	}
	if len(created.InviteURL) <= len("/invite/") {
		t.Error("invite_url disappeared when mail was disabled")
	}
}

// --- the configuration test endpoint ------------------------------------------

// testMailSender is a Sender whose verdict the test picks.
type testMailSender struct {
	name string
	err  error
	sent []*mail.Message
}

func (s *testMailSender) Name() string { return s.name }

func (s *testMailSender) Send(_ context.Context, msg *mail.Message) error {
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, msg)
	return nil
}

func TestMailTestEndpointSendsToTheCallerOnly(t *testing.T) {
	e := setup(t)
	sender := &testMailSender{name: mail.TransportSMTP}
	e.h.mail.Sender = sender

	resp := e.expect(t, http.StatusOK, http.MethodPost, "/api/v1/admin/mail/test", e.f.alphaOwner, nil)

	var result struct {
		Sent      bool   `json:"sent"`
		Transport string `json:"transport"`
		To        string `json:"to"`
	}
	decode(t, resp.Data, &result)

	if !result.Sent || result.Transport != mail.TransportSMTP {
		t.Errorf("result = %+v, want a successful send naming the transport", result)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("%d messages sent, want 1", len(sender.sent))
	}
	// Only the caller's own address, so this cannot be turned into a relay.
	if len(sender.sent[0].To) != 1 || sender.sent[0].To[0].Email != result.To {
		t.Errorf("sent to %+v, want only the caller's own address %q", sender.sent[0].To, result.To)
	}
	if !strings.Contains(sender.sent[0].Text, mail.TransportSMTP) {
		t.Error("the test message does not name the transport it was sent through")
	}
}

func TestMailTestEndpointReportsTheRealError(t *testing.T) {
	e := setup(t)
	e.h.mail.Secrets = []string{"hunter2sekrit"}

	t.Run("permanent failures are distinguishable", func(t *testing.T) {
		e.h.mail.Sender = &testMailSender{
			name: mail.TransportSMTP,
			err:  &mail.PermanentError{Reason: "AUTH rejected: 535 5.7.8 bad password hunter2sekrit"},
		}
		resp := e.expect(t, http.StatusBadGateway, http.MethodPost, "/api/v1/admin/mail/test", e.f.alphaOwner, nil)
		if resp.Error == nil {
			t.Fatal("no error body")
		}
		if resp.Error.Code != "MAIL_CONFIG_REJECTED" {
			t.Errorf("code = %q, want MAIL_CONFIG_REJECTED", resp.Error.Code)
		}
		// A diagnostic that hides the reason is not a diagnostic...
		if !strings.Contains(resp.Error.Message, "535") {
			t.Errorf("message %q does not carry the transport's status code", resp.Error.Message)
		}
		// ...but the credential must not travel with it.
		if strings.Contains(resp.Error.Message, "hunter2sekrit") {
			t.Errorf("message %q leaked the SMTP password", resp.Error.Message)
		}
	})

	t.Run("transient failures are distinguishable", func(t *testing.T) {
		e.h.mail.Sender = &testMailSender{
			name: mail.TransportSMTP,
			err:  errors.New("smtp: dial relay.example.com:587: connection refused"),
		}
		resp := e.expect(t, http.StatusBadGateway, http.MethodPost, "/api/v1/admin/mail/test", e.f.alphaOwner, nil)
		if resp.Error == nil || resp.Error.Code != "MAIL_TRANSPORT_UNAVAILABLE" {
			t.Errorf("error = %+v, want MAIL_TRANSPORT_UNAVAILABLE", resp.Error)
		}
	})
}

func TestMailTestEndpointRequiresWorkspaceAdmin(t *testing.T) {
	e := setup(t)
	e.h.mail.Sender = &testMailSender{name: mail.TransportLog}

	// The route middleware only proves "administers something"; the handler
	// re-checks, exactly as every other /admin/* endpoint does.
	for _, actor := range []string{e.f.alphaMember, e.f.alphaGuest} {
		e.expect(t, http.StatusForbidden, http.MethodPost, "/api/v1/admin/mail/test", actor, nil)
	}
}

func TestMailTestEndpointWithMailDisabled(t *testing.T) {
	e := setup(t)
	e.h.mail = MailDeps{}

	e.expect(t, http.StatusServiceUnavailable, http.MethodPost, "/api/v1/admin/mail/test", e.f.alphaOwner, nil)
}
