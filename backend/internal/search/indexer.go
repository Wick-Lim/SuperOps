package search

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

type MessageEvent struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	UserID    string `json:"user_id"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
	IsDeleted bool   `json:"is_deleted"`
}

type Indexer struct {
	service *Service
	logger  *slog.Logger
}

func NewIndexer(service *Service, logger *slog.Logger) *Indexer {
	return &Indexer{service: service, logger: logger}
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
//     delete keyed on the message id, so re-running the handler is harmless.
//   - permanent (payload does not parse, subject carries no workspace id, the
//     document was rejected): redelivery cannot help, so the caller terminates
//     the message rather than burning five deliveries on it first.
func (idx *Indexer) HandleMessage(ctx context.Context, msg *nats.Msg) error {
	var envelope struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		return &PermanentError{Reason: "malformed event envelope on " + msg.Subject, Err: err}
	}

	switch envelope.Type {
	case "message.new", "message.updated", "message.deleted":
	default:
		// The consumer's filter is superops.*.message.*, which also carries events
		// this indexer has no opinion about. Nothing to do, and nothing wrong.
		return nil
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
		if err := idx.service.DeleteMessageAwait(ctx, event.ID); err != nil {
			return fmt.Errorf("unindex message %s: %w", event.ID, err)
		}
		idx.logger.Debug("unindexed message", "id", event.ID)
		return nil
	}

	// Extract the workspace id from the subject: superops.{workspace_id}.message.{action}.
	//
	// This is not cosmetic. Every search is filtered by `workspace_id = "..."`
	// against a canonicalised uuid, so a document indexed with an empty or
	// non-canonical workspace id can never be found again — it would be written,
	// acked, and invisible. A subject that does not carry one comes from a broken
	// publisher, not a broken network.
	parts := splitSubject(msg.Subject)
	if len(parts) < 2 {
		return &PermanentError{Reason: "event subject carries no workspace id: " + msg.Subject}
	}
	workspaceID, ok := canonicalUUID(parts[1])
	if !ok {
		return &PermanentError{Reason: "event subject workspace id is not a uuid: " + msg.Subject}
	}

	// Sorting and the created_at filter both depend on this. The zero time this
	// used to fall back to sorts every unparseable message to the very start of
	// the index, silently.
	createdAt, err := time.Parse(time.RFC3339Nano, event.CreatedAt)
	if err != nil {
		return &PermanentError{Reason: "message " + event.ID + " has an unusable created_at", Err: err}
	}

	doc := MessageDoc{
		ID:          event.ID,
		ChannelID:   event.ChannelID,
		WorkspaceID: workspaceID,
		UserID:      event.UserID,
		Content:     event.Content,
		CreatedAt:   createdAt.Unix(),
	}

	if err := idx.service.IndexMessageAwait(ctx, doc); err != nil {
		return fmt.Errorf("index message %s: %w", event.ID, err)
	}
	idx.logger.Debug("indexed message", "id", event.ID)
	return nil
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
