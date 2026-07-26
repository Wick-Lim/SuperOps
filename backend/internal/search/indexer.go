package search

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

// MessageEvent is the payload of superops.{workspace_id}.message.{action}.
type MessageEvent struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	UserID    string `json:"user_id"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
	IsDeleted bool   `json:"is_deleted"`
}

// FileEvent is the payload of superops.{workspace_id}.file.{action}.
//
// ChannelID is the channel of the message the file is attached to and is empty
// while the upload is unattached — it is what decides the file's access keys,
// so a publisher that omits it makes the file visible to its uploader only.
// A file that is attached to a message after upload must be republished (as
// file.updated) with the channel filled in, or it stays uploader-only in the
// index.
//
// FileType is the Drive registry kind — "document", "spreadsheet", "design", or
// "file" for a plain upload. It decides the search type and therefore the
// document id, so a publisher that omits it indexes an editor object as a plain
// file. That is the safe direction (it stays findable, under the wrong facet)
// and it is why FileObjectType defaults to TypeFile rather than erroring.
type FileEvent struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	FolderID  string `json:"folder_id"`
	FileType  string `json:"file_type"`
	UserID    string `json:"user_id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	IsDeleted bool   `json:"is_deleted"`
}

// ACLSource reads an object's materialized access keys.
//
// The indexer asks for them rather than deriving them, because the derivation
// cannot express Drive: a file is readable through its folder, through the
// workspace grant on the Drive root, through a per-object grant, or through a
// channel it was shared into. Reading acl_key means the index and the database
// answer the same question — and a search filter that disagrees with the
// database is not a narrowing, it is a leak or a blank page.
//
// nil is allowed: the indexer then falls back to FileDoc's own derivation, which
// is exactly file.Handler.canRead. A deployment with no ACL source indexes
// message attachments correctly and Drive files narrowly, which is the safe end.
type ACLSource interface {
	KeysForObject(ctx context.Context, objectType, objectID string) ([]string, error)
}

// BodySource reads an object's client-published body projection.
//
// It exists for the same reason ACLSource does: the fact belongs to the
// database, not to the event. A document body is up to 1 MiB and the file event
// fires on every rename and move, so carrying it on a JetStream message would
// put a megabyte on the wire to record that somebody renamed a file.
//
// nil is allowed and indexes titles only, which is the safe end. A read error
// is RETURNED, not swallowed — same rule as the ACL read: indexing a document
// with content that does not match it is worse than not indexing it.
type BodySource interface {
	BodyForFile(ctx context.Context, fileID string) (string, error)
}

// ChannelSource reads the channel a file hangs off, through the message it is
// attached to.
//
// SAME REASON AGAIN, and this one was a live defect. `?channel=` returned no
// files, ever: the fact lives in files.message_id -> messages.channel_id, only
// the chat-attachment path knows it, and that path did not set it on the event.
// cmd/reindex read it from the database and the live indexer did not, so the
// same file's channel scoping depended on which of the two had last touched it.
//
// Reading it here fixes it once, for every publisher, present and future — the
// alternative was teaching each one a join it has no other reason to know.
//
// nil is allowed and leaves channel_id empty, which is the narrow end: the
// file is still findable, just not by channel.
type ChannelSource interface {
	ChannelForFile(ctx context.Context, fileID string) (string, error)
}

type Indexer struct {
	service *Service
	// acl reads the materialized keys for an object. Optional; see ACLSource.
	acl ACLSource
	// bodies reads the client-published projection. Optional; see BodySource.
	bodies BodySource
	// channels resolves a chat attachment's channel. Optional; see ChannelSource.
	channels ChannelSource
	logger   *slog.Logger
}

func NewIndexer(service *Service, acl ACLSource, bodies BodySource, logger *slog.Logger) *Indexer {
	idx := &Indexer{service: service, acl: acl, bodies: bodies, logger: logger}
	// A BodySource that can also answer the channel question answers it. One
	// constructor rather than two, so no caller can be updated for the body and
	// silently miss the channel.
	//
	// SAID OUT LOUD when it cannot. A type assertion that quietly fails leaves
	// every chat attachment with an empty channel_id and `?channel=` matching
	// nothing — which is the exact defect this seam was added to fix, returning
	// through the back door. A nil bodies is a deployment choice and says so at
	// a lower level; a NON-nil source that cannot answer is a wiring mistake.
	if cs, ok := bodies.(ChannelSource); ok {
		idx.channels = cs
	} else if bodies != nil && logger != nil {
		logger.Warn("the search body source cannot resolve a file's channel; "+
			"chat attachments will index with no channel and `?channel=` will not match them",
			"body_source", fmt.Sprintf("%T", bodies))
	} else if logger != nil {
		logger.Info("search indexing without a body source: titles only, no channel scope")
	}
	return idx
}

