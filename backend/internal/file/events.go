package file

import (
	"context"
	"time"
)

// Chat-attachment events.
//
// NOTHING PUBLISHED THESE. internal/drive's upload path emits an event on every
// write, but the route the client actually calls for a chat attachment —
// POST /api/v1/files/upload — emitted none, and neither did its delete. So a
// file posted into a conversation was findable only after an operator ran
// cmd/reindex by hand, and a deleted one became an index orphan that no
// operation could remove, because reindex upserts from live rows and has no
// prune pass.
//
// It is also why `?channel=` returned nothing, ever: channel_id reaches the
// index from `files.message_id -> messages.channel_id`, which only this path
// knows. A live-indexed attachment carried an empty channel while a rebuilt one
// carried the real value, so the same file's channel scoping depended on how it
// had last been indexed.

// ChatFileEvent is what the search index needs about a chat attachment.
//
// It lives here, not in internal/drive, because internal/drive already imports
// this package — the dependency cannot run the other way. The Publisher
// interface is the seam.
type ChatFileEvent struct {
	ID          string
	WorkspaceID string
	UserID      string
	Name        string
	// ChannelID is the channel of the message this file is attached to. Empty
	// for an attachment uploaded before it was posted, which is the ordinary
	// case: the client uploads, then sends. The message route re-publishes once
	// the attachment has a home.
	ChannelID string
	CreatedAt time.Time
	Deleted   bool
}

// Events is the narrow seam onto whatever publishes. A nil Events is a working
// no-op, so a deployment without NATS needs no branch at the call site.
type Events interface {
	PublishChatFile(ctx context.Context, e ChatFileEvent)
}

func (h *Handler) publish(ctx context.Context, e ChatFileEvent) {
	if h.events == nil {
		return
	}
	h.events.PublishChatFile(ctx, e)
}
