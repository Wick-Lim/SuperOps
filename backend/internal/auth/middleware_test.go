package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExtractTokenQueryParamAllowlist(t *testing.T) {
	tests := []struct {
		name   string
		method string
		target string
		header string
		want   string
	}{
		{"bearer header on a normal route", http.MethodGet, "/api/v1/messages", "Bearer abc", "abc"},
		{"lowercase bearer scheme", http.MethodGet, "/api/v1/messages", "bearer abc", "abc"},
		{"header wins over query", http.MethodGet, "/api/v1/ws?token=query", "Bearer header", "header"},

		// Allowed: neither client can set a request header.
		{"websocket handshake", http.MethodGet, "/api/v1/ws?token=abc", "", "abc"},
		{"websocket handshake trailing slash", http.MethodGet, "/api/v1/ws/?token=abc", "", "abc"},
		{"file download", http.MethodGet, "/api/v1/files/1234?token=abc", "", "abc"},

		// Refused: a token in the URL leaks into logs, history and Referer.
		{"arbitrary api route", http.MethodGet, "/api/v1/messages?token=abc", "", ""},
		{"admin route", http.MethodGet, "/api/v1/admin/users?token=abc", "", ""},
		{"file upload", http.MethodPost, "/api/v1/files/upload?token=abc", "", ""},
		{"file delete", http.MethodDelete, "/api/v1/files/1234?token=abc", "", ""},
		{"file sub-resource", http.MethodGet, "/api/v1/files/1234/versions?token=abc", "", ""},
		{"ws prefix impostor", http.MethodGet, "/api/v1/wsx?token=abc", "", ""},
		{"no token at all", http.MethodGet, "/api/v1/messages", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, tt.target, nil)
			if tt.header != "" {
				r.Header.Set("Authorization", tt.header)
			}
			if got := extractToken(r); got != tt.want {
				t.Errorf("extractToken() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBearerToken(t *testing.T) {
	tests := []struct {
		header string
		want   string
	}{
		{"", ""},
		{"Bearer", ""},
		{"Bearer ", ""},
		{"Basic abc", ""},
		{"Bearer abc.def.ghi", "abc.def.ghi"},
		{"BEARER abc", "abc"},
	}
	for _, tt := range tests {
		t.Run(tt.header, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/messages", nil)
			if tt.header != "" {
				r.Header.Set("Authorization", tt.header)
			}
			if got := bearerToken(r); got != tt.want {
				t.Errorf("bearerToken(%q) = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}
