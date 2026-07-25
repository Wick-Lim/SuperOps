// Package inbox is the product's notification substrate: one "what needs my
// attention" list spanning every pillar, coalesced per subject, with three
// states and per-kind delivery preferences.
//
// # Why this package exists, and why it is pillar-neutral
//
// `notifications` (migration 005) is keyed to a message id, carries a five-value
// Postgres enum and has no workspace column. Nine pillars means an issue
// assigned, a document commented, mail received, a huddle invite and a workflow
// failure all have to land in the same list — and four separate plans each
// independently proposed extending that enum, two of which would have failed
// outright against each other (docs/plans/README.md "Resolved conflicts" §1).
//
// So this package owns notifications for the whole product, and it deliberately
// knows nothing about channels or messages. A work-tracking package importing
// internal/notification to file an inbox event would read wrong; the seam is the
// point. internal/notification stays as the message-domain PRODUCER and calls
// Notifier.Deliver.
//
// # The entry point every pillar uses
//
// There are exactly two, and they are the same code path:
//
//   - In-process:      inbox.Notifier.Deliver(ctx, inbox.Request{...})
//   - Out-of-process:  publish superops.{workspace}.inbox.requested carrying an
//     envelope {"type":"inbox.requested","data":<Request>}. One durable consumer
//     (inbox-fanout, bound in cmd/worker) calls Deliver. A new pillar adds zero
//     durables and zero worker wiring.
//
// Request carries an EXPLICIT recipient list. There is no watch/subscribe model
// in v1: recipients are directed — the person mentioned, the assignee, the DM
// participant, the thread author — which is what keeps the fan-out audience a
// list rather than a query.
//
// # The one number
//
// The badge is `COUNT(*) FROM inbox_items WHERE user_id = $1 AND state =
// 'unread'` and nothing else. Three subsystems own part of "unread" —
// notifications.is_read, the channel_members read marker driving unread.update,
// and inbox_items.state — and they are kept correct by covering DISJOINT things
// and agreeing at their single overlap:
//
//   - A plain channel message never creates an inbox item. That is the channel
//     badge's job and it already works. Only DIRECTED events do.
//   - PUT /channels/{id}/read marks that channel's inbox items read in the same
//     transaction as the read marker (Repository.MarkSubjectRead).
//   - inbox_reconcile in the worker recomputes unread_count and state from
//     inbox_events for a rotating slice of users, reports mismatches and repairs
//     them — the same treatment docs/plans/00-permissions.md prescribes for
//     acl_key drift, for the same reason: a silent divergence here is not a
//     stale cache, it is a wrong answer to a question the user asked.
package inbox

import "time"

// Kinds emitted by internal/notification, the message-domain producer. A pillar
// is free to define its own; nothing here enumerates them exhaustively, which is
// the entire point of `kind` being TEXT rather than an enum.
const (
	KindMessageDM          = "message.dm"
	KindMessageMention     = "message.mention"
	KindMessageThreadReply = "message.thread_reply"
	KindMessageReaction    = "message.reaction"
	KindChannelInvited     = "channel.invited"
)

// Item states. 'open' is not a state, it is the default filter (state <> 'done').
const (
	StateUnread = "unread"
	StateRead   = "read"
	StateDone   = "done"
)

// Email delivery modes for notification_prefs.email.
const (
	EmailNever     = "never"
	EmailDigest    = "digest"
	EmailImmediate = "immediate"
)

// Subject types this package writes today. A subject is what a burst coalesces
// under — usually the object's container.
const SubjectChannel = "channel"

// previewRunes bounds an item's preview. It is the same rune budget the push
// path uses, and it is a RUNE budget: this deployment is Korean-language, so a
// byte slice would cut mid-rune and produce invalid UTF-8 that Postgres rejects
// on INSERT.
const previewRunes = 140

// maxDataBytes caps the serialized `data` payload. It is written by every
// pillar, and without a cap it becomes an accidental content store inside a
// table the digest job mails excerpts of. Rejected at the Deliver boundary,
// where the caller can still see the error, rather than truncated silently.
const maxDataBytes = 4 << 10

// MaxRecipients bounds one fan-out. `@channel` in a 5000-member channel has to
// finish inside cmd/worker's 25-second handler budget; beyond this the event is
// terminated as permanently unprocessable rather than naked forever, because a
// wider fan-out will not get narrower on redelivery.
const MaxRecipients = 2000

