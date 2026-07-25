package inbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// Pool exposes the underlying pool for the jobs in cmd/worker that need
// withSingletonLock on the same database. It is not a general escape hatch:
// every query against inbox_* lives in this file.
func (r *Repository) Pool() *pgxpool.Pool { return r.pool }

// itemColumns is the projection every item read shares.
const itemColumns = `id, user_id, workspace_id, subject_type, subject_id, state, top_kind,
	unread_count, event_count, last_event_id, last_actor_id, title, preview,
	first_at, last_at, read_at, done_at, notified_at`

// Deliver writes one event to many recipients in a single transaction, and is
// the only thing in this package that creates rows.
//
// # Why two statements and not one
//
// Coalescing means `unread_count = unread_count + 1`, and an increment is not
// idempotent under at-least-once delivery. The event insert restores the
// property: `ON CONFLICT (id) DO NOTHING ... RETURNING user_id` names exactly
// the recipients for whom this event is NEW, and only those recipients' items
// are touched. A redelivery returns an empty set, moves no counter, and — via
// Delivered.Created — sends no push and publishes no relay frame.
//
// Both statements are in one transaction, so there is no window in which an
// event row exists without its item having been counted. There is also no window
// in the other direction: a crash after the event insert and before the item
// upsert rolls both back, and the redelivery redoes both.
//
// # Why multi-row and not a loop
//
// The previous implementation looped one INSERT per recipient
// (internal/notification's fan-out). At a 500-member @channel that is 500 round
// trips inside cmd/worker's 25-second handler budget. One unnest-fed statement
// per event keeps it flat, and it is the reason MaxRecipients can be as high as
// it is.
//
// # top_kind
//
// Ranked in Go (kindRank), passed in as the set of kinds that outrank the
// incoming one. An existing top_kind in that set survives; anything else,
// including a kind this build has never seen, loses. See kindsRankedAbove.
func (r *Repository) Deliver(ctx context.Context, req Request, recipients []string, now time.Time) ([]Delivered, error) {
	if len(recipients) == 0 {
		return nil, nil
	}

	dataJSON, err := json.Marshal(req.Data)
	if err != nil {
		return nil, &PermanentError{Reason: "inbox: data payload is not encodable", Err: err}
	}
	if req.Data == nil {
		dataJSON = []byte("{}")
	}
	if len(dataJSON) > maxDataBytes {
		return nil, &PermanentError{
			Reason: fmt.Sprintf("inbox: data payload is %d bytes, over the %d-byte cap", len(dataJSON), maxDataBytes),
		}
	}

	body := truncate(req.Body, previewRunes)
	key := eventKey(req.ObjectID, req.Discriminator)

	eventIDs := make([]string, len(recipients))
	for i, uid := range recipients {
		eventIDs[i] = EventID(req.Kind, uid, key)
	}

	var actor *string
	if req.ActorID != "" {
		a := req.ActorID
		actor = &a
	}

	created := map[string]bool{}
	items := map[string]*Item{}

	err = database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			INSERT INTO inbox_events (
			    id, user_id, workspace_id, kind, object_type, object_id,
			    subject_type, subject_id, actor_id, title, body, data, created_at)
			SELECT e.id, e.user_id, $3::uuid, $4, $5, $6::uuid, $7, $8::uuid, $9::uuid, $10, $11, $12::jsonb, $13
			  FROM unnest($1::uuid[], $2::uuid[]) AS e(id, user_id)
			ON CONFLICT (id) DO NOTHING
			RETURNING user_id`,
			eventIDs, recipients, req.WorkspaceID, req.Kind, req.ObjectType, req.ObjectID,
			req.SubjectType, req.SubjectID, actor, req.Title, body, string(dataJSON), now)
		if err != nil {
			return fmt.Errorf("insert inbox events: %w", err)
		}
		var fresh []string
		for rows.Next() {
			var uid string
			if err := rows.Scan(&uid); err != nil {
				rows.Close()
				return fmt.Errorf("scan inserted inbox event: %w", err)
			}
			fresh = append(fresh, uid)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("insert inbox events: %w", err)
		}
		if len(fresh) == 0 {
			// Every recipient is a redelivery. Nothing to upsert, nothing to buzz.
			return nil
		}

		freshItemIDs := make([]string, len(fresh))
		freshEventIDs := make([]string, len(fresh))
		for i, uid := range fresh {
			freshItemIDs[i] = ItemID(uid, req.SubjectType, req.SubjectID)
			freshEventIDs[i] = EventID(req.Kind, uid, key)
			created[uid] = true
		}

		itemRows, err := tx.Query(ctx, `
			INSERT INTO inbox_items (
			    id, user_id, workspace_id, subject_type, subject_id, state, top_kind,
			    unread_count, event_count, last_event_id, last_actor_id, title, preview,
			    first_at, last_at)
			SELECT n.id, n.user_id, $3::uuid, $4, $5::uuid, 'unread', $6,
			       1, 1, n.event_id, $7::uuid, $8, $9, $10, $10
			  FROM unnest($1::uuid[], $2::uuid[], $11::uuid[]) AS n(id, user_id, event_id)
			ON CONFLICT (user_id, subject_type, subject_id) DO UPDATE SET
			    state         = 'unread',
			    unread_count  = inbox_items.unread_count + 1,
			    event_count   = inbox_items.event_count  + 1,
			    last_event_id = EXCLUDED.last_event_id,
			    last_actor_id = EXCLUDED.last_actor_id,
			    last_at       = EXCLUDED.last_at,
			    title         = EXCLUDED.title,
			    preview       = EXCLUDED.preview,
			    top_kind      = CASE WHEN inbox_items.top_kind = ANY($12::text[])
			                         THEN inbox_items.top_kind ELSE EXCLUDED.top_kind END,
			    -- A new event makes the item mailable again, and reopens it: a
			    -- 'done' item that gets a new event is not done any more.
			    notified_at   = NULL,
			    read_at       = NULL,
			    done_at       = NULL
			RETURNING `+itemColumns,
			freshItemIDs, fresh, req.WorkspaceID, req.SubjectType, req.SubjectID, req.Kind,
			actor, req.Title, body, now, freshEventIDs, kindsRankedAbove(req.Kind))
		if err != nil {
			return fmt.Errorf("upsert inbox items: %w", err)
		}
		defer itemRows.Close()
		for itemRows.Next() {
			it, err := scanItem(itemRows)
			if err != nil {
				return err
			}
			items[it.UserID] = it
		}
		return itemRows.Err()
	})
	if err != nil {
		return nil, err
	}

	// Unread badges are read after the commit, not inside it: the badge is a
	// COUNT over the whole user, which would make the fan-out transaction hold
	// row locks while scanning an unrelated index.
	out := make([]Delivered, 0, len(recipients))
	for _, uid := range recipients {
		d := Delivered{UserID: uid, Created: created[uid], Item: items[uid]}
		out = append(out, d)
	}
	return out, nil
}

// UnreadItems is THE badge. One definition, written through from every path that
// changes it, and the one the reconciler verifies against inbox_events.
func (r *Repository) UnreadItems(ctx context.Context, userID string) (int, error) {
	var n int
	if err := r.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM inbox_items WHERE user_id = $1 AND state = 'unread'`, userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count unread inbox items: %w", err)
	}
	return n, nil
}

