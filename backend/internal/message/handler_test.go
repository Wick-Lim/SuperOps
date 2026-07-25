package message

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

func appError(t *testing.T, err error) *httputil.AppError {
	t.Helper()
	var appErr *httputil.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected an *httputil.AppError, got %T (%v)", err, err)
	}
	return appErr
}

func TestValidateContent(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		fileIDs  []string
		wantCode string
	}{
		{name: "text only", content: "hello"},
		{name: "attachment only", fileIDs: []string{"f1"}},
		{name: "at the length limit", content: strings.Repeat("a", maxContentRunes)},
		{
			name:     "empty with no attachment",
			wantCode: "BAD_REQUEST",
		},
		{
			name:     "one rune over the limit",
			content:  strings.Repeat("a", maxContentRunes+1),
			wantCode: "CONTENT_TOO_LONG",
		},
		{
			// char_length() counts characters, not bytes: a body of multi-byte
			// runes under the limit must not be rejected as if it were bytes.
			name:    "multi-byte runes are counted as characters",
			content: strings.Repeat("한", maxContentRunes),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateContent(tc.content, tc.fileIDs)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			appErr := appError(t, err)
			if appErr.Code != tc.wantCode {
				t.Errorf("code: got %q, want %q", appErr.Code, tc.wantCode)
			}
			if appErr.Status != http.StatusBadRequest {
				t.Errorf("status: got %d, want %d", appErr.Status, http.StatusBadRequest)
			}
		})
	}
}

func TestNormalizeContentType(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		want     string
		wantCode string
	}{
		{name: "empty defaults to markdown", in: "", want: contentTypeMarkdown},
		{name: "markdown is accepted", in: "markdown", want: contentTypeMarkdown},
		{
			// 'system' passes the column CHECK, which is exactly why it has to
			// be rejected here: it would let a user impersonate a webhook.
			name:     "system is rejected",
			in:       "system",
			wantCode: "INVALID_CONTENT_TYPE",
		},
		{name: "file is rejected", in: "file", wantCode: "INVALID_CONTENT_TYPE"},
		{
			// Anything else used to reach the DB CHECK and surface as a 500.
			name:     "unknown value is a client error, not a 500",
			in:       "text/html",
			wantCode: "INVALID_CONTENT_TYPE",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeContentType(tc.in)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != tc.want {
					t.Errorf("got %q, want %q", got, tc.want)
				}
				return
			}
			appErr := appError(t, err)
			if appErr.Code != tc.wantCode {
				t.Errorf("code: got %q, want %q", appErr.Code, tc.wantCode)
			}
			if appErr.Status != http.StatusBadRequest {
				t.Errorf("status: got %d, want %d", appErr.Status, http.StatusBadRequest)
			}
		})
	}
}

func TestValidateEmoji(t *testing.T) {
	tests := []struct {
		name     string
		emoji    string
		wantCode string
	}{
		{name: "single emoji", emoji: "👍"},
		{name: "shortcode", emoji: ":tada:"},
		{name: "at the limit", emoji: strings.Repeat("x", maxEmojiRunes)},
		{name: "empty", emoji: "", wantCode: "BAD_REQUEST"},
		{name: "over the limit", emoji: strings.Repeat("x", maxEmojiRunes+1), wantCode: "EMOJI_TOO_LONG"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateEmoji(tc.emoji)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if code := appError(t, err).Code; code != tc.wantCode {
				t.Errorf("code: got %q, want %q", code, tc.wantCode)
			}
		})
	}
}

func TestValidateFileIDs(t *testing.T) {
	tests := []struct {
		name    string
		fileIDs []string
		wantErr bool
	}{
		{name: "none"},
		{name: "uuids", fileIDs: []string{"6f1c3f0e-6b1a-4a0e-9f0a-3f6b1a4a0e9f"}},
		{name: "non-uuid would be a 500 from Postgres", fileIDs: []string{"../../etc/passwd"}, wantErr: true},
		{name: "one bad id in the list", fileIDs: []string{"6f1c3f0e-6b1a-4a0e-9f0a-3f6b1a4a0e9f", "nope"}, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFileIDs(tc.fileIDs)
			if tc.wantErr {
				if code := appError(t, err).Code; code != "INVALID_FILE_IDS" {
					t.Errorf("code: got %q, want INVALID_FILE_IDS", code)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestWriteWriteErrorMapsSentinels(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "invalid parent", err: ErrInvalidParent, wantStatus: http.StatusBadRequest, wantCode: "INVALID_PARENT"},
		{name: "unavailable files", err: ErrFilesUnavailable, wantStatus: http.StatusBadRequest, wantCode: "INVALID_FILE_IDS"},
		{name: "anything else stays internal", err: errors.New("connection reset"), wantStatus: http.StatusInternalServerError, wantCode: "INTERNAL_ERROR"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeWriteError(rec, tc.err)
			if rec.Code != tc.wantStatus {
				t.Errorf("status: got %d, want %d", rec.Code, tc.wantStatus)
			}
			if !strings.Contains(rec.Body.String(), `"code":"`+tc.wantCode+`"`) {
				t.Errorf("body %q does not carry code %q", rec.Body.String(), tc.wantCode)
			}
			// A wrapped database error must never reach the client verbatim.
			if strings.Contains(rec.Body.String(), "connection reset") {
				t.Errorf("internal error text leaked to the client: %q", rec.Body.String())
			}
		})
	}
}

func TestNextCursorRoundTrips(t *testing.T) {
	at := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	pinned := at.Add(time.Hour)
	scheduled := at.Add(2 * time.Hour)

	messages := []*Message{
		{ID: "first", CreatedAt: at.Add(-time.Hour)},
		{ID: "last", CreatedAt: at, PinnedAt: &pinned, ScheduledAt: &scheduled},
	}

	tests := []struct {
		name string
		at   func(*Message) *time.Time
		want time.Time
	}{
		{name: "timeline pages on created_at", at: createdAt, want: at},
		{name: "pins page on pinned_at", at: pinnedAt, want: pinned},
		{name: "scheduled pages on scheduled_at", at: scheduledAt, want: scheduled},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decoded, err := httputil.DecodeCursor(nextCursor(messages, tc.at))
			if err != nil {
				t.Fatalf("decode cursor: %v", err)
			}
			if !decoded.CreatedAt.Equal(tc.want) {
				t.Errorf("time: got %s, want %s", decoded.CreatedAt, tc.want)
			}
			if decoded.ID != "last" {
				t.Errorf("id: got %q, want the last row of the page", decoded.ID)
			}
		})
	}
}

func TestNextCursorEmptyCases(t *testing.T) {
	if got := nextCursor(nil, createdAt); got != "" {
		t.Errorf("empty page: got %q, want no cursor", got)
	}
	// An unpinned row cannot produce a pin cursor; it must not panic either.
	if got := nextCursor([]*Message{{ID: "m1"}}, pinnedAt); got != "" {
		t.Errorf("nil timestamp: got %q, want no cursor", got)
	}
}
