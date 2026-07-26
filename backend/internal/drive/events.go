package drive

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/Wick-Lim/SuperOps/backend/internal/file"
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
	FileType  string `json:"file_type"`
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
	p.publishFile(ctx, action, f, strconv.FormatInt(f.UpdatedAt.UTC().UnixNano(), 10))
}

// FileRef is the minimum an unindex needs: the id, and the type the document id
// was built from.
type FileRef struct {
	ID          string
	FileType    string
	WorkspaceID string

	// Revision distinguishes THIS removal from any other removal of the same
	// file, and every caller must supply one that actually varies.
	//
	// A constant here is a bug, and it was one: trashing a folder and then
	// purging it both published file.deleted for the same id, so the purge
	// collapsed into the trash inside the stream's duplicate window and the
	// destroyed document was never unindexed. Trash uses its trashed_at;
	// purge uses "purge", which is terminal because the row is gone.
	Revision string
}

// PublishFileDeletions unindexes files removed in bulk.
//
// TRASHING A FOLDER, RESTORING ONE AND EMPTYING THE TRASH ALL WENT UNNOTICED BY
// THE INDEX. Only the single-file routes published, so deleting a folder left
// every document inside it fully searchable, and emptying the trash left them
// searchable FOREVER — the rows were gone, so no reindex could ever remove
// them and no repair path existed. A user deleted a folder of documents, the
// files 404'd, and search still returned their titles and bodies.
//
// Per file rather than one subtree event: the indexer's unit is a document, and
// a "folder was trashed" event would have it re-derive the subtree from a
// database that has already moved on. The volume is bounded by the folder's
// contents and each message is three fields.
func (p *Publisher) PublishFileDeletions(ctx context.Context, files []FileRef) {
	if p == nil {
		return
	}
	for _, f := range files {
		if f.ID == "" || f.WorkspaceID == "" {
			continue
		}
		if f.Revision == "" {
			// LOUD, not defaulted. A constant revision is the collapse this
			// field exists to prevent — trash and purge shared one once and the
			// destroyed document was never unindexed. Substituting "deleted"
			// here would reinstate exactly that, silently, for whichever caller
			// forgot. Refusing to publish is worse for one event and better for
			// the next person: cmd/reindex converges a missing unindex, and
			// nothing converges a collapsed one.
			p.logger.Error("refusing to publish a deletion with no revision; "+
				"the caller must supply one that distinguishes this removal",
				"file_id", f.ID)
			continue
		}
		p.publishFile(ctx, ActionDeleted, &File{
			ID: f.ID, WorkspaceID: f.WorkspaceID, FileType: f.FileType,
		}, f.Revision)
	}
}

// PublishChatFile emits an event for a CHAT attachment, whose route lives in
// internal/file and had no publisher of its own.
//
// It is on this type because this is where the JetStream client is, and the
// dependency only runs one way: internal/drive imports internal/file, so the
// payload type is defined there and this satisfies file.Events.
func (p *Publisher) PublishChatFile(ctx context.Context, e file.ChatFileEvent) {
	if p == nil || e.ID == "" || e.WorkspaceID == "" {
		return
	}
	action := ActionUploaded
	revision := strconv.FormatInt(e.CreatedAt.UTC().UnixNano(), 10)
	if e.Deleted {
		action = ActionDeleted
		// Terminal: the row is gone, so nothing can collide with it later.
		revision = "delete"
	}

	subject := fmt.Sprintf("superops.%s.%s", e.WorkspaceID, action)
	msgID := fmt.Sprintf("%s:%s:%s", action, e.ID, revision)

	pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout)
	defer cancel()

	if err := p.nats.PublishDurable(pubCtx, subject, msgID, natspkg.Event{
		Type: action,
		Data: fileEvent{
			ID: e.ID, UserID: e.UserID, Name: e.Name,
			ChannelID: e.ChannelID,
			// A chat attachment is a plain file; it has no editor type.
			FileType:  "file",
			CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339Nano),
			IsDeleted: e.Deleted,
		},
	}); err != nil {
		p.logger.Warn("publish chat file event", "id", e.ID, "action", action, "error", err)
	}
}

// PublishFileProjection emits the re-index for a document whose PROJECTION
// changed.
//
// It exists because the ordinary message id could not describe this event.
// That id is (action, file, files.updated_at), and PutProjection writes only
// file_projections — the files row is untouched, so updated_at does not move.
// Every projection landing inside the stream's duplicate window therefore
// collapsed onto the FIRST one's id and was discarded before any consumer saw
// it. The window is two minutes; the client's projection debounce is two
// seconds. A document was searchable by the text of its first save and by
// nothing typed afterwards, and no repair path could see it: the database row
// was current, so the staleness existed only between the database and the
// index, where nothing looks.
//
// The projection seq IS the identity of the logical event, so it is what the
// id is built from. Retries still collapse; distinct projections no longer do.
func (p *Publisher) PublishFileProjection(ctx context.Context, f *File, projectionSeq int64) {
	p.publishFile(ctx, ActionUpdated, f, "projection-"+strconv.FormatInt(projectionSeq, 10))
}

func (p *Publisher) publishFile(ctx context.Context, action string, f *File, revision string) {
	if p == nil || f == nil {
		return
	}

	payload := fileEvent{
		ID: f.ID,
		// One payload serves uploaded/updated/deleted, so setting this here
		// covers all three at once — which is exactly what the delete arm needs
		// in order to compute the same document id the index arm did.
		FileType:  f.FileType,
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
	// The message id is (action, file, revision), so a retry of the same
	// logical event collapses in the stream's duplicate window rather than
	// re-indexing. The revision is the caller's, because only the caller knows
	// what made this event distinct — see PublishFileProjection for the case
	// that proved updated_at is not always it.
	msgID := fmt.Sprintf("%s:%s:%s", action, f.ID, revision)

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