// kindRank orders the reasons an item exists. It decides top_kind — the icon and
// the kind= filter — when several kinds land on one subject.
//
// It lives in Go and not in the database on purpose: a ranking encoded in a
// CHECK constraint or a lookup table would need a migration per pillar, which is
// exactly what `kind TEXT` exists to avoid. An unregistered kind ranks below
// every registered one and is overwritten by the next event, which is the
// forgiving direction: a pillar that never registers a rank still gets a
// sensible newest-wins icon.
var kindRank = map[string]int{
	KindMessageDM:          50,
	KindMessageMention:     40,
	KindChannelInvited:     30,
	KindMessageThreadReply: 20,
	KindMessageReaction:    10,
}

// rankOf is the rank of a kind, or 0 for one nothing has registered.
func rankOf(kind string) int { return kindRank[kind] }

// kindsRankedAbove lists the registered kinds that outrank kind.
//
// It is passed to the item upsert so the CASE that picks top_kind can be written
// without a ranking function in SQL: an existing top_kind in this set survives,
// anything else — including a kind this build has never heard of — loses to the
// incoming one.
func kindsRankedAbove(kind string) []string {
	r := rankOf(kind)
	above := make([]string, 0, len(kindRank))
	for k, v := range kindRank {
		if v > r {
			above = append(above, k)
		}
	}
	return above
}

// Event is one immutable row of inbox_events: a thing that happened, to one
// person. Its id is derived (see EventID) and that derivation is the whole
// idempotency story.
type Event struct {
	ID          string            `json:"id"`
	UserID      string            `json:"user_id"`
	WorkspaceID string            `json:"workspace_id"`
	Kind        string            `json:"kind"`
	ObjectType  string            `json:"object_type"`
	ObjectID    string            `json:"object_id"`
	SubjectType string            `json:"subject_type"`
	SubjectID   string            `json:"subject_id"`
	ActorID     *string           `json:"actor_id"`
	Title       string            `json:"title"`
	Body        string            `json:"body"`
	Data        map[string]string `json:"data"`
	CreatedAt   time.Time         `json:"created_at"`
}

// Item is one mutable row of inbox_items: everything that has happened to one
// person about one subject, collapsed.
type Item struct {
	ID          string  `json:"id"`
	UserID      string  `json:"user_id"`
	WorkspaceID string  `json:"workspace_id"`
	SubjectType string  `json:"subject_type"`
	SubjectID   string  `json:"subject_id"`
	State       string  `json:"state"`
	TopKind     string  `json:"top_kind"`
	UnreadCount int     `json:"unread_count"`
	EventCount  int     `json:"event_count"`
	LastEventID string  `json:"last_event_id"`
	LastActorID *string `json:"last_actor_id"`
	Title       string  `json:"title"`
	Preview     string  `json:"preview"`

	FirstAt    time.Time  `json:"first_at"`
	LastAt     time.Time  `json:"last_at"`
	ReadAt     *time.Time `json:"read_at"`
	DoneAt     *time.Time `json:"done_at"`
	NotifiedAt *time.Time `json:"notified_at"`
}

// Request is one thing that happened plus the people it happened to. It is the
// only input to the fan-out, and it is what travels on
// superops.{ws}.inbox.requested.
type Request struct {
	WorkspaceID string `json:"workspace_id"`

	// Kind is '<resource>.<verb>' — the shape internal/audit already uses for
	// actions, and the shape migration 020's CHECK enforces.
	Kind string `json:"kind"`

	// Object is what the event is about: the message, the comment, the issue.
	ObjectType string `json:"object_type"`
	ObjectID   string `json:"object_id"`

	// Subject is what it coalesces under: the channel, the doc, the issue. For
	// an issue, subject and object are the same thing.
	SubjectType string `json:"subject_type"`
	SubjectID   string `json:"subject_id"`

	ActorID string            `json:"actor_id"`
	Title   string            `json:"title"`
	Body    string            `json:"body"`
	Data    map[string]string `json:"data"`

	// Discriminator folded into the derived event id, for events that share an
	// object but are genuinely distinct. Two people reacting to one message are
	// two events; the same person re-reacting with the same emoji is not. Empty
	// means the object alone identifies the event.
	Discriminator string `json:"discriminator,omitempty"`

	// Recipients is explicit and deduplicated by Deliver. The author is NOT
	// excluded here — the producer knows who the author is and excludes them,
	// because "who should not hear about this" is domain knowledge (a DM roster,
	// a block list, a channel mute) that this package deliberately does not have.
	Recipients []string `json:"recipients"`
}

// Delivered reports what one recipient's fan-out actually did.
type Delivered struct {
	UserID string
	// Created is false on a redelivery: the event row already existed, so no
	// counter moved, and nothing may buzz.
	Created bool
	// Unread is the recipient's item count after this event, for the app badge.
	Unread int
	// Push is the resolved preference. False means an item exists but no device
	// is woken.
	Push bool
	// Item is the projection the realtime relay and the compat route carry.
	Item *Item
}