// WorkspaceUnread breaks the badge down per workspace for the shell's switcher.
func (r *Repository) WorkspaceUnread(ctx context.Context, userID string) (map[string]int, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT workspace_id, COUNT(*) FROM inbox_items
		  WHERE user_id = $1 AND state = 'unread' GROUP BY workspace_id`, userID)
	if err != nil {
		return nil, fmt.Errorf("count unread inbox items by workspace: %w", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var ws string
		var n int
		if err := rows.Scan(&ws, &n); err != nil {
			return nil, fmt.Errorf("scan workspace unread: %w", err)
		}
		out[ws] = n
	}
	return out, rows.Err()
}

// ListFilter is the query behind GET /api/v1/inbox.
type ListFilter struct {
	UserID string
	// State is 'unread', 'open' (the default: state <> 'done'), 'done' or 'all'.
	State       string
	WorkspaceID string
	Kind        string
}

// ListItems pages by the total (last_at, id) key, as every other list in this
// tree does. Ordering on last_at alone is not total — one fan-out writes a whole
// burst at one timestamp — so a strictly-less-than filter on it would drop every
// row but one at a page boundary.
func (r *Repository) ListItems(ctx context.Context, f ListFilter, cursor httputil.Cursor, limit int) ([]*Item, error) {
	sql := `SELECT ` + itemColumns + ` FROM inbox_items WHERE user_id = $1`
	args := []any{f.UserID}

	switch f.State {
	case StateUnread, StateRead, StateDone:
		args = append(args, f.State)
		sql += fmt.Sprintf(" AND state = $%d", len(args))
	case "all":
	default: // "open" and anything unrecognised
		sql += " AND state <> 'done'"
	}
	if f.WorkspaceID != "" {
		args = append(args, f.WorkspaceID)
		sql += fmt.Sprintf(" AND workspace_id = $%d", len(args))
	}
	if f.Kind != "" {
		args = append(args, f.Kind)
		sql += fmt.Sprintf(" AND top_kind = $%d", len(args))
	}
	if !cursor.IsZero() {
		args = append(args, cursor.CreatedAt, cursor.ID)
		sql += fmt.Sprintf(" AND (last_at, id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, limit)
	sql += fmt.Sprintf(" ORDER BY last_at DESC, id DESC LIMIT $%d", len(args))

	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list inbox items: %w", err)
	}
	defer rows.Close()

	items := []*Item{}
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// GetItem returns one item, or (nil, nil) when it does not exist or belongs to
// somebody else. Ownership is the whole authorization story for an inbox item —
// there is no sharing model and no admin read — so it is a predicate rather than
// a separate check.
func (r *Repository) GetItem(ctx context.Context, id, userID string) (*Item, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+itemColumns+` FROM inbox_items WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return nil, fmt.Errorf("get inbox item: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	return scanItem(rows)
}

// ListEvents pages the events behind one item, newest first, scoped to the
// caller by the same ownership predicate GetItem uses.
func (r *Repository) ListEvents(ctx context.Context, item *Item, cursor httputil.Cursor, limit int) ([]*Event, error) {
	sql := `SELECT id, user_id, workspace_id, kind, object_type, object_id,
	               subject_type, subject_id, actor_id, title, body, data::text, created_at
	          FROM inbox_events
	         WHERE user_id = $1 AND subject_type = $2 AND subject_id = $3`
	args := []any{item.UserID, item.SubjectType, item.SubjectID}
	if !cursor.IsZero() {
		args = append(args, cursor.CreatedAt, cursor.ID)
		sql += fmt.Sprintf(" AND (created_at, id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, limit)
	sql += fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d", len(args))

	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list inbox events: %w", err)
	}
	defer rows.Close()

	events := []*Event{}
	for rows.Next() {
		e := &Event{}
		var raw string
		if err := rows.Scan(&e.ID, &e.UserID, &e.WorkspaceID, &e.Kind, &e.ObjectType, &e.ObjectID,
			&e.SubjectType, &e.SubjectID, &e.ActorID, &e.Title, &e.Body, &raw, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan inbox event: %w", err)
		}
		if err := json.Unmarshal([]byte(raw), &e.Data); err != nil {
			// Non-fatal: the event is still listable, it just cannot deep-link.
			e.Data = nil
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// SetState moves one item between unread / read / done.
//
// Every mutation is `WHERE id = $1 AND user_id = $2` — ownership enforced in the
// UPDATE rather than by a preceding read, so there is no window between the
// check and the write and no second query to forget.
//
// unread_count is deliberately NOT reset to zero on read. It is the number of
// events behind the item, which is what the list renders ("40 new messages"), and
// resetting it would make the reconciler's recount from inbox_events disagree
// with the item on every row anybody had ever read. `state` is what the badge
// counts; the two are different questions.
func (r *Repository) SetState(ctx context.Context, id, userID, state string) (bool, error) {
	var sql string
	switch state {
	case StateUnread:
		sql = `UPDATE inbox_items SET state = 'unread', read_at = NULL, done_at = NULL
		        WHERE id = $1 AND user_id = $2`
	case StateRead:
		sql = `UPDATE inbox_items SET state = 'read', read_at = NOW(), done_at = NULL
		        WHERE id = $1 AND user_id = $2`
	case StateDone:
		sql = `UPDATE inbox_items SET state = 'done', done_at = NOW(),
		           read_at = COALESCE(read_at, NOW())
		        WHERE id = $1 AND user_id = $2`
	default:
		return false, fmt.Errorf("inbox: unknown state %q", state)
	}
	tag, err := r.pool.Exec(ctx, sql, id, userID)
	if err != nil {
		return false, fmt.Errorf("set inbox item state: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ReadAll marks every open item read, optionally narrowed to one workspace or
// one kind. It reports how many rows moved.
func (r *Repository) ReadAll(ctx context.Context, userID, workspaceID, kind string) (int64, error) {
	sql := `UPDATE inbox_items SET state = 'read', read_at = NOW()
	         WHERE user_id = $1 AND state = 'unread'`
	args := []any{userID}
	if workspaceID != "" {
		args = append(args, workspaceID)
		sql += fmt.Sprintf(" AND workspace_id = $%d", len(args))
	}
	if kind != "" {
		args = append(args, kind)
		sql += fmt.Sprintf(" AND top_kind = $%d", len(args))
	}
	tag, err := r.pool.Exec(ctx, sql, args...)
	if err != nil {
		return 0, fmt.Errorf("mark all inbox items read: %w", err)
	}
	return tag.RowsAffected(), nil
}

// MarkSubjectRead marks one user's item for one subject read, inside a caller's
// transaction.
//
// This is the single overlap between this package and the channel unread badge,
// and it is why the two can be trusted to agree. internal/channel's read-marker
// handler calls it in the SAME transaction that advances channel_members
// .last_read_at, so there is no interval during which a user has read #alerts
// and the inbox bell still says 1. Everything else the two systems count is
// disjoint by construction: a plain channel message never produces an inbox
// item, and a directed event never moves the channel read marker.
func MarkSubjectRead(ctx context.Context, tx pgx.Tx, userID, subjectType, subjectID string) error {
	if _, err := tx.Exec(ctx,
		`UPDATE inbox_items SET state = 'read', read_at = NOW()
		  WHERE user_id = $1 AND subject_type = $2 AND subject_id = $3 AND state = 'unread'`,
		userID, subjectType, subjectID); err != nil {
		return fmt.Errorf("mark inbox items read for %s %s: %w", subjectType, subjectID, err)
	}
	return nil
}

// --- digest ------------------------------------------------------------------

// DigestCandidate is one item eligible for a digest mail.
type DigestCandidate struct {
	ItemID      string
	UserID      string
	UserEmail   string
	UserName    string
	WorkspaceID string
	WorkspaceNm string
	Kind        string
	Title       string
	Preview     string
	SubjectType string
	SubjectID   string
	UnreadCount int
	LastAt      time.Time
	// PrivateSubject suppresses the preview. A digest sits in an external
	// mailbox forever and access can be revoked between claim and delivery, so
	// nothing quoted from a private channel goes into one.
	PrivateSubject bool
}

// DigestCandidates selects unread, never-mailed items that have been quiet for
// at least quiet, for users whose last digest is older than minInterval.
//
// The quiet period is what collapses a burst: an item still receiving events is
// not yet mailed, and any new event resets notified_at to NULL and last_at to
// now, pushing it back out of the window.
func (r *Repository) DigestCandidates(
	ctx context.Context, quiet, minInterval time.Duration, limit int,
) ([]DigestCandidate, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT i.id, i.user_id, u.email, COALESCE(NULLIF(u.full_name, ''), u.username),
		       i.workspace_id, w.name, i.top_kind, i.title, i.preview,
		       i.subject_type, i.subject_id, i.unread_count, i.last_at,
		       COALESCE(c.type <> 'public', FALSE)
		  FROM inbox_items i
		  JOIN users u      ON u.id = i.user_id
		  JOIN workspaces w ON w.id = i.workspace_id
		  LEFT JOIN inbox_digest_state d ON d.user_id = i.user_id AND d.workspace_id = i.workspace_id
		  LEFT JOIN channels c ON i.subject_type = 'channel' AND c.id = i.subject_id
		 WHERE i.state = 'unread'
		   AND i.notified_at IS NULL
		   AND i.last_at < NOW() - $1::interval
		   AND (d.last_sent_at IS NULL OR d.last_sent_at < NOW() - $2::interval)
		   AND u.is_active = TRUE
		 ORDER BY i.user_id, i.workspace_id, i.last_at DESC
		 LIMIT $3`,
		quiet, minInterval, limit)
	if err != nil {
		return nil, fmt.Errorf("select digest candidates: %w", err)
	}
	defer rows.Close()

	out := []DigestCandidate{}
	for rows.Next() {
		var c DigestCandidate
		if err := rows.Scan(&c.ItemID, &c.UserID, &c.UserEmail, &c.UserName,
			&c.WorkspaceID, &c.WorkspaceNm, &c.Kind, &c.Title, &c.Preview,
			&c.SubjectType, &c.SubjectID, &c.UnreadCount, &c.LastAt, &c.PrivateSubject); err != nil {
			return nil, fmt.Errorf("scan digest candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ClaimDigest marks a batch of items as mailed and records the send against the
// per-(user, workspace) rate ceiling, in one transaction.
//
// Claim-then-send, deliberately. If the publish fails after this commits, that
// digest is lost and logged. The alternative — send, then claim — loses the
// crash window in the other direction and mails the same digest twice. Losing
// one digest beats mailing a company twice, and the ordering looks arbitrary
// unless that is written down.
func (r *Repository) ClaimDigest(ctx context.Context, userID, workspaceID string, itemIDs []string) error {
	return database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`UPDATE inbox_items SET notified_at = NOW() WHERE id = ANY($1) AND user_id = $2`,
			itemIDs, userID); err != nil {
			return fmt.Errorf("claim digest items: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO inbox_digest_state (user_id, workspace_id, last_sent_at)
			 VALUES ($1, $2, NOW())
			 ON CONFLICT (user_id, workspace_id) DO UPDATE SET last_sent_at = NOW()`,
			userID, workspaceID); err != nil {
			return fmt.Errorf("record digest send: %w", err)
		}
		return nil
	})
}

// --- reconciliation ----------------------------------------------------------

// Drift is one item whose stored counters disagree with the events behind it.
type Drift struct {
	ItemID      string
	UserID      string
	StoredCount int
	ActualCount int
	// Orphan is an item with no events at all — the shape a partial rollback or
	// a hand-written row leaves behind.
	Orphan bool
}

// ReconcileUsers recomputes unread_count and event_count from inbox_events for
// every item belonging to a slice of users, reports what disagreed, and repairs
// it.
//
// This exists for the same reason internal/authz has a drift job: inbox_items is
// denormalized state that answers a question the user asked out loud. A counter
// that silently drifts is not a stale cache, it is a product that lies, and the
// user's response to a bell that says 1 over an empty list is to stop trusting
// the bell — after which every pillar that files into it is wasted work.
//
// It repairs as well as reports, and says exactly what it changed. Verify-only
// was considered and rejected for the reason cmd/worker's runACLDrift already
// documents: a watchdog that is permanently red is a watchdog nobody reads.
//
// Note what it does NOT recompute: `state`. Read/done are decisions the user
// made and no amount of event history can re-derive them. Only the counts, and
// the one structural case where state is provably wrong — an item with zero
// events cannot be unread — are touched.
func (r *Repository) ReconcileUsers(ctx context.Context, userIDs []string, sampleLimit int) ([]Drift, int, error) {
	if len(userIDs) == 0 {
		return nil, 0, nil
	}

	var drifts []Drift
	var repaired int

	err := database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH actual AS (
			    SELECT i.id,
			           i.user_id,
			           i.event_count AS stored,
			           (SELECT COUNT(*) FROM inbox_events e
			             WHERE e.user_id = i.user_id
			               AND e.subject_type = i.subject_type
			               AND e.subject_id = i.subject_id) AS n
			      FROM inbox_items i
			     WHERE i.user_id = ANY($1)
			       FOR UPDATE OF i
			)
			SELECT id, user_id, stored, n FROM actual WHERE stored <> n`,
			userIDs)
		if err != nil {
			return fmt.Errorf("recount inbox items: %w", err)
		}
		var ids []string
		for rows.Next() {
			var d Drift
			if err := rows.Scan(&d.ItemID, &d.UserID, &d.StoredCount, &d.ActualCount); err != nil {
				rows.Close()
				return fmt.Errorf("scan inbox drift: %w", err)
			}
			d.Orphan = d.ActualCount == 0
			ids = append(ids, d.ItemID)
			if len(drifts) < sampleLimit {
				drifts = append(drifts, d)
			}
			repaired++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("recount inbox items: %w", err)
		}
		if len(ids) == 0 {
			return nil
		}

		// Repair. unread_count is clamped to the recomputed total rather than
		// set to it: an item the user has partially read has fewer unread events
		// than events, and that difference is real information the recount
		// cannot reproduce.
		if _, err := tx.Exec(ctx, `
			UPDATE inbox_items i
			   SET event_count  = a.n,
			       unread_count = LEAST(i.unread_count, a.n),
			       state = CASE WHEN a.n = 0 THEN 'read' ELSE i.state END
			  FROM (
			      SELECT i2.id,
			             (SELECT COUNT(*) FROM inbox_events e
			               WHERE e.user_id = i2.user_id
			                 AND e.subject_type = i2.subject_type
			                 AND e.subject_id = i2.subject_id) AS n
			        FROM inbox_items i2 WHERE i2.id = ANY($1)
			  ) a
			 WHERE i.id = a.id`, ids); err != nil {
			return fmt.Errorf("repair inbox items: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return drifts, repaired, nil
}

// UserSliceAfter returns up to limit user ids that own inbox items, in id order
// starting after `after`. The reconciler rotates through the whole population a
// slice at a time rather than recomputing everything on every tick.
func (r *Repository) UserSliceAfter(ctx context.Context, after string, limit int) ([]string, error) {
	sql := `SELECT DISTINCT user_id FROM inbox_items`
	args := []any{}
	if after != "" {
		args = append(args, after)
		sql += " WHERE user_id > $1"
	}
	args = append(args, limit)
	sql += fmt.Sprintf(" ORDER BY user_id LIMIT $%d", len(args))

	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list inbox users: %w", err)
	}
	defer rows.Close()

	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan inbox user: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// --- scanning ----------------------------------------------------------------

func scanItem(rows pgx.Rows) (*Item, error) {
	it := &Item{}
	if err := rows.Scan(&it.ID, &it.UserID, &it.WorkspaceID, &it.SubjectType, &it.SubjectID,
		&it.State, &it.TopKind, &it.UnreadCount, &it.EventCount, &it.LastEventID, &it.LastActorID,
		&it.Title, &it.Preview, &it.FirstAt, &it.LastAt, &it.ReadAt, &it.DoneAt, &it.NotifiedAt); err != nil {
		return nil, fmt.Errorf("scan inbox item: %w", err)
	}
	return it, nil
}
