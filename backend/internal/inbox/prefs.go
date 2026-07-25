package inbox

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
)

// Pref is one resolved delivery decision for one (user, kind).
type Pref struct {
	InApp bool
	Push  bool
	Email string
}

// DefaultPref is the built-in, and it is the last rung of the resolution ladder
// rather than a row anybody has to create. A user with no notification_prefs
// rows gets in-app and push, and their email batched into a digest.
var DefaultPref = Pref{InApp: true, Push: true, Email: EmailDigest}

// PrefRow is one stored row of notification_prefs.
type PrefRow struct {
	Kind  string `json:"kind"`
	InApp bool   `json:"in_app"`
	Push  bool   `json:"push"`
	Email string `json:"email"`
}

// PrefSet is one workspace's preferences for a set of users, already loaded.
//
// It exists as a distinct type to make the N+1 guard structural rather than a
// review-checklist line: the only way to get one is LoadPrefs, which issues ONE
// query for the WHOLE recipient set, and Resolve is a map lookup that cannot
// reach the database at all. A per-recipient preference query is therefore not
// something a future edit can accidentally introduce inside the fan-out — it
// would have to change this type's shape first.
type PrefSet struct {
	// byUser[user][kindPattern] — patterns are the exact kind, '<resource>.*'
	// and '*'.
	byUser map[string]map[string]Pref
}

// Resolve applies the ladder, most specific first:
//
//  1. notification_prefs exact kind
//  2. notification_prefs '<resource>.*'
//  3. notification_prefs '*'
//  4. DefaultPref
//
// Channel mute and channel_members.notification_pref sit ABOVE all of these and
// are deliberately not here: they are more specific still (one channel rather
// than one kind), they already exist, they already work, and users already rely
// on them. internal/notification applies them before it ever calls Deliver, so
// a muted channel produces no Request at all rather than a suppressed one.
func (p PrefSet) Resolve(userID, kind string) Pref {
	byKind := p.byUser[userID]
	if byKind == nil {
		return DefaultPref
	}
	if pref, ok := byKind[kind]; ok {
		return pref
	}
	if resource, _, found := strings.Cut(kind, "."); found {
		if pref, ok := byKind[resource+".*"]; ok {
			return pref
		}
	}
	if pref, ok := byKind["*"]; ok {
		return pref
	}
	return DefaultPref
}

// LoadPrefs reads every preference row for a recipient set in one query.
//
// `WHERE user_id = ANY($1) AND workspace_id = $2` — the same shape the existing
// message fan-out uses for channel preferences. This is the query whose absence
// would make a 500-recipient @channel 500 round trips, and it is why PrefSet is
// a value rather than a service with a pool on it.
func (r *Repository) LoadPrefs(ctx context.Context, workspaceID string, userIDs []string) (PrefSet, error) {
	set := PrefSet{byUser: map[string]map[string]Pref{}}
	if len(userIDs) == 0 {
		return set, nil
	}

	rows, err := r.pool.Query(ctx,
		`SELECT user_id, kind, in_app, push, email
		   FROM notification_prefs
		  WHERE user_id = ANY($1) AND workspace_id = $2`,
		userIDs, workspaceID)
	if err != nil {
		return set, fmt.Errorf("load notification prefs: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var uid string
		var kind string
		var p Pref
		if err := rows.Scan(&uid, &kind, &p.InApp, &p.Push, &p.Email); err != nil {
			return set, fmt.Errorf("scan notification pref: %w", err)
		}
		if set.byUser[uid] == nil {
			set.byUser[uid] = map[string]Pref{}
		}
		set.byUser[uid][kind] = p
	}
	return set, rows.Err()
}

// ListPrefs returns one user's stored rows for one workspace, for the prefs
// screen. Absent rows mean DefaultPref and are not synthesised here — the client
// needs to be able to tell "I chose the default" from "I chose nothing", because
// only the second one keeps following a change to the default.
func (r *Repository) ListPrefs(ctx context.Context, userID, workspaceID string) ([]PrefRow, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT kind, in_app, push, email FROM notification_prefs
		  WHERE user_id = $1 AND workspace_id = $2 ORDER BY kind`,
		userID, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list notification prefs: %w", err)
	}
	defer rows.Close()

	out := []PrefRow{}
	for rows.Next() {
		var p PrefRow
		if err := rows.Scan(&p.Kind, &p.InApp, &p.Push, &p.Email); err != nil {
			return nil, fmt.Errorf("scan notification pref: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ReplacePrefs is a full replace of one user's rows for one workspace, in one
// transaction.
//
// Full replace rather than per-row PATCH because the prefs screen edits the
// whole ladder at once, and a partial update of an ordered ruleset is how two
// clients produce a state neither of them asked for.
func (r *Repository) ReplacePrefs(ctx context.Context, userID, workspaceID string, prefs []PrefRow) error {
	return database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`DELETE FROM notification_prefs WHERE user_id = $1 AND workspace_id = $2`,
			userID, workspaceID); err != nil {
			return fmt.Errorf("clear notification prefs: %w", err)
		}
		if len(prefs) == 0 {
			return nil
		}

		kinds := make([]string, len(prefs))
		inApp := make([]bool, len(prefs))
		push := make([]bool, len(prefs))
		email := make([]string, len(prefs))
		for i, p := range prefs {
			kinds[i], inApp[i], push[i], email[i] = p.Kind, p.InApp, p.Push, p.Email
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO notification_prefs (user_id, workspace_id, kind, in_app, push, email, updated_at)
			 SELECT $1, $2, n.kind, n.in_app, n.push, n.email, $7
			   FROM unnest($3::text[], $4::bool[], $5::bool[], $6::text[]) AS n(kind, in_app, push, email)
			 ON CONFLICT (user_id, workspace_id, kind) DO UPDATE SET
			     in_app = EXCLUDED.in_app, push = EXCLUDED.push,
			     email = EXCLUDED.email, updated_at = EXCLUDED.updated_at`,
			userID, workspaceID, kinds, inApp, push, email, time.Now()); err != nil {
			return fmt.Errorf("write notification prefs: %w", err)
		}
		return nil
	})
}

// ValidPrefKind reports whether a kind string is one notification_prefs will
// accept: an exact '<resource>.<verb>', a '<resource>.*' wildcard, or '*'.
//
// Checked in Go before the INSERT so the client gets a 400 naming the bad value
// rather than a 500 carrying a CHECK constraint name.
func ValidPrefKind(kind string) bool {
	if kind == "*" {
		return true
	}
	resource, verb, found := strings.Cut(kind, ".")
	if !found || !validSegment(resource) {
		return false
	}
	return verb == "*" || validSegment(verb)
}

// ValidKind reports whether a kind is acceptable on an event — no wildcards.
func ValidKind(kind string) bool {
	resource, verb, found := strings.Cut(kind, ".")
	return found && validSegment(resource) && validSegment(verb)
}

// validSegment mirrors migration 020's CHECK: ^[a-z][a-z0-9_]{0,23}$.
func validSegment(s string) bool {
	if len(s) == 0 || len(s) > 24 {
		return false
	}
	if s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// validObjectType mirrors migration 020's CHECK on object_type / subject_type:
// ^[a-z][a-z0-9_]{0,31}$.
func validObjectType(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	if s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// ValidEmailMode reports whether m is one of the three stored email modes.
func ValidEmailMode(m string) bool {
	return m == EmailNever || m == EmailDigest || m == EmailImmediate
}
