package push

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// recorder is a stand-in for Expo's push API. reply builds the response body
// for one request from the messages it received.
type recorder struct {
	mu       sync.Mutex
	requests [][]expoMessage
	headers  []http.Header

	// reply answers request n (0-based) given the decoded batch. Returning a
	// status outside 2xx sends body verbatim.
	reply func(n int, batch []expoMessage) (int, string)
}

func (r *recorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var batch []expoMessage
		if err := json.NewDecoder(req.Body).Decode(&batch); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}

		r.mu.Lock()
		n := len(r.requests)
		r.requests = append(r.requests, batch)
		r.headers = append(r.headers, req.Header.Clone())
		r.mu.Unlock()

		status, body := http.StatusOK, ""
		if r.reply != nil {
			status, body = r.reply(n, batch)
		} else {
			body = okReceipts(batch)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (r *recorder) batches() [][]expoMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]expoMessage(nil), r.requests...)
}

// okReceipts renders one "ok" ticket per message, which is what a healthy Expo
// answers.
func okReceipts(batch []expoMessage) string {
	tickets := make([]string, len(batch))
	for i := range batch {
		tickets[i] = fmt.Sprintf(`{"status":"ok","id":"ticket-%d"}`, i)
	}
	return `{"data":[` + strings.Join(tickets, ",") + `]}`
}

func senderTo(t *testing.T, srv *httptest.Server, onInvalid InvalidTokenFunc) *ExpoSender {
	t.Helper()
	return NewExpoSender(ExpoConfig{
		Endpoint:        srv.URL,
		Logger:          quietLogger(),
		OnInvalidTokens: onInvalid,
	})
}

func messages(n int) []Message {
	msgs := make([]Message, n)
	for i := range msgs {
		msgs[i] = Message{Token: fmt.Sprintf("ExponentPushToken[t%03d]", i), Title: "T", Body: "B"}
	}
	return msgs
}

// Expo rejects a request carrying more than MaxBatchSize messages outright, so
// a fan-out to a big channel must be split rather than sent whole.
func TestSendSplitsIntoBatchesOfAtMostMaxBatchSize(t *testing.T) {
	rec := &recorder{}
	srv := rec.server(t)

	if err := senderTo(t, srv, nil).Send(t.Context(), messages(2*MaxBatchSize+7)); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := rec.batches()
	if len(got) != 3 {
		t.Fatalf("made %d requests, want 3", len(got))
	}
	sizes := []int{len(got[0]), len(got[1]), len(got[2])}
	want := []int{MaxBatchSize, MaxBatchSize, 7}
	for i := range want {
		if sizes[i] != want[i] {
			t.Fatalf("batch sizes = %v, want %v", sizes, want)
		}
	}

	// Every message must appear exactly once and in order — a split that
	// duplicated or dropped one would be invisible in the sizes alone.
	seen := map[string]int{}
	for _, batch := range got {
		for _, m := range batch {
			seen[m.To]++
		}
	}
	if len(seen) != 2*MaxBatchSize+7 {
		t.Fatalf("delivered %d distinct tokens, want %d", len(seen), 2*MaxBatchSize+7)
	}
	for token, n := range seen {
		if n != 1 {
			t.Fatalf("token %s sent %d times", token, n)
		}
	}
}

func TestSendMarshalsTheExpectedPayload(t *testing.T) {
	rec := &recorder{}
	srv := rec.server(t)

	badge := 7
	sender := NewExpoSender(ExpoConfig{
		Endpoint:    srv.URL,
		AccessToken: "secret-access-token",
		Logger:      quietLogger(),
	})
	err := sender.Send(t.Context(), []Message{{
		Token: "ExponentPushToken[abc]",
		Title: "New message",
		Body:  "안녕하세요",
		Data:  map[string]string{"channel_id": "c1", "message_id": "m1"},
		Badge: &badge,
	}})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := rec.batches()
	if len(got) != 1 || len(got[0]) != 1 {
		t.Fatalf("unexpected request shape: %+v", got)
	}
	m := got[0][0]
	if m.To != "ExponentPushToken[abc]" || m.Title != "New message" || m.Body != "안녕하세요" {
		t.Fatalf("payload mismatch: %+v", m)
	}
	if m.Data["channel_id"] != "c1" || m.Data["message_id"] != "m1" {
		t.Fatalf("data not forwarded: %+v", m.Data)
	}
	if m.Badge == nil || *m.Badge != 7 {
		t.Fatalf("badge = %v, want 7", m.Badge)
	}
	// A chat message is time-sensitive and user-visible; the default priority
	// lets the OS defer it to the next maintenance window.
	if m.Priority != "high" || m.Sound != "default" {
		t.Fatalf("priority/sound = %q/%q, want high/default", m.Priority, m.Sound)
	}

	rec.mu.Lock()
	auth := rec.headers[0].Get("Authorization")
	ctype := rec.headers[0].Get("Content-Type")
	rec.mu.Unlock()
	if auth != "Bearer secret-access-token" {
		t.Fatalf("Authorization = %q", auth)
	}
	if ctype != "application/json" {
		t.Fatalf("Content-Type = %q", ctype)
	}
}

