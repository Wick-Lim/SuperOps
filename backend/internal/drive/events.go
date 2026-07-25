package drive

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
)

// Drive's outbound events.
//
// The subject is `superops.{workspace}.file.{action}` — the SAME subject space
// the chat-attachment path uses and the same one search.Indexer.HandleFile
// already decodes. A Drive file and a chat upload are one row in one table with
// one index document, so giving them two subjects would mean two consumers that
// have to agree about which one owns a file that is both.
//
// Publishing is BEST EFFORT and always post-commit. The row is the source of
// truth; the index is derived. An upload that succeeded must not fail because
// NATS blinked, and cmd/reindex exists precisely so a missed event is a
// recoverable state rather than a lost one.

// publishTimeout bounds the JetStream storage ack. Short: the row is already
// committed and the response is waiting.
const publishTimeout = 3 * time.Second

const (
	ActionUploaded = "file.uploaded"
	ActionUpdated  = "file.updated"
	ActionDeleted  = "file.deleted"

	// ActionThumbnailRequested asks the thumbnailer for a preview. Its own
	// action rather than a second consumer on file.uploaded, because the
	// thumbnailer needs the storage key and the content type — which the search
	// indexer has no business knowing — and because a preview that fails must
	// not affect indexing.
	ActionThumbnailRequested = "thumbnail.requested"
)

// Publisher emits Drive events. A nil *Publisher is a working no-op, so a
// deployment without NATS — or a test that does not care — needs no branch at
// the call site.
type Publisher struct {
	nats   *natspkg.Client
	logger *slog.Logger
}

func NewPublisher(nc *natspkg.Client, logger *slog.Logger) *Publisher {
	if nc == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Publisher{nats: nc, logger: logger}
}

// fileEvent is the payload search.FileEvent decodes. The field names are that
// struct's json tags and changing one here without changing it there is a
// silently empty index document rather than an error.
type fileEvent struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id,omitempty"`
	FolderID  string `json:"folder_id,omitempty"`
	UserID    string `json:"user_id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	IsDeleted bool   `json:"is_deleted,omitempty"`
}

// PublishFile emits one file event.
//
// It never returns an error to the caller's request path. The alternative —
// failing a completed upload because a message bus was unreachable — destroys
// work that is already committed in order to keep a derived index fresh, and
// the index has a rebuild path.
func (p *Publisher) PublishFile(ctx context.Context, action string, f *File) {
	if p == nil || f == nil {
		return
	}

	payload := fileEvent{
		ID:        f.ID,
		UserID:    f.CreatedBy,
		Name:      f.Name,
		CreatedAt: f.CreatedAt.UTC().Format(time.RFC3339Nano),
		IsDeleted: action == ActionDeleted,
	}
	if f.FolderID != nil {
		payload.FolderID = *f.FolderID
	}

	subject := fmt.Sprintf("superops.%s.%s", f.WorkspaceID, action)

	// JetStream, not core NATS. The indexer is a DURABLE consumer: it binds to
	// the stream, so a core publish — at-most-once, no persistence — would be
	// dropped outright while the worker was restarting, and there is no
	// reconciliation pass that would ever notice. That is the same reasoning
	// cmd/worker's header gives for every other consumer in the product.
	//
	// The message id is (action, file, updated_at), so a retry of the same
	// logical event collapses in the stream's duplicate window rather than
	// re-indexing.
	msgID := fmt.Sprintf("%s:%s:%d", action, f.ID, f.UpdatedAt.UTC().UnixNano())

	// Deliberately not the request context: the response is about to be written
	// and cancelling the publish with it would drop the event for a client that
	// hung up a millisecond early.
	pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout)
	defer cancel()

	if err := p.nats.PublishDurable(pubCtx, subject, msgID, natspkg.Event{
		Type: action,
		Data: payload,
	}); err != nil {
		// Warn, not Error: the write is committed and cmd/reindex converges the
		// index. Failing the request would destroy work to keep a derived index
		// fresh; paging somebody would be about a stale search result.
		p.logger.Warn("publish drive file event; the row is committed and the index will "+
			"converge on the next reindex", "subject", subject, "file_id", f.ID, "error", err)
	}
}

// thumbnailRequest is what internal/thumb's consumer decodes.
type thumbnailRequest struct {
	FileID      string `json:"file_id"`
	StorageKey  string `json:"storage_key"`
	ContentType string `json:"content_type"`
}

// RequestThumbnail asks for a preview of a file's current bytes.
//
// Called after every write that changes them — upload, and each new version —
// and never for a collab type, which has none. Like every publish here it is
// post-commit and best effort: a missing preview is a missing preview.
func (p *Publisher) RequestThumbnail(ctx context.Context, f *File) {
	if p == nil || f == nil || f.storageKey == "" {
		return
	}
	subject := fmt.Sprintf("superops.%s.%s", f.WorkspaceID, ActionThumbnailRequested)

	pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout)
	defer cancel()

	// The message id is (file, storage key): a retry collapses, and a NEW
	// version — which has a new key — is a different request rather than a
	// duplicate of the old one.
	msgID := "thumb:" + f.ID + ":" + f.storageKey

	if err := p.nats.PublishDurable(pubCtx, subject, msgID, natspkg.Event{
		Type: ActionThumbnailRequested,
		Data: thumbnailRequest{
			FileID:      f.ID,
			StorageKey:  f.storageKey,
			ContentType: f.ContentType,
		},
	}); err != nil {
		p.logger.Warn("request thumbnail; the file is stored and simply has no preview",
			"subject", subject, "file_id", f.ID, "error", err)
	}
}
