// Package huddle is the live call attached to a channel.
//
// THE DIVISION OF AUTHORITY, restated here because every function below
// depends on it: the SFU knows who is in the call; this package knows the call
// EXISTS and what it is scoped to. Participant rows are written only by the
// webhook sink and the reconciler. A client saying "I joined" is a claim
// nothing can verify, and a roster built from such claims shows people who
// never connected and misses people who did.
//
// A huddle is not an acl_object. Its capability IS its scope's capability,
// computed per request. Storing a copy would create a second answer to a
// question that already has one.
package huddle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
)

// ScopeChannel is the only scope v1 registers. The column takes any shape-valid
// string so an issue-scoped huddle is a row value rather than a migration, but
// the Go side is a closed set: an unregistered scope is a 400, because the
// capability lookup for it does not exist.
const ScopeChannel = "channel"

// Room names are derived from the huddle id, never from the scope. A restarted
// huddle must land in a fresh room or it inherits the media state — and the
// participant list — of the one that just ended.
func roomName(id string) string { return "hd_" + id }

var (
	ErrNotFound    = errors.New("huddle: not found")
	ErrEnded       = errors.New("huddle: already ended")
	ErrBadScope    = errors.New("huddle: unsupported scope")
	ErrDuplicateID = errors.New("huddle: event already applied")
)

// Huddle is one call.
type Huddle struct {
	ID              string     `json:"id"`
	WorkspaceID     string     `json:"workspace_id"`
	ScopeType       string     `json:"scope_type"`
	ScopeID         string     `json:"scope_id"`
	RoomName        string     `json:"room_name"`
	StartedBy       *string    `json:"started_by"`
	MaxParticipants int        `json:"max_participants"`
	CreatedAt       time.Time  `json:"created_at"`
	EndedAt         *time.Time `json:"ended_at"`
	EndedReason     *string    `json:"ended_reason"`
}

func (h *Huddle) Live() bool { return h != nil && h.EndedAt == nil }

// Participant is one live or past session in a call.
type Participant struct {
	HuddleID        string     `json:"huddle_id"`
	ParticipantSID  string     `json:"participant_sid"`
	UserID          string     `json:"user_id"`
	JoinedAt        time.Time  `json:"joined_at"`
	LeftAt          *time.Time `json:"left_at"`
	LeftReason      *string    `json:"left_reason"`
	IsScreenSharing bool       `json:"is_screen_sharing"`
}

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

const huddleColumns = `id::text, workspace_id::text, scope_type, scope_id::text, room_name,
	started_by::text, max_participants, created_at, ended_at, ended_reason`

func scanHuddle(row pgx.Row) (*Huddle, error) {
	var h Huddle
	if err := row.Scan(&h.ID, &h.WorkspaceID, &h.ScopeType, &h.ScopeID, &h.RoomName,
		&h.StartedBy, &h.MaxParticipants, &h.CreatedAt, &h.EndedAt, &h.EndedReason); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan huddle: %w", err)
	}
	return &h, nil
}