// DeviceNotRegistered is the one receipt that must have a side effect. Left in
// place, the token is re-sent and re-rejected on every notification that user
// receives, forever, and it never starts working again.
func TestSendReportsDeviceNotRegisteredTokens(t *testing.T) {
	rec := &recorder{
		reply: func(_ int, batch []expoMessage) (int, string) {
			tickets := make([]string, len(batch))
			for i, m := range batch {
				switch m.To {
				case "ExponentPushToken[dead1]", "ExponentPushToken[dead2]":
					tickets[i] = `{"status":"error","message":"not registered",` +
						`"details":{"error":"DeviceNotRegistered"}}`
				case "ExponentPushToken[toobig]":
					// A different error: real, but not a reason to delete.
					tickets[i] = `{"status":"error","message":"too big","details":{"error":"MessageTooBig"}}`
				default:
					tickets[i] = fmt.Sprintf(`{"status":"ok","id":"t%d"}`, i)
				}
			}
			return http.StatusOK, `{"data":[` + strings.Join(tickets, ",") + `]}`
		},
	}
	srv := rec.server(t)

	var dead []string
	sender := senderTo(t, srv, func(_ context.Context, tokens []string) {
		dead = append(dead, tokens...)
	})

	err := sender.Send(t.Context(), []Message{
		{Token: "ExponentPushToken[alive]"},
		{Token: "ExponentPushToken[dead1]"},
		{Token: "ExponentPushToken[toobig]"},
		{Token: "ExponentPushToken[dead2]"},
	})
	// Per-message receipts are not a batch failure: the request succeeded.
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	want := []string{"ExponentPushToken[dead1]", "ExponentPushToken[dead2]"}
	if len(dead) != len(want) {
		t.Fatalf("dead tokens = %v, want %v", dead, want)
	}
	for i := range want {
		if dead[i] != want[i] {
			t.Fatalf("dead tokens = %v, want %v", dead, want)
		}
	}
}

// Receipts are attributed to messages by position and nothing else. If the
// counts disagree the mapping is unknown, and guessing means deleting a token
// that some other message's receipt condemned.
func TestSendRefusesToAttributeMismatchedReceipts(t *testing.T) {
	rec := &recorder{
		reply: func(int, []expoMessage) (int, string) {
			return http.StatusOK, `{"data":[{"status":"error","message":"gone",` +
				`"details":{"error":"DeviceNotRegistered"}}]}`
		},
	}
	srv := rec.server(t)

	called := false
	sender := senderTo(t, srv, func(context.Context, []string) { called = true })

	err := sender.Send(t.Context(), []Message{
		{Token: "ExponentPushToken[a]"},
		{Token: "ExponentPushToken[b]"},
	})
	if err == nil {
		t.Fatal("a receipt-count mismatch must be an error")
	}
	if !strings.Contains(err.Error(), "cannot attribute") {
		t.Fatalf("error should name the problem, got %v", err)
	}
	if called {
		t.Fatal("no token may be deleted when receipts cannot be attributed")
	}
}

// Dead tokens found in one batch must still be cleaned up when a different
// batch failed outright — they are dead regardless of what happened elsewhere.
func TestSendCleansUpDeadTokensEvenWhenAnotherBatchFails(t *testing.T) {
	rec := &recorder{
		reply: func(n int, batch []expoMessage) (int, string) {
			if n == 0 {
				tickets := make([]string, len(batch))
				for i := range batch {
					tickets[i] = `{"status":"error","message":"gone",` +
						`"details":{"error":"DeviceNotRegistered"}}`
				}
				return http.StatusOK, `{"data":[` + strings.Join(tickets, ",") + `]}`
			}
			return http.StatusTooManyRequests, `{"errors":[{"code":"RATE_LIMITED","message":"slow down"}]}`
		},
	}
	srv := rec.server(t)

	var dead []string
	sender := senderTo(t, srv, func(_ context.Context, tokens []string) { dead = append(dead, tokens...) })

	err := sender.Send(t.Context(), messages(MaxBatchSize+5))
	if err == nil {
		t.Fatal("a rejected batch must surface as an error")
	}
	if len(dead) != MaxBatchSize {
		t.Fatalf("cleaned up %d tokens, want %d", len(dead), MaxBatchSize)
	}
}

func TestSendSurfacesTopLevelRejections(t *testing.T) {
	rec := &recorder{
		reply: func(int, []expoMessage) (int, string) {
			return http.StatusBadRequest,
				`{"errors":[{"code":"PUSH_TOO_MANY_EXPERIENCE_IDS","message":"too many projects"}]}`
		},
	}
	srv := rec.server(t)

	err := senderTo(t, srv, nil).Send(t.Context(), messages(1))
	if err == nil {
		t.Fatal("HTTP 400 must be an error")
	}
	if !strings.Contains(err.Error(), "PUSH_TOO_MANY_EXPERIENCE_IDS") {
		t.Fatalf("error should carry the service's own code, got %v", err)
	}
}

// A proxy 502 answers HTML. Reporting a JSON syntax error there says nothing
// useful about what went wrong.
func TestSendSurfacesNonJSONFailures(t *testing.T) {
	rec := &recorder{
		reply: func(int, []expoMessage) (int, string) {
			return http.StatusBadGateway, "<html>502 Bad Gateway</html>"
		},
	}
	srv := rec.server(t)

	err := senderTo(t, srv, nil).Send(t.Context(), messages(1))
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("want an error naming HTTP 502, got %v", err)
	}
}

func TestSendOfNothingDoesNothing(t *testing.T) {
	rec := &recorder{}
	srv := rec.server(t)
	if err := senderTo(t, srv, nil).Send(t.Context(), nil); err != nil {
		t.Fatalf("Send(nil): %v", err)
	}
	if n := len(rec.batches()); n != 0 {
		t.Fatalf("made %d requests for an empty batch", n)
	}
}
