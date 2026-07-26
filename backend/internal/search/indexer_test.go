package search

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
)

func testIndexer() *Indexer {
	// service is nil on purpose: every case below must be decided from the
	// payload alone, before anything reaches Meilisearch. A test that panics on a
	// nil service is a test that found the handler talking to the index when it
	// should have rejected the event.
	return NewIndexer(nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func msg(subject, data string) *nats.Msg {
	return &nats.Msg{Subject: subject, Data: []byte(data)}
}

func isPermanent(err error) bool {
	var p *PermanentError
	return errors.As(err, &p)
}

// The whole point of the error return: a bad event must be terminated, not
// redelivered five times and then dropped anyway.
func TestHandleMessagePermanentFailures(t *testing.T) {
	const subject = "superops." + wsA + ".message.created"

	tests := []struct {
		name    string
		subject string
		data    string
	}{
		{"envelope is not json", subject, `{`},
		{"payload is not json", subject, `{"type":"message.new","data":"nope"}`},
		{"no message id", subject, `{"type":"message.new","data":{"channel_id":"` + chA + `"}}`},
		{
			"subject has no workspace id",
			"superops",
			`{"type":"message.new","data":{"id":"` + chB + `","created_at":"2026-07-25T10:00:00Z"}}`,
		},
		{
			"workspace id is not a uuid",
			"superops.not-a-uuid.message.created",
			`{"type":"message.new","data":{"id":"` + chB + `","created_at":"2026-07-25T10:00:00Z"}}`,
		},
		{
			"created_at is unusable",
			subject,
			`{"type":"message.new","data":{"id":"` + chB + `","created_at":"yesterday"}}`,
		},
		{
			"created_at is missing",
			subject,
			`{"type":"message.new","data":{"id":"` + chB + `"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := testIndexer().HandleMessage(context.Background(), msg(tt.subject, tt.data))
			if err == nil {
				t.Fatal("expected an error, got nil (the event would have been acked and lost)")
			}
			if !isPermanent(err) {
				t.Fatalf("error %v is retryable; a permanently bad event must be terminated", err)
			}
		})
	}
}

// Events the indexer has no opinion about are a clean ack, not a failure: the
// consumer's filter is superops.*.message.* and carries more than these three.
func TestHandleMessageIgnoresOtherEventTypes(t *testing.T) {
	for _, typ := range []string{"message.pinned", "reaction.added", "file.uploaded", ""} {
		body := `{"type":"` + typ + `","data":{}}`
		if err := testIndexer().HandleMessage(context.Background(), msg("superops."+wsA+".message.created", body)); err != nil {
			t.Fatalf("type %q: %v", typ, err)
		}
	}
}

// A message with no channel has no access key, so it could never be found
// again. The old code indexed it anyway; now the event is terminated.
func TestHandleMessageWithoutAChannelIsPermanent(t *testing.T) {
	body := `{"type":"message.new","data":{"id":"` + chB + `","created_at":"2026-07-25T10:00:00Z"}}`
	err := testIndexer().HandleMessage(context.Background(), msg("superops."+wsA+".message.created", body))
	if err == nil || !isPermanent(err) {
		t.Fatalf("err = %v, want a permanent error", err)
	}
}

// The file handler is the same contract as the message one; the cases that
// decide the ack must be decided from the payload, before Meilisearch.
func TestHandleFilePermanentFailures(t *testing.T) {
	const subject = "superops." + wsA + ".file.uploaded"

	tests := []struct {
		name    string
		subject string
		data    string
	}{
		{"envelope is not json", subject, `{`},
		{"payload is not json", subject, `{"type":"file.uploaded","data":42}`},
		{"no file id", subject, `{"type":"file.uploaded","data":{"name":"x.pdf"}}`},
		{
			"subject has no workspace id",
			"superops",
			`{"type":"file.uploaded","data":{"id":"` + fil + `","user_id":"` + usr + `","created_at":"2026-07-25T10:00:00Z"}}`,
		},
		{
			"created_at is unusable",
			subject,
			`{"type":"file.uploaded","data":{"id":"` + fil + `","user_id":"` + usr + `","created_at":"yesterday"}}`,
		},
		{
			// No channel and no uploader means no access key at all.
			"neither a channel nor an uploader",
			subject,
			`{"type":"file.uploaded","data":{"id":"` + fil + `","created_at":"2026-07-25T10:00:00Z"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := testIndexer().HandleFile(context.Background(), msg(tt.subject, tt.data))
			if err == nil {
				t.Fatal("expected an error, got nil (the event would have been acked and lost)")
			}
			if !isPermanent(err) {
				t.Fatalf("error %v is retryable; a permanently bad event must be terminated", err)
			}
		})
	}
}

func TestHandleFileIgnoresOtherEventTypes(t *testing.T) {
	for _, typ := range []string{"file.downloaded", "message.new", ""} {
		body := `{"type":"` + typ + `","data":{}}`
		if err := testIndexer().HandleFile(context.Background(), msg("superops."+wsA+".file.uploaded", body)); err != nil {
			t.Fatalf("type %q: %v", typ, err)
		}
	}
}

func TestPermanentErrorUnwraps(t *testing.T) {
	inner := errors.New("boom")
	err := error(&PermanentError{Reason: "bad payload", Err: inner})
	if !errors.Is(err, inner) {
		t.Fatal("PermanentError must unwrap to its cause")
	}
	if got, want := err.Error(), "bad payload: boom"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if got := (&PermanentError{Reason: "bad payload"}).Error(); got != "bad payload" {
		t.Fatalf("Error() = %q, want %q", got, "bad payload")
	}
}

func TestSettingsComparison(t *testing.T) {
	// Ordering is part of the ranking rules for searchable attributes, so it is
	// compared as a sequence; the other two are sets.
	if sameSequence([]string{"a", "b"}, []string{"b", "a"}) {
		t.Error("sameSequence must respect order")
	}
	if !sameSet([]string{"a", "b"}, []string{"b", "a"}) {
		t.Error("sameSet must ignore order")
	}
	if sameSet([]string{"a"}, []string{"a", "b"}) {
		t.Error("sameSet must compare lengths")
	}
	// The default Meilisearch answer for an untouched index; it must not compare
	// equal to what this build wants, or the settings would never be applied.
	if sameSequence([]string{"*"}, wantSearchable) {
		t.Error("the default searchable attributes must trigger an update")
	}
	if got := attributeNames(&[]interface{}{"channel_id", 7}); !sameSequence(got, []string{"channel_id", "7"}) {
		t.Errorf("attributeNames = %v", got)
	}
	if attributeNames(nil) != nil || deref(nil) != nil {
		t.Error("nil settings must flatten to nil")
	}
}

// The filter that enforces object visibility can only be evaluated if every
// attribute it names is filterable — Meilisearch 400s on the rest, which would
// take search down rather than widen it, but down is not much better.
func TestFilterableAttributesCoverTheAuthorizationFilter(t *testing.T) {
	filter, ok := buildFilter(Query{
		WorkspaceID: wsA,
		Keys:        keysFor(chA),
		Types:       []ObjectType{TypeMessage},
		ChannelID:   chA,
		FromUserID:  usr,
	})
	if !ok {
		t.Fatal("expected a filter")
	}
	for _, attr := range []string{"acl", "type", "workspace_id", "channel_id", "user_id", "is_deleted"} {
		if !contains(wantFilterable, attr) {
			t.Errorf("filter %q uses %s, which is not in wantFilterable", filter, attr)
		}
	}
	if !contains(wantSortable, "created_at") {
		t.Error("search sorts on created_at, which must be sortable")
	}
	// Titles are searchable and ranked ahead of bodies; a file has nothing else.
	if !sameSequence(wantSearchable, []string{"title", "content"}) {
		t.Errorf("wantSearchable = %v", wantSearchable)
	}
}

// THE CHANNEL SEAM MUST NOT FAIL SILENTLY.
//
// NewIndexer picks up ChannelSource with a type assertion on the BodySource. If
// that assertion quietly fails, every chat attachment indexes with an empty
// channel_id and `?channel=` matches nothing — the exact defect the seam was
// added to fix, returning through the back door.
func TestNewIndexerSaysSoWhenItCannotResolveAChannel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	NewIndexer(nil, nil, bodyOnlySource{}, logger)
	if !strings.Contains(buf.String(), "cannot resolve a file's channel") {
		t.Errorf("a body source that is not a ChannelSource was accepted silently; log was %q", buf.String())
	}

	// And one that CAN answer says nothing.
	buf.Reset()
	NewIndexer(nil, nil, bodyAndChannelSource{}, logger)
	if strings.Contains(buf.String(), "cannot resolve") {
		t.Errorf("a capable body source was warned about anyway: %q", buf.String())
	}
}

type bodyOnlySource struct{}

func (bodyOnlySource) BodyForFile(context.Context, string) (string, error) { return "", nil }

type bodyAndChannelSource struct{ bodyOnlySource }

func (bodyAndChannelSource) ChannelForFile(context.Context, string) (string, error) {
	return "", nil
}
