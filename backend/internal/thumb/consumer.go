package thumb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	"github.com/Wick-Lim/SuperOps/backend/internal/storage"
)

// The thumbnailer: a durable consumer that turns an uploaded image into a
// preview and is the FIRST WRITER of files.thumbnail_key.
//
// That column has existed since migration 004 and has never held a row, so this
// is also the first time internal/file's Orphan.Keys() thumbnail handling and
// StorageKeysPresent's thumbnail arm do any work at all. Both were written for
// this moment and neither has been exercised.

// maxConcurrentDecodes bounds how many images are being decoded at once.
//
// The consumer's MaxAckPending is 256, and each decode holds up to MaxPixels*4
// bytes plus the scaled destination. Unbounded, a burst of large uploads is an
// OOM in a process that also indexes search and fans out notifications; the
// decodes are the only part of this worker that allocates in proportion to
// user-supplied numbers.
const maxConcurrentDecodes = 2

// Event is what drive and file publish when bytes land.
type Event struct {
	FileID      string `json:"file_id"`
	StorageKey  string `json:"storage_key"`
	ContentType string `json:"content_type"`
}

// Consumer generates previews.
type Consumer struct {
	pool    *pgxpool.Pool
	storage storage.Backend
	logger  *slog.Logger
	slots   chan struct{}
}

func NewConsumer(pool *pgxpool.Pool, store storage.Backend, logger *slog.Logger) *Consumer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Consumer{
		pool:    pool,
		storage: store,
		logger:  logger,
		slots:   make(chan struct{}, maxConcurrentDecodes),
	}
}

// PermanentError marks an event that must never be redelivered. It matches the
// error contract cmd/worker's bindDurable enforces: Permanent() true means TERM,
// anything else means nak and try again.
type PermanentError struct {
	Reason string
	Err    error
}

func (e *PermanentError) Error() string {
	if e.Err != nil {
		return e.Reason + ": " + e.Err.Error()
	}
	return e.Reason
}
func (e *PermanentError) Unwrap() error   { return e.Err }
func (e *PermanentError) Permanent() bool { return true }

// Handle generates one thumbnail.
//
// Every outcome is one of three, and getting the mapping wrong is either a
// poison message redelivered forever or a transient outage that silently drops
// previews:
//
//	permanent — not an image, undecodable, or past the pixel budget. TERM.
//	transient — storage or Postgres unreachable. nak, retry.
//	success   — the object is written and the column is set.
func (c *Consumer) Handle(ctx context.Context, msg *nats.Msg) error {
	var envelope struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		return &PermanentError{Reason: "malformed thumbnail envelope", Err: err}
	}
	var event Event
	if err := json.Unmarshal(envelope.Data, &event); err != nil {
		return &PermanentError{Reason: "malformed thumbnail payload", Err: err}
	}
	if event.FileID == "" || event.StorageKey == "" {
		return &PermanentError{Reason: "thumbnail event names no file or no object"}
	}

	// Checked BEFORE anything is fetched: this is what stops the worker
	// downloading a 60MB video to discover it cannot decode it.
	if !Decodable(event.ContentType) {
		c.logger.Debug("no preview for this type", "file_id", event.FileID, "content_type", event.ContentType)
		return nil
	}
	if c.storage == nil {
		return &PermanentError{Reason: "object storage is not configured; no preview can be generated"}
	}

	// One slot per decode. Acquired before the fetch so the bytes are not held
	// in memory while waiting.
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}

	body, _, err := c.storage.Get(ctx, event.StorageKey)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// The object is gone — trashed, purged, or replaced by a new
			// version. Permanent: there is nothing to come back to.
			return &PermanentError{Reason: "source object no longer exists", Err: err}
		}
		return fmt.Errorf("fetch %s for preview: %w", event.StorageKey, err)
	}
	preview, genErr := Generate(body)
	_ = body.Close()
	if genErr != nil {
		if errors.Is(genErr, ErrNoPreview) || errors.Is(genErr, ErrTooLarge) {
			c.logger.Info("no preview generated", "file_id", event.FileID, "reason", genErr)
			return nil
		}
		return fmt.Errorf("generate preview for %s: %w", event.FileID, genErr)
	}

	thumbKey := ThumbKey(event.StorageKey)

	// Object BEFORE column, deliberately. The column is what makes the object
	// reachable and what protects it from the bucket sweep, so an object with no
	// column is a leak the sweep collects, while a column pointing at nothing is
	// a broken image in every listing.
	if err := c.storage.Put(ctx, thumbKey, bytes.NewReader(preview.Bytes),
		int64(len(preview.Bytes)), MediaType); err != nil {
		return fmt.Errorf("store preview for %s: %w", event.FileID, err)
	}

	// The WHERE clause is the concurrency guard: a new version may have landed
	// while this ran, and stamping a preview of the old bytes onto the new head
	// is worse than having none.
	tag, err := c.pool.Exec(ctx, `
		UPDATE files
		   SET thumbnail_key = $2, width = $3, height = $4
		 WHERE id = $1 AND storage_key = $5 AND trashed_at IS NULL`,
		event.FileID, thumbKey, preview.Width, preview.Height, event.StorageKey)
	if err != nil {
		// The object is already written; leaving it is a leak the bucket sweep
		// collects, which is the cheap failure. Retry the column.
		return fmt.Errorf("record preview for %s: %w", event.FileID, err)
	}
	if tag.RowsAffected() == 0 {
		// The file moved on. Remove the preview we just made rather than leaving
		// an object nothing will ever name.
		_ = c.storage.Delete(ctx, thumbKey)
		c.logger.Debug("preview discarded; the file changed while it was generated",
			"file_id", event.FileID)
		return nil
	}

	c.logger.Debug("generated preview", "file_id", event.FileID,
		"width", preview.Width, "height", preview.Height, "bytes", len(preview.Bytes))
	return nil
}

// ThumbKey derives the preview's key from the original's.
//
// Under the same workspace/date prefix, so the bucket sweep's hex sharding
// covers it on the same run as the object it belongs to. A distinct suffix
// rather than a distinct prefix, so a key can be read as "the preview of that".
func ThumbKey(storageKey string) string {
	ext := path.Ext(storageKey)
	return strings.TrimSuffix(storageKey, ext) + "_thumb" + Extension
}
