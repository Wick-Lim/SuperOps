package httputil

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A share-link token is a BEARER CREDENTIAL FOR CONTENT and it travels in the
// request path. LoggingMiddleware writes r.URL.Path at INFO on every request,
// so without redaction every resolve attempt puts a working token into the log
// — where it outlives the link, is shipped to whatever aggregator the operator
// runs, and is readable by everyone with access to it.
func TestLoggingMiddlewareRedactsSecretPaths(t *testing.T) {
	const token = "s3cr3t-token-value-that-must-not-be-logged"

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	handler := LoggingMiddleware(logger)(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/drive/links/"+token+"/resolve", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	if out == "" {
		t.Fatal("the middleware logged nothing, so this test proves nothing")
	}
	if strings.Contains(out, token) {
		t.Fatalf("the token appears in the log line:\n%s", out)
	}
	if !strings.Contains(out, "<redacted>") {
		t.Fatalf("the path was not redacted:\n%s", out)
	}
	// The route SHAPE survives, or the log stops being useful for finding which
	// endpoint was hit.
	if !strings.Contains(out, "/api/v1/drive/links/") || !strings.Contains(out, "/resolve") {
		t.Errorf("the redaction destroyed the route shape:\n%s", out)
	}
}

// Everything else is logged verbatim. A blanket redaction would be a log nobody
// can debug from.
func TestRedactPathLeavesOrdinaryPathsAlone(t *testing.T) {
	for _, path := range []string{
		"/api/v1/drive/files/7c9e6679-7425-40de-944b-e07fc1f90ae7",
		"/api/v1/workspaces/abc/drive/folders",
		"/health",
		"",
	} {
		if got := redactPath(path); got != path {
			t.Errorf("redactPath(%q) = %q, want it unchanged", path, got)
		}
	}
}

func TestRedactPathHandlesATrailingToken(t *testing.T) {
	// No trailing segment: the whole remainder is the secret.
	if got := redactPath("/api/v1/drive/links/abc123"); strings.Contains(got, "abc123") {
		t.Errorf("redactPath left the token in %q", got)
	}
}