// HandleMessage indexes — or unindexes — the message an event describes.
//
// It returns an error instead of swallowing one, because the caller is a
// durable JetStream consumer and the return value is its ack decision. The
// handler used to log and return nothing, so a Meilisearch write failure still
// acked and the event was gone for good: the index is only ever written from
// this event stream, and nothing reconciles it afterwards.
//
// Two error classes, distinguished by *PermanentError:
//
//   - transient (Meilisearch unreachable, task still running): plain error, the
//     caller naks and the event comes back. Every write here is an upsert or a
//     delete keyed on the object id, so re-running the handler is harmless.
//   - permanent (payload does not parse, subject carries no workspace id, the
//     document was rejected): redelivery cannot help, so the caller terminates
//     the message rather than burning five deliveries on it first.
func (idx *Indexer) HandleMessage(ctx context.Context, msg *nats.Msg) error {
	envelope, ok, err := decodeEnvelope(msg, "message.new", "message.updated", "message.deleted")
	if err != nil || !ok {
		return err
	}

	var event MessageEvent
	if err := json.Unmarshal(envelope.Data, &event); err != nil {
		return &PermanentError{Reason: "malformed " + envelope.Type + " payload", Err: err}
	}
	if event.ID == "" {
		return &PermanentError{Reason: envelope.Type + " event carries no message id"}
	}

	// A soft-deleted message can arrive as an update too; drop it from the index
	// rather than re-indexing deleted content.
	if envelope.Type == "message.deleted" || event.IsDeleted {
		if err := idx.service.DeleteAwait(ctx, TypeMessage, event.ID); err != nil {
			return fmt.Errorf("unindex message %s: %w", event.ID, err)
		}
		idx.logger.Debug("unindexed message", "id", event.ID)
		return nil
	}

	workspaceID, err := workspaceFromSubject(msg.Subject)
	if err != nil {
		return err
	}
	createdAt, err := eventTime(event.CreatedAt, "message "+event.ID)
	if err != nil {
		return err
	}

	doc := MessageDoc{
		ID:          event.ID,
		ChannelID:   event.ChannelID,
		WorkspaceID: workspaceID,
		UserID:      event.UserID,
		Content:     event.Content,
		CreatedAt:   createdAt.Unix(),
	}.Doc()

	if err := idx.service.IndexAwait(ctx, doc); err != nil {
		return fmt.Errorf("index message %s: %w", event.ID, err)
	}
	idx.logger.Debug("indexed message", "id", event.ID)
	return nil
}

