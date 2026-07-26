package drive

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// A DELETION WITH NO REVISION IS REFUSED, NOT DEFAULTED.
//
// The revision is the JetStream dedupe key. A constant one is the collapse the
// field exists to prevent — trash and purge shared a constant once and the
// destroyed document was never unindexed, answering searches for content that
// had been erased. The code defaulted an empty revision to "deleted", which
// would have reinstated exactly that for whichever caller forgot, silently.
//
// Refusing is the better trade: cmd/reindex converges a missing unindex, and
// nothing converges a collapsed one.
func TestPublishFileDeletionsRefusesAnEmptyRevision(t *testing.T) {
	var buf bytes.Buffer
	p := &Publisher{
		// A nil NATS client is fine: the point is that publishFile is never
		// reached, and reaching it with a nil client would panic — which would
		// itself be a failure this test reports.
		logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})),
	}

	p.PublishFileDeletions(context.Background(), []FileRef{
		{ID: "f-1", WorkspaceID: "w-1", FileType: "file"}, // no Revision
	})

	if !strings.Contains(buf.String(), "no revision") {
		t.Errorf("an empty revision was accepted silently; log was %q", buf.String())
	}
}

// And a ref with no id or workspace is skipped without a word, because that is
// an empty slot rather than a caller mistake.
func TestPublishFileDeletionsSkipsAnEmptyRef(t *testing.T) {
	var buf bytes.Buffer
	p := &Publisher{logger: slog.New(slog.NewTextHandler(&buf, nil))}
	p.PublishFileDeletions(context.Background(), []FileRef{{}})
	if buf.Len() != 0 {
		t.Errorf("an empty ref logged %q", buf.String())
	}
}