// StartOrJoin returns the live huddle for a scope, creating it if there is none.
//
// TWO PEOPLE CLICKING AT THE SAME MOMENT is the whole reason this is one
// statement. The partial unique index makes the second INSERT conflict, and the
// SELECT that follows returns the first one's row — so they land in the same
// room. Two INSERTs that both succeeded would put them in different rooms,
// where neither can hear the other, which looks like a network fault and is a
// missing index.
//
// Reports whether it created the huddle, because the caller must call
// CreateRoom on the SFU exactly once and must publish exactly one "started"
// event.
func (r *Repository) StartOrJoin(ctx context.Context, workspaceID, scopeType, scopeID, actorID string,
	maxParticipants int) (*Huddle, bool, error) {

	if scopeType != ScopeChannel {
		return nil, false, ErrBadScope
	}

	id := uuid.NewString()
	var created bool
	var out *Huddle

	err := database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO huddles (id, workspace_id, scope_type, scope_id, room_name,
			                     started_by, max_participants)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (scope_type, scope_id) WHERE ended_at IS NULL DO NOTHING`,
			id, workspaceID, scopeType, scopeID, roomName(id), actorID, maxParticipants)
		if err != nil {
			return fmt.Errorf("insert huddle: %w", err)
		}
		created = tag.RowsAffected() == 1

		out, err = scanHuddle(tx.QueryRow(ctx, `
			SELECT `+huddleColumns+` FROM huddles
			 WHERE scope_type = $1 AND scope_id = $2 AND ended_at IS NULL`, scopeType, scopeID))
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return out, created, nil
}

// Live returns the current huddle for a scope, or ErrNotFound.
func (r *Repository) Live(ctx context.Context, scopeType, scopeID string) (*Huddle, error) {
	return scanHuddle(r.pool.QueryRow(ctx, `
		SELECT `+huddleColumns+` FROM huddles
		 WHERE scope_type = $1 AND scope_id = $2 AND ended_at IS NULL`, scopeType, scopeID))
}

func (r *Repository) ByID(ctx context.Context, id string) (*Huddle, error) {
	return scanHuddle(r.pool.QueryRow(ctx, `SELECT `+huddleColumns+` FROM huddles WHERE id = $1`, id))
}

func (r *Repository) ByRoom(ctx context.Context, room string) (*Huddle, error) {
	return scanHuddle(r.pool.QueryRow(ctx, `SELECT `+huddleColumns+` FROM huddles WHERE room_name = $1`, room))
}

// End closes a huddle and every session still open in it.
//
// Conditional on ended_at IS NULL, so the reconciler and a webhook racing to
// end the same call do not produce two different reasons — the first one wins
// and the second is a no-op rather than an overwrite.
func (r *Repository) End(ctx context.Context, id, reason string) (bool, error) {
	var ended bool
	err := database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE huddles SET ended_at = NOW(), ended_reason = $2
			  WHERE id = $1 AND ended_at IS NULL`, id, reason)
		if err != nil {
			return fmt.Errorf("end huddle: %w", err)
		}
		ended = tag.RowsAffected() == 1
		if !ended {
			return nil
		}
		// Sessions left open when the room went away. Without this they stay
		// "live" forever and the roster of a finished call keeps listing
		// everybody who was in it.
		_, err = tx.Exec(ctx,
			`UPDATE huddle_participants SET left_at = NOW(), left_reason = 'room_ended'
			  WHERE huddle_id = $1 AND left_at IS NULL`, id)
		if err != nil {
			return fmt.Errorf("close sessions: %w", err)
		}
		return nil
	})
	return ended, err
}

