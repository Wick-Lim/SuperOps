package httputil

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestJSON(t *testing.T) {
	w := httptest.NewRecorder()
	JSON(w, http.StatusOK, map[string]string{"hello": "world"})

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json, got %s", ct)
	}

	var resp Response
	json.NewDecoder(w.Body).Decode(&resp)
	data, ok := resp.Data.(map[string]interface{})
	if !ok {
		t.Fatal("data should be a map")
	}
	if data["hello"] != "world" {
		t.Errorf("expected world, got %v", data["hello"])
	}
}

func TestJSONError(t *testing.T) {
	w := httptest.NewRecorder()
	JSONError(w, http.StatusNotFound, "NOT_FOUND", "resource not found")

	if w.Code != 404 {
		t.Errorf("expected 404, got %d", w.Code)
	}

	var resp Response
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Error == nil {
		t.Fatal("error should not be nil")
	}
	if resp.Error.Code != "NOT_FOUND" {
		t.Errorf("expected NOT_FOUND, got %s", resp.Error.Code)
	}
}

func TestEncodeCursor(t *testing.T) {
	cursor := EncodeCursor(mustParseTime("2026-01-15T10:30:00Z"))
	if cursor == "" {
		t.Fatal("cursor should not be empty")
	}
}

func mustParseTime(s string) (t time.Time) {
	t, _ = time.Parse(time.RFC3339, s)
	return
}