// HandleFile is the same contract for files: subject
// superops.{workspace_id}.file.{action}, and the ack decision is the returned
// error.
//
// internal/drive publishes these (events.go) and cmd/worker binds them to the
// file-indexer durable.
func (idx *Indexer) HandleFile(ctx context.Context, msg *nats.Msg) error {
	envelope, ok, err := decodeEnvelope(msg, "file.uploaded", "file.updated", "file.deleted")
	if err != nil || !ok {
		return err
	}

	var event FileEvent
	if err := json.Unmarshal(envelope.Data, &event); err != nil {
		return &PermanentError{Reason: "malformed " + envelope.Type + " payload", Err: err}
	}
	if event.ID == "" {
		return &PermanentError{Reason: envelope.Type + " event carries no file id"}
	}

	// The SAME mapping the index arm uses, and that is the whole point. DocID is
	// "<type>_<uuid>": indexing a document as document_<id> while deleting it as
	// file_<id> would leave a trashed document fully searchable.
	typ := FileObjectType(event.FileType)

	if envelope.Type == "file.deleted" || event.IsDeleted {
		if err := idx.service.DeleteAwait(ctx, typ, event.ID); err != nil {
			return fmt.Errorf("unindex %s %s: %w", typ, event.ID, err)
		}
		// Transitional: sweep the file_<uuid> twin an earlier release wrote for
		// this object, before the event carried a type. Two awaited deletes on a
		// trash is cheap, and leaving the twin behind means the document stays
		// searchable forever. Removable one release after a full reindex.
		if typ != TypeFile {
			if err := idx.service.DeleteAwait(ctx, TypeFile, event.ID); err != nil {
				return fmt.Errorf("unindex the untyped twin of %s %s: %w", typ, event.ID, err)
			}
		}
		idx.logger.Debug("unindexed file", "id", event.ID, "type", typ)
		return nil
	}

	workspaceID, err := workspaceFromSubject(msg.Subject)
	if err != nil {
		return err
	}
	createdAt, err := eventTime(event.CreatedAt, "file "+event.ID)
	if err != nil {
		return err
	}

	// The channel, when the event did not carry one. An attachment is uploaded
	// BEFORE it is posted, so at upload time it genuinely has none — the event
	// is republished when the message claims it, and this read is what makes
	// both events agree with cmd/reindex.
	if event.ChannelID == "" && idx.channels != nil {
		ch, err := idx.channels.ChannelForFile(ctx, event.ID)
		if err != nil {
			return fmt.Errorf("resolve channel for file %s: %w", event.ID, err)
		}
		event.ChannelID = ch
	}

	// The materialized keys, not a second derivation of them. A failure here is
	// returned rather than swallowed: indexing a document with the WRONG key set
	// is worse than not indexing it, because the wrong set is usually wider.
	var acl []string
	if idx.acl != nil {
		acl, err = idx.acl.KeysForObject(ctx, "file", event.ID)
		if err != nil {
			return fmt.Errorf("read access keys for file %s: %w", event.ID, err)
		}
	}

	// The body, from the database rather than the event. Only for a type that
	// has one: a plain upload's bytes are on disk and nothing extracts them
	// today, so asking would be a query per file event that always answers "".
	var body string
	if idx.bodies != nil && typ != TypeFile {
		body, err = idx.bodies.BodyForFile(ctx, event.ID)
		if err != nil {
			return fmt.Errorf("read projected body for %s %s: %w", typ, event.ID, err)
		}
	}

	doc := FileDoc{
		Type:        typ,
		Body:        body,
		ID:          event.ID,
		WorkspaceID: workspaceID,
		ChannelID:   event.ChannelID,
		FolderID:    event.FolderID,
		UserID:      event.UserID,
		Name:        event.Name,
		ACL:         acl,
		CreatedAt:   createdAt.Unix(),
	}.Doc()

	if err := idx.service.IndexAwait(ctx, doc); err != nil {
		return fmt.Errorf("index %s %s: %w", typ, event.ID, err)
	}
	// The other half of the transitional sweep: an object that used to be
	// indexed as file_<uuid> is now written under its own type, and the old id
	// would otherwise survive as a second, stale hit for the same object.
	if typ != TypeFile {
		if err := idx.service.DeleteAwait(ctx, TypeFile, event.ID); err != nil {
			return fmt.Errorf("remove the untyped twin of %s %s: %w", typ, event.ID, err)
		}
	}
	idx.logger.Debug("indexed file", "id", event.ID, "type", typ)
	return nil
}

type eventEnvelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// decodeEnvelope unwraps natspkg.Event and reports whether this handler has an
// opinion about it. ok=false with a nil error means "not ours": a consumer's
// subject filter is wider than the set of actions any one indexer cares about,
// and an event it does not handle is a clean ack, not a failure.
func decodeEnvelope(msg *nats.Msg, handled ...string) (eventEnvelope, bool, error) {
	var envelope eventEnvelope
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		return envelope, false, &PermanentError{Reason: "malformed event envelope on " + msg.Subject, Err: err}
	}
	for _, t := range handled {
		if envelope.Type == t {
			return envelope, true, nil
		}
	}
	return envelope, false, nil
}

// workspaceFromSubject extracts the workspace id from
// superops.{workspace_id}.{resource}.{action}.
//
// This is not cosmetic. Every search is filtered by `workspace_id = "..."`
// against a canonicalised uuid, so a document indexed with an empty or
// non-canonical workspace id can never be found again — it would be written,
// acked, and invisible. A subject that does not carry one comes from a broken
// publisher, not a broken network.
func workspaceFromSubject(subject string) (string, error) {
	parts := splitSubject(subject)
	if len(parts) < 2 {
		return "", &PermanentError{Reason: "event subject carries no workspace id: " + subject}
	}
	workspaceID, ok := canonicalUUID(parts[1])
	if !ok {
		return "", &PermanentError{Reason: "event subject workspace id is not a uuid: " + subject}
	}
	return workspaceID, nil
}

// eventTime parses an RFC3339 timestamp off an event. Sorting and the
// created_at filter both depend on it; the zero time this used to fall back to
// sorts every unparseable object to the very start of the index, silently.
func eventTime(raw, what string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, &PermanentError{Reason: what + " has an unusable created_at", Err: err}
	}
	return t, nil
}

func splitSubject(subject string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(subject); i++ {
		if subject[i] == '.' {
			parts = append(parts, subject[start:i])
			start = i + 1
		}
	}
	parts = append(parts, subject[start:])
	return parts
}