// Participants is the live roster.
func (r *Repository) Participants(ctx context.Context, huddleID string) ([]Participant, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT huddle_id::text, participant_sid, user_id::text, joined_at, left_at,
		       left_reason, is_screen_sharing
		  FROM huddle_participants
		 WHERE huddle_id = $1 AND left_at IS NULL
		 ORDER BY joined_at`, huddleID)
	if err != nil {
		return nil, fmt.Errorf("query participants: %w", err)
	}
	defer rows.Close()

	out := make([]Participant, 0, 8)
	for rows.Next() {
		var p Participant
		if err := rows.Scan(&p.HuddleID, &p.ParticipantSID, &p.UserID, &p.JoinedAt,
			&p.LeftAt, &p.LeftReason, &p.IsScreenSharing); err != nil {
			return nil, fmt.Errorf("scan participant: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// The webhook sink — the only writer of participant rows
// ---------------------------------------------------------------------------

// Event is one SFU notification, already parsed and verified.
type Event struct {
	// ID is the SFU's event id. It is what makes redelivery safe, and an event
	// without one is REFUSED rather than applied — at-least-once delivery plus
	// no idempotency key is a roster that drifts.
	ID       string
	Type     string
	Room     string
	SID      string
	Identity string
	Screen   bool
	At       time.Time
}

// Apply records one event, exactly once.
//
// The dedupe insert and the state change are in ONE transaction, so "recorded
// the event" and "applied the event" cannot come apart — the failure mode that
// makes a dedupe table worse than none, because a crash between them silently
// drops the event forever.
func (r *Repository) Apply(ctx context.Context, ev Event) error {
	if ev.ID == "" {
		return errors.New("huddle: webhook event has no id, so redelivery cannot be made safe")
	}
	return database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO huddle_webhook_events (event_id, event_type) VALUES ($1, $2)
			 ON CONFLICT (event_id) DO NOTHING`, ev.ID, ev.Type)
		if err != nil {
			return fmt.Errorf("record webhook event: %w", err)
		}
		if tag.RowsAffected() == 0 {
			// Already applied. Not an error — this is the SFU retrying, which
			// it is supposed to do.
			return nil
		}

		h, err := scanHuddle(tx.QueryRow(ctx,
			`SELECT `+huddleColumns+` FROM huddles WHERE room_name = $1`, ev.Room))
		if err != nil {
			// A room we have no record of. Ignored rather than failed: the SFU
			// may host rooms this deployment did not create, and answering 500
			// makes it retry forever.
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			return err
		}

		switch ev.Type {
		case "participant_joined":
			if ev.Identity == "" || ev.SID == "" {
				return nil
			}
			_, err = tx.Exec(ctx, `
				INSERT INTO huddle_participants (huddle_id, participant_sid, user_id, joined_at, is_screen_sharing)
				VALUES ($1, $2, $3, COALESCE($4, NOW()), $5)
				ON CONFLICT (huddle_id, participant_sid) DO NOTHING`,
				h.ID, ev.SID, ev.Identity, nullTime(ev.At), ev.Screen)
			return err

		case "participant_left":
			_, err = tx.Exec(ctx, `
				UPDATE huddle_participants
				   SET left_at = COALESCE($3, NOW()), left_reason = 'client'
				 WHERE huddle_id = $1 AND participant_sid = $2 AND left_at IS NULL`,
				h.ID, ev.SID, nullTime(ev.At))
			return err

		case "track_published", "track_unpublished":
			_, err = tx.Exec(ctx, `
				UPDATE huddle_participants SET is_screen_sharing = $3
				 WHERE huddle_id = $1 AND participant_sid = $2 AND left_at IS NULL`,
				h.ID, ev.SID, ev.Screen)
			return err

		case "room_finished":
			_, err = tx.Exec(ctx,
				`UPDATE huddles SET ended_at = COALESCE($2, NOW()), ended_reason = 'empty'
				  WHERE id = $1 AND ended_at IS NULL`, h.ID, nullTime(ev.At))
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx,
				`UPDATE huddle_participants SET left_at = COALESCE($2, NOW()), left_reason = 'room_ended'
				  WHERE huddle_id = $1 AND left_at IS NULL`, h.ID, nullTime(ev.At))
			return err
		}
		// An event type this build does not handle. Recorded (so a retry is
		// cheap) and otherwise ignored.
		return nil
	})
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// StaleLive lists huddles that are live in the database, oldest first. The
// reconciler asks the SFU about each and ends the ones it has forgotten.
func (r *Repository) StaleLive(ctx context.Context, olderThan time.Duration, limit int) ([]Huddle, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+huddleColumns+` FROM huddles
		 WHERE ended_at IS NULL AND created_at < NOW() - $1::interval
		 ORDER BY created_at
		 LIMIT $2`, fmt.Sprintf("%d seconds", int(olderThan.Seconds())), limit)
	if err != nil {
		return nil, fmt.Errorf("query stale huddles: %w", err)
	}
	defer rows.Close()

	out := make([]Huddle, 0, limit)
	for rows.Next() {
		var h Huddle
		if err := rows.Scan(&h.ID, &h.WorkspaceID, &h.ScopeType, &h.ScopeID, &h.RoomName,
			&h.StartedBy, &h.MaxParticipants, &h.CreatedAt, &h.EndedAt, &h.EndedReason); err != nil {
			return nil, fmt.Errorf("scan huddle: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ReconcileParticipants replaces the live roster with what the SFU reports.
//
// The SFU is authoritative for presence, so a disagreement is resolved in its
// favour without ceremony: sessions it does not list are closed, sessions it
// lists that we do not have are opened. This is what repairs a roster after a
// webhook was lost, which at-least-once delivery does not prevent — it only
// prevents duplicates.
func (r *Repository) ReconcileParticipants(ctx context.Context, huddleID string, live []Participant) error {
	return database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		sids := make([]string, 0, len(live))
		for _, p := range live {
			sids = append(sids, p.ParticipantSID)
			if _, err := tx.Exec(ctx, `
				INSERT INTO huddle_participants (huddle_id, participant_sid, user_id, joined_at, is_screen_sharing)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (huddle_id, participant_sid)
				DO UPDATE SET is_screen_sharing = EXCLUDED.is_screen_sharing`,
				huddleID, p.ParticipantSID, p.UserID, p.JoinedAt, p.IsScreenSharing); err != nil {
				return fmt.Errorf("reconcile participant: %w", err)
			}
		}
		_, err := tx.Exec(ctx, `
			UPDATE huddle_participants SET left_at = NOW(), left_reason = 'reconciled'
			 WHERE huddle_id = $1 AND left_at IS NULL AND NOT (participant_sid = ANY($2))`,
			huddleID, sids)
		return err
	})
}
