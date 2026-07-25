package mail

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type capturedRequest struct {
	authorization  string
	idempotencyKey string
	contentType    string
	body           resendRequest
}

// resendStub answers with a fixed status and body, and records what it was sent.
func resendStub(t *testing.T, status int, response string) (*httptest.Server, func() capturedRequest) {
	t.Helper()

	var (
		mu  sync.Mutex
		got capturedRequest
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)

		mu.Lock()
		got.authorization = r.Header.Get("Authorization")
		got.idempotencyKey = r.Header.Get("Idempotency-Key")
		got.contentType = r.Header.Get("Content-Type")
		_ = json.Unmarshal(raw, &got.body)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(srv.Close)

	return srv, func() capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		return got
	}
}

func newTestResendSender(t *testing.T, endpoint string) *ResendSender {
	t.Helper()
	sender, err := NewResendSender(ResendConfig{
		APIKey:   "re_test_key",
		Endpoint: endpoint,
		From:     testFrom,
		Logger:   discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewResendSender: %v", err)
	}
	return sender
}

func TestResendPostsTheMessageWithItsIdempotencyKey(t *testing.T) {
	srv, captured := resendStub(t, http.StatusOK, `{"id":"a1b2c3"}`)
	sender := newTestResendSender(t, srv.URL)

	msg := testMessage()
	msg.IdempotencyKey = "invitation:42"

	if err := sender.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sender.Name() != TransportResend {
		t.Errorf("Name() = %q", sender.Name())
	}

	got := captured()
	if got.authorization != "Bearer re_test_key" {
		t.Errorf("Authorization = %q", got.authorization)
	}
	// Without this, a redelivery after a lost response sends the invitation twice.
	if got.idempotencyKey != "invitation:42" {
		t.Errorf("Idempotency-Key = %q, want the message's key", got.idempotencyKey)
	}
	if got.contentType != "application/json" {
		t.Errorf("Content-Type = %q", got.contentType)
	}
	if got.body.From != `SuperOps <no-reply@superops.example>` {
		t.Errorf("from = %q", got.body.From)
	}
	if len(got.body.To) != 1 || got.body.To[0] != `Dana <dana@example.org>` {
		t.Errorf("to = %v", got.body.To)
	}
	if got.body.Subject != "You have been invited" {
		t.Errorf("subject = %q", got.body.Subject)
	}
	// Both bodies travel: HTML-only scores worse with spam filters.
	if got.body.Text == "" || got.body.HTML == "" {
		t.Errorf("text = %q, html = %q; both must be sent", got.body.Text, got.body.HTML)
	}
}

func TestResendOmitsTheIdempotencyHeaderWhenThereIsNoKey(t *testing.T) {
	srv, captured := resendStub(t, http.StatusOK, `{"id":"a1b2c3"}`)
	sender := newTestResendSender(t, srv.URL)

	if err := sender.Send(context.Background(), testMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if key := captured().idempotencyKey; key != "" {
		t.Errorf("Idempotency-Key = %q, want it absent", key)
	}
}

func TestResendErrorMapping(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		permanent bool
		wantIn    string
	}{
		{"bad api key", http.StatusUnauthorized, `{"message":"API key is invalid"}`, true, "MAIL_RESEND_API_KEY"},
		{"forbidden", http.StatusForbidden, `{"message":"domain not verified"}`, true, "domain not verified"},
		{"validation failure", http.StatusUnprocessableEntity, `{"message":"Invalid to field"}`, true, "Invalid to field"},
		{"bad request", http.StatusBadRequest, `{"message":"missing subject"}`, true, "missing subject"},
		{"rate limited", http.StatusTooManyRequests, `{"message":"slow down"}`, false, "slow down"},
		{"provider outage", http.StatusBadGateway, `<html>502</html>`, false, "502"},
		{"provider error", http.StatusInternalServerError, `{"message":"internal"}`, false, "internal"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := resendStub(t, tc.status, tc.body)
			sender := newTestResendSender(t, srv.URL)

			err := sender.Send(context.Background(), testMessage())
			if err == nil {
				t.Fatalf("Send succeeded against HTTP %d", tc.status)
			}
			if IsPermanent(err) != tc.permanent {
				t.Fatalf("HTTP %d: IsPermanent = %v, want %v (%v)", tc.status, IsPermanent(err), tc.permanent, err)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not mention %q", err, tc.wantIn)
			}
		})
	}
}

func TestResendTreatsAProviderOutageAsTransient(t *testing.T) {
	// A server that is simply not there: connection refused, not a status code.
	srv, _ := resendStub(t, http.StatusOK, `{}`)
	endpoint := srv.URL
	srv.Close()

	sender := newTestResendSender(t, endpoint)
	err := sender.Send(context.Background(), testMessage())
	if err == nil {
		t.Fatal("Send succeeded against a closed server")
	}
	if IsPermanent(err) {
		t.Errorf("a connection failure mapped to permanent (%v); an outage would drop the mail", err)
	}
}

func TestResendConfigValidation(t *testing.T) {
	if _, err := NewResendSender(ResendConfig{From: testFrom, Logger: discardLogger()}); err == nil {
		t.Error("NewResendSender accepted an empty API key")
	} else if !strings.Contains(err.Error(), "MAIL_RESEND_API_KEY") {
		t.Errorf("error %q does not name the missing setting", err)
	}

	if _, err := NewResendSender(ResendConfig{
		APIKey: "k", Endpoint: "not-a-url", From: testFrom, Logger: discardLogger(),
	}); err == nil {
		t.Error("NewResendSender accepted a relative endpoint")
	}
}

func TestResendRejectsAnUnsendableMessage(t *testing.T) {
	srv, _ := resendStub(t, http.StatusOK, `{"id":"x"}`)
	sender := newTestResendSender(t, srv.URL)

	err := sender.Send(context.Background(), &Message{Subject: "no recipients", Text: "x"})
	if err == nil {
		t.Fatal("Send accepted a message with no recipients")
	}
	if !IsPermanent(err) {
		t.Errorf("a malformed message mapped to transient (%v)", err)
	}
}
