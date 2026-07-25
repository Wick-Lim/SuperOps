package httputil

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeJSONRejectsOversizedBody(t *testing.T) {
	type payload struct {
		Email string `json:"email"`
	}

	huge := strings.Repeat("a", int(MaxRequestBodyBytes)+1)
	body := `{"email":"` + huge + `"}`
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))

	var v payload
	err := DecodeJSON(r, &v)
	if err == nil {
		t.Fatal("an unbounded login body must be rejected")
	}

	var appErr *AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("error %v is not an *AppError", err)
	}
	if appErr.Status != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", appErr.Status)
	}
	if appErr.Code != "PAYLOAD_TOO_LARGE" {
		t.Errorf("code = %q", appErr.Code)
	}
}

func TestDecodeJSONAcceptsNormalBody(t *testing.T) {
	type payload struct {
		Email string `json:"email"`
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"email":"a@b.c"}`))

	var v payload
	if err := DecodeJSON(r, &v); err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	if v.Email != "a@b.c" {
		t.Errorf("email = %q", v.Email)
	}
}

func TestDecodeJSONRejectsUnknownFieldsAsBadRequest(t *testing.T) {
	type payload struct {
		Email string `json:"email"`
	}
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"email":"a","role":"owner"}`))

	var v payload
	err := DecodeJSON(r, &v)
	if err == nil {
		t.Fatal("unknown fields must be rejected")
	}
	var appErr *AppError
	if !errors.As(err, &appErr) || appErr.Status != http.StatusBadRequest {
		t.Fatalf("want a 400 AppError, got %v", err)
	}
}

func TestEnvelopeMuxErrors(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/ok", func(w http.ResponseWriter, r *http.Request) {
		JSON(w, http.StatusOK, map[string]string{"ok": "yes"})
	})

	h := EnvelopeMuxErrors(mux)

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantCode   string
	}{
		{"match passes through", http.MethodGet, "/api/v1/ok", http.StatusOK, ""},
		{"unmatched path", http.MethodGet, "/api/v1/nope", http.StatusNotFound, "NOT_FOUND"},
		{"wrong method", http.MethodPost, "/api/v1/ok", http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(tt.method, tt.path, nil))

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("content-type = %q, want application/json", ct)
			}

			var resp Response
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("body %q is not the envelope: %v", w.Body.String(), err)
			}
			if tt.wantCode == "" {
				if resp.Error != nil {
					t.Errorf("unexpected error body %+v", resp.Error)
				}
				return
			}
			if resp.Error == nil || resp.Error.Code != tt.wantCode {
				t.Errorf("error = %+v, want code %s", resp.Error, tt.wantCode)
			}
		})
	}
}

// The 405 path only exists because the mux still gets to compute it. A
// catch-all "/" registration would swallow it.
func TestEnvelopeMuxErrorsKeepsAllowHeader(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/ok", func(w http.ResponseWriter, r *http.Request) {})

	w := httptest.NewRecorder()
	EnvelopeMuxErrors(mux).ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/v1/ok", nil))

	if got := w.Header().Get("Allow"); !strings.Contains(got, "GET") {
		t.Errorf("Allow = %q, want it to mention GET", got)
	}
}

func TestLoggingMiddlewareLogsPanickingRequests(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	h := LoggingMiddleware(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))

	func() {
		defer func() { _ = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/boom", nil))
	}()

	if !strings.Contains(buf.String(), `"path":"/api/v1/boom"`) {
		t.Fatalf("a panicking request produced no access log line: %q", buf.String())
	}
}

func TestLoggingMiddlewareIncludesRequestID(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	// Composed the way app.go composes it: LoggingMiddleware wraps
	// RequestIDMiddleware, so the id is only reachable via the response header.
	h := LoggingMiddleware(log)(RequestIDMiddleware(inner))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/x", nil)
	r.Header.Set("X-Request-ID", "test-correlation-id")
	h.ServeHTTP(w, r)

	if !strings.Contains(buf.String(), `"request_id":"test-correlation-id"`) {
		t.Errorf("access log has no request_id: %q", buf.String())
	}
	if !strings.Contains(buf.String(), `"status":418`) {
		t.Errorf("access log lost the status: %q", buf.String())
	}
}

func TestRequestIDMiddlewarePropagatesToContext(t *testing.T) {
	var seen string
	h := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.Header.Set("X-Request-ID", "abc123")
	h.ServeHTTP(w, r)

	if seen != "abc123" {
		t.Errorf("context request id = %q, want abc123", seen)
	}
	if got := w.Header().Get("X-Request-ID"); got != "abc123" {
		t.Errorf("response header = %q", got)
	}
}

func TestRequestIDMiddlewareMintsWhenAbsent(t *testing.T) {
	h := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if RequestIDFromContext(r.Context()) == "" {
			t.Error("a request without X-Request-ID should still get one")
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
}
