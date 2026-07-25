package inbox

import (
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// Namespace seeds every derived id in this package.
//
// It is byte-for-byte the uuid internal/notification's notificationID has always
// used, and that is deliberate rather than incidental: migration 020's backfill
// re-derives event ids for the notifications it carries forward, so a
// message.created event still sitting in JetStream at cutover has to hash to the
// same id here that the backfill computed there. Changing this constant
// duplicates every inbox event exactly once on the first fan-out after the
// change — silently, because both rows are individually valid.
var Namespace = uuid.MustParse("3d0f5e2a-6c1b-4f7e-9a1d-2b8c5f0e7a41")

// derive hashes a tuple into a stable UUIDv5.
//
// Parts are LENGTH-PREFIXED rather than merely delimited, so no choice of
// separator can make two different tuples produce the same id — the property
// notificationID had and the one thing about it that was not arbitrary. The wire
// format is "<len>:<part>|<len>:<part>|…" and migration 020 reproduces it
// exactly with format('%s:%s|%s:%s|%s:%s', length(a), a, …).
//
// Postgres `length()` counts characters where Go's len() counts bytes. Every
// part fed to either side is ASCII (a uuid, a lowercase dotted kind, a fixed
// subject type), so the two agree; a caller passing non-ASCII into a part that
// the SQL side also derives would break that, which is why nothing does.
func derive(parts ...string) string {
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteByte('|')
		}
		b.WriteString(strconv.Itoa(len(p)))
		b.WriteByte(':')
		b.WriteString(p)
	}
	return uuid.NewSHA1(Namespace, []byte(b.String())).String()
}

// EventID derives the id of one (thing that happened, person it happened to).
//
// This is the idempotency gate for the entire package. The worker consumes the
// events that produce these from durable JetStream consumers with explicit acks,
// which is at-least-once: a fan-out that fails halfway through, or simply
// outruns AckWait, is redelivered in full. With random ids every recipient
// already processed would get a second row and — far worse, because coalescing
// makes the item counter an increment — a second unread. Deriving the id makes
// the INSERT an `ON CONFLICT (id) DO NOTHING` no-op the second time round, and
// only an insert that actually happened is allowed to move the counter.
//
// key identifies the event within its kind for one user: the message id, or the
// object id joined to a discriminator when several distinct events share an
// object.
func EventID(kind, userID, key string) string { return derive(kind, userID, key) }

// ItemID derives the id of one (person, subject) row.
//
// Derived rather than random so it is stable across a rebuild and usable as the
// keyset tiebreaker httputil.Cursor requires — the cursor is (CreatedAt, ID) and
// ID is a single string, so an item's id has to be a function of its identity
// rather than of when it happened to be created.
func ItemID(userID, subjectType, subjectID string) string {
	return derive(userID, subjectType, subjectID)
}

// eventKey folds the discriminator into the event's identity.
//
// The NUL separator is the one internal/notification already used for reactions
// (message + reactor + emoji), and it is safe for the same reason the length
// prefix is unnecessary here: neither half can contain a NUL.
func eventKey(objectID, discriminator string) string {
	if discriminator == "" {
		return objectID
	}
	return objectID + "\x00" + discriminator
}
