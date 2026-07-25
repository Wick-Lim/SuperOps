package notification

// This package is the MESSAGE-DOMAIN PRODUCER for the unified inbox.
//
// Everything generic about a notification — the row, the coalescing, the
// idempotency gate, the badge, the preferences, the HTTP surface, the digest —
// lives in internal/inbox and is shared by every pillar
// (docs/plans/README.md "Resolved conflicts" §1). What is left here is the part
// that is genuinely message-shaped and would read wrong anywhere else: the DM
// roster, mention extraction, the thread-parent lookup, the block list and
// channel_members' mute / notification_pref.
//
// The three durable consumers keep their names, their filters and their error
// contract. What changed underneath them is that they now build one
// inbox.Request per event and call inbox.Notifier.Deliver once, instead of
// looping one INSERT per recipient.

// Kinds this package produces. They are inbox kinds, not a local enum: the
// values are the strings that land in inbox_events.kind and that a
// notification_prefs row matches on, so they belong to the vocabulary rather
// than to this package. Aliased here only so the fan-out below reads in domain
// terms.
const (
	KindDM          = "message.dm"
	KindMention     = "message.mention"
	KindThreadReply = "message.thread_reply"
	KindReaction    = "message.reaction"
	KindInvite      = "channel.invited"
)
