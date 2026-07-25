package mail

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func testRenderer(t *testing.T) *Renderer {
	t.Helper()
	r, err := NewRenderer(RendererConfig{BaseURL: "https://chat.example.com/", ProductName: "SuperOps"})
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}
	return r
}

func TestRendererRejectsARelativeBaseURL(t *testing.T) {
	for _, base := range []string{"", "/app", "chat.example.com", "ftp://chat.example.com"} {
		if _, err := NewRenderer(RendererConfig{BaseURL: base}); err == nil {
			t.Errorf("NewRenderer accepted base URL %q; links in email would be unusable", base)
		}
	}
}

func TestRendererStripsTheTrailingSlash(t *testing.T) {
	r := testRenderer(t)
	if r.BaseURL() != "https://chat.example.com" {
		t.Errorf("BaseURL() = %q", r.BaseURL())
	}
}

func TestInvitationHasBothBodiesAndAnAbsoluteLink(t *testing.T) {
	r := testRenderer(t)

	msg, err := r.Invitation(Address{Email: "dana@example.org"}, InvitationData{
		WorkspaceName: "Acme Engineering",
		InviterName:   "Sam Okafor",
		Role:          "member",
		AcceptPath:    "/invite/tok-123",
		ExpiresAt:     time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Invitation: %v", err)
	}

	// Both bodies, always: a message with only HTML scores worse with spam
	// filters, and a text-only one loses the button.
	if strings.TrimSpace(msg.Text) == "" {
		t.Error("invitation has no text body")
	}
	if strings.TrimSpace(msg.HTML) == "" {
		t.Error("invitation has no HTML body")
	}

	const want = "https://chat.example.com/invite/tok-123"
	if !strings.Contains(msg.Text, want) {
		t.Errorf("text body has no absolute accept link:\n%s", msg.Text)
	}
	if !strings.Contains(msg.HTML, want) {
		t.Errorf("HTML body has no absolute accept link:\n%s", msg.HTML)
	}
	// A relative link in an email is not a link.
	if strings.Contains(msg.Text, `"/invite/`) || strings.Contains(msg.HTML, `href="/invite/`) {
		t.Error("a relative /invite/ link survived rendering")
	}

	for _, body := range []string{msg.Text, msg.HTML} {
		for _, want := range []string{"Acme Engineering", "Sam Okafor", "member"} {
			if !strings.Contains(body, want) {
				t.Errorf("body is missing %q:\n%s", want, body)
			}
		}
	}
	if !strings.Contains(msg.Subject, "Acme Engineering") || !strings.Contains(msg.Subject, "Sam Okafor") {
		t.Errorf("subject = %q", msg.Subject)
	}
	if strings.ContainsAny(msg.Subject, "\r\n") {
		t.Errorf("subject %q contains a line break", msg.Subject)
	}
	if err := msg.Validate(); err != nil {
		t.Errorf("rendered invitation does not validate: %v", err)
	}
}

func TestConfigTestMessageRenders(t *testing.T) {
	r := testRenderer(t)

	msg, err := r.ConfigTest(Address{Name: "Ada", Email: "ada@example.org"}, ConfigTestData{
		Transport:   TransportSMTP,
		RequestedBy: "Ada",
		SentAt:      time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("ConfigTest: %v", err)
	}
	if !strings.Contains(msg.Subject, TransportSMTP) {
		t.Errorf("subject = %q, want it to name the transport", msg.Subject)
	}
	for _, body := range []string{msg.Text, msg.HTML} {
		if !strings.Contains(body, TransportSMTP) {
			t.Errorf("body does not name the transport:\n%s", body)
		}
	}
}

func TestRenderRejectsAnUnknownKind(t *testing.T) {
	r := testRenderer(t)
	_, err := r.Render("password_reset_that_does_not_exist_yet", []Address{{Email: "x@y.test"}}, nil)
	if err == nil {
		t.Fatal("Render accepted an unknown message kind")
	}
	if !IsPermanent(err) {
		t.Errorf("unknown kind mapped to transient (%v)", err)
	}
}

func TestTemplateURLMakesPathsAbsolute(t *testing.T) {
	td := templateData{BaseURL: "https://chat.example.com"}
	cases := map[string]string{
		"/invite/x":                   "https://chat.example.com/invite/x",
		"invite/x":                    "https://chat.example.com/invite/x",
		"":                            "https://chat.example.com",
		"https://elsewhere.test/page": "https://elsewhere.test/page",
	}
	for in, want := range cases {
		if got := td.URL(in); got != want {
			t.Errorf("URL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLogSenderPrintsRecipientsSubjectAndLinks(t *testing.T) {
	var buf safeBuffer
	sender := NewLogSender(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	if sender.Name() != TransportLog {
		t.Errorf("Name() = %q", sender.Name())
	}
	if err := sender.Send(context.Background(), testMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	out := buf.String()
	// A developer must be able to redeem an invitation from the log alone.
	for _, want := range []string{"dana@example.org", "You have been invited", "https://chat.example.com/invite/abc"} {
		if !strings.Contains(out, want) {
			t.Errorf("log output is missing %q:\n%s", want, out)
		}
	}
}

func TestLogSenderRejectsAnUnsendableMessage(t *testing.T) {
	sender := NewLogSender(discardLogger())
	err := sender.Send(context.Background(), &Message{Subject: "no recipients", Text: "x"})
	if err == nil {
		t.Fatal("the log transport accepted a message with no recipients")
	}
	if !IsPermanent(err) {
		t.Errorf("error is transient (%v); it would be retried forever", err)
	}
}

func TestExtractLinksDeduplicatesAndTrimsPunctuation(t *testing.T) {
	got := extractLinks("Go to https://a.test/x. Or https://a.test/x, or https://b.test/y)")
	want := []string{"https://a.test/x", "https://b.test/y"}
	if len(got) != len(want) {
		t.Fatalf("extractLinks = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("extractLinks[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestNewSenderSelectsTheTransport(t *testing.T) {
	from := Address{Name: "SuperOps", Email: "no-reply@superops.example"}

	s, err := NewSender(SenderConfig{Transport: "", From: from}, discardLogger())
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	if s.Name() != TransportLog {
		t.Errorf("the empty transport resolved to %q, want the log transport as the safe default", s.Name())
	}

	if _, err := NewSender(SenderConfig{Transport: "sendmail", From: from}, discardLogger()); err == nil {
		t.Error("NewSender accepted an unknown transport name")
	}

	// Selecting a transport without its required setting must fail here, not at
	// the first invitation.
	if _, err := NewSender(SenderConfig{Transport: TransportSMTP, From: from}, discardLogger()); err == nil {
		t.Error("NewSender accepted the smtp transport with no host")
	}
	if _, err := NewSender(SenderConfig{Transport: TransportResend, From: from}, discardLogger()); err == nil {
		t.Error("NewSender accepted the resend transport with no API key")
	}
	if _, err := NewSender(SenderConfig{Transport: TransportSMTPDirect, From: from}, discardLogger()); err == nil {
		t.Error("NewSender accepted smtp-direct with no HELO name")
	}
	if _, err := NewSender(SenderConfig{Transport: TransportSMTP, From: Address{}}, discardLogger()); err == nil {
		t.Error("NewSender accepted a sending transport with no From address")
	}
}

func TestRedactRemovesSecrets(t *testing.T) {
	got := Redact("535 auth failed for user with password hunter2sekrit", "hunter2sekrit", "")
	if strings.Contains(got, "hunter2sekrit") {
		t.Errorf("Redact left the secret in place: %q", got)
	}
	if !strings.Contains(got, "535 auth failed") {
		t.Errorf("Redact destroyed the diagnostic: %q", got)
	}
}
