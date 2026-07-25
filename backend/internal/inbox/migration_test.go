package inbox

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The digest-storm guard, asserted at the only place it can be: the migration.
//
// Migration 020 backfills every unread `notifications` row into an inbox item.
// If those items are left with notified_at NULL, the first inbox_digest tick
// after the cutover — ten minutes later — mails every user in the company a
// summary of every notification they have ever ignored. There is no undo and no
// rate limiter that helps, because it is one digest per person and the ceiling
// permits one per hour.
//
// The line that prevents it is a single `NOW()` in the backfill's column list,
// and it is exactly the kind of line a later edit removes while tidying. This is
// a text assertion rather than a behavioural one on purpose: reproducing the
// backfill behaviourally would mean rolling the schema back to pre-020 inside a
// test that shares its database with every other test in the package, and the
// failure it guards is "somebody deleted a line", which a text assertion catches
// precisely.
func TestBackfillSuppressesTheFirstDigest(t *testing.T) {
	body := readMigration(t, "020_inbox.up.sql")

	// The INSERT INTO inbox_items that carries the backfill.
	idx := strings.Index(body, "INSERT INTO inbox_items (")
	if idx < 0 {
		t.Fatal("migration 020 no longer has an inbox_items backfill; if that is intentional, delete this test")
	}
	backfill := body[idx:]
	if end := strings.Index(backfill, "INSERT INTO inbox_events"); end > 0 {
		backfill = backfill[:end]
	}

	if !strings.Contains(backfill, "notified_at") {
		t.Fatal("the inbox_items backfill no longer names notified_at. Without it, the first " +
			"digest cycle after deploy mails every user their entire notification history")
	}
	// The column list ends with notified_at and the value list ends with NOW().
	if !regexp.MustCompile(`(?s)notified_at\s*\n?\s*\)`).MatchString(backfill) {
		t.Error("notified_at is no longer the last column of the backfill's column list; " +
			"check that it is still paired with NOW()")
	}
	if !strings.Contains(backfill, "NOW()") {
		t.Fatal("the inbox_items backfill no longer sets notified_at = NOW()")
	}
}

// The other property the backfill must have: a row whose channel is gone is
// SKIPPED, not defaulted. Inventing a workspace for it would file somebody's
// notification into a tenant they do not belong to.
func TestBackfillJoinsRatherThanDefaultsTheWorkspace(t *testing.T) {
	body := readMigration(t, "020_inbox.up.sql")

	if !strings.Contains(body, "JOIN channels c ON c.id = (n.data->>'channel_id')::uuid") {
		t.Fatal("the backfill no longer resolves workspace_id by an INNER JOIN to channels. " +
			"A LEFT JOIN, or a default, would put an orphaned notification in an arbitrary tenant")
	}
	if strings.Contains(body, "LEFT JOIN channels c ON c.id = (n.data->>'channel_id')") {
		t.Fatal("the backfill uses a LEFT JOIN for the workspace lookup; an unresolvable row " +
			"must be skipped, not carried with a NULL workspace")
	}
}

// The namespace and the key encoding are shared between Go and SQL. If the
// migration stops deriving ids the same way, a message.created event still in
// JetStream at cutover produces a SECOND item for a notification the backfill
// already carried — silently, because both rows are individually valid.
func TestBackfillDerivesIDsLikeTheGoSide(t *testing.T) {
	body := readMigration(t, "020_inbox.up.sql")

	if !strings.Contains(body, Namespace.String()) {
		t.Fatalf("migration 020 no longer derives ids in the %s namespace that inbox.Namespace uses",
			Namespace)
	}
	// The length-prefixed key encoding derive() produces.
	if !strings.Contains(body, "format('%s:%s|%s:%s|%s:%s'") {
		t.Fatal("migration 020 no longer uses the length-prefixed key encoding inbox.derive produces")
	}
}

func readMigration(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "migrations", name)
	body, err := os.ReadFile(path) //nolint:gosec // a fixed path inside the repository
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}
