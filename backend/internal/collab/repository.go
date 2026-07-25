package collab

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/ws"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
)

// Repository owns the update log. It is the only thing in the tree that writes
// collab_updates or collab_snapshots.
type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

const documentColumns = `id, workspace_id, resource_type, resource_id, head_seq, snapshot_seq, created_by, created_at, updated_at`

func scanDocument(row pgx.Row) (*Document, error) {
	var d Document
	err := row.Scan(&d.ID, &d.WorkspaceID, &d.ResourceType, &d.ResourceID,
		&d.HeadSeq, &d.SnapshotSeq, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan collaboration document: %w", err)
	}
	return &d, nil
}

// Get returns (nil, nil) when the document does not exist.
func (r *Repository) Get(ctx context.Context, documentID string) (*Document, error) {
	return scanDocument(r.pool.QueryRow(ctx,
		`SELECT `+documentColumns+` FROM collab_documents WHERE id = $1`, documentID))
}

// GetByResource returns the document for a Drive object, or (nil, nil).
func (r *Repository) GetByResource(ctx context.Context, resourceType, resourceID string) (*Document, error) {
	return scanDocument(r.pool.QueryRow(ctx,
		`SELECT `+documentColumns+` FROM collab_documents
		  WHERE resource_type = $1 AND resource_id = $2`, resourceType, resourceID))
}

// EnsureDocument returns the document for an object, creating it on first open.
//
// Opening a file is the only trigger there is — an editor is a handler
// dispatched on file type (ROADMAP §3b), and nothing knows a document should
// exist before someone opens it. The insert is ON CONFLICT DO NOTHING followed
// by a read rather than a read followed by an insert, because two people
// opening the same file at the same moment is the normal case, not the edge.
func (r *Repository) EnsureDocument(ctx context.Context, workspaceID, resourceType, resourceID, createdBy string) (*Document, error) {
	doc, err := scanDocument(r.pool.QueryRow(ctx,
		`INSERT INTO collab_documents (workspace_id, resource_type, resource_id, created_by)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (resource_type, resource_id) DO NOTHING
		 RETURNING `+documentColumns,
		workspaceID, resourceType, resourceID, nullableID(createdBy)))
	if err != nil {
		return nil, err
	}
	if doc != nil {
		return doc, nil
	}

	// Lost the insert race, or the document already existed.
	doc, err = r.GetByResource(ctx, resourceType, resourceID)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		// The conflicting row was deleted between the two statements. Rare
		// enough to report rather than loop: the caller retries the open.
		return nil, fmt.Errorf("create collaboration document: lost insert race for %s/%s", resourceType, resourceID)
	}
	if doc.WorkspaceID != workspaceID {
		// The object is claimed by another workspace. Returning it would let a
		// caller reach across tenants by guessing a resource id.
		return nil, ErrResourceConflict
	}
	return doc, nil
}

// nullableID renders an empty id as SQL NULL, so an unattributed write does not
// fail the users(id) foreign key.
func nullableID(id string) interface{} {
	if id == "" {
		return nil
	}
	return id
}

// AppendUpdate writes one opaque update to the log and returns the sequence
// number it was given, plus the document's current snapshot position (which the
// caller needs to decide whether compaction is due — fetching it here costs
// nothing because the row is already locked).
//
// The UPDATE ... RETURNING is the first statement on purpose: its row lock is
// what serialises appends to one document, and that is what makes the log
// gap-free and what makes compaction safe. See the package documentation.
func (r *Repository) AppendUpdate(ctx context.Context, documentID, actorID string, payload []byte) (seq, snapshotSeq int64, err error) {
	err = database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`UPDATE collab_documents
			    SET head_seq = head_seq + 1, updated_at = NOW()
			  WHERE id = $1
			  RETURNING head_seq, snapshot_seq`, documentID,
		).Scan(&seq, &snapshotSeq)
		if errors.Is(err, pgx.ErrNoRows) {
			return ws.ErrRoomNotFound
		}
		if err != nil {
			return fmt.Errorf("advance collaboration log head: %w", err)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO collab_updates (document_id, seq, actor_id, payload)
			 VALUES ($1, $2, $3, $4)`,
			documentID, seq, nullableID(actorID), payload,
		); err != nil {
			return fmt.Errorf("append collaboration update: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return seq, snapshotSeq, nil
}

// Load reads everything a client needs to catch up from `since`.
//
// The statement order matters and is not the obvious one: the updates are read
// BEFORE the snapshot. Read the other way round, a compaction committing
// between the two reads would advance the snapshot past updates this call has
// not read yet and delete them, and the caller would be handed a state with a
// hole in it. Reading updates first means a snapshot that arrives late can only
// ever cover MORE than the caller has already been given, never less.
func (r *Repository) Load(ctx context.Context, documentID string, since int64, limit int) (*State, error) {
	if limit <= 0 || limit > maxStateUpdates {
		limit = maxStateUpdates
	}

	doc, err := r.Get(ctx, documentID)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, ws.ErrRoomNotFound
	}

	// One extra row is fetched purely to detect truncation.
	rows, err := r.pool.Query(ctx,
		`SELECT seq, actor_id, payload, created_at
		   FROM collab_updates
		  WHERE document_id = $1 AND seq > $2
		  ORDER BY seq
		  LIMIT $3`, documentID, since, limit+1)
	if err != nil {
		return nil, fmt.Errorf("list collaboration updates: %w", err)
	}
	defer rows.Close()

	updates := []Update{}
	for rows.Next() {
		var u Update
		if err := rows.Scan(&u.Seq, &u.ActorID, &u.Payload, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan collaboration update: %w", err)
		}
		updates = append(updates, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list collaboration updates: %w", err)
	}

	hasMore := len(updates) > limit
	if hasMore {
		updates = updates[:limit]
	}

	state := &State{
		DocumentID: documentID,
		Updates:    updates,
		ThroughSeq: since,
		HeadSeq:    doc.HeadSeq,
		HasMore:    hasMore,
	}

	var snapSeq int64
	var snapPayload []byte
	err = r.pool.QueryRow(ctx,
		`SELECT seq, payload FROM collab_snapshots
		  WHERE document_id = $1 ORDER BY seq DESC LIMIT 1`, documentID,
	).Scan(&snapSeq, &snapPayload)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("get collaboration snapshot: %w", err)
	}

	if err == nil && snapSeq > since {
		// The caller's watermark predates the snapshot, so it needs the
		// snapshot and only the updates the snapshot does not already contain.
		state.Snapshot = snapPayload
		state.SnapshotSeq = snapSeq
		state.ThroughSeq = snapSeq
		state.Updates = dropThrough(state.Updates, snapSeq)
	}

	for _, u := range state.Updates {
		if u.Seq > state.ThroughSeq {
			state.ThroughSeq = u.Seq
		}
	}
	return state, nil
}

// dropThrough removes the updates a snapshot already covers. The slice is
// ordered by seq, so this is a prefix.
func dropThrough(updates []Update, seq int64) []Update {
	for i, u := range updates {
		if u.Seq > seq {
			return updates[i:]
		}
	}
	return []Update{}
}

// SaveSnapshot stores a client-produced snapshot and compacts everything it
// covers, returning how many log rows were removed.
//
// The SELECT ... FOR UPDATE is what makes this safe against a concurrent
// append: it takes the same row lock the append path takes as its first
// statement, so head_seq cannot move and no in-flight update can be missed
// while this transaction decides what to delete. A snapshot that lost the race
// (another compaction got there first, or it claims updates that do not exist)
// is refused as stale and deletes nothing.
func (r *Repository) SaveSnapshot(ctx context.Context, documentID string, throughSeq int64, actorID string, payload []byte) (int64, error) {
	if throughSeq <= 0 {
		return 0, ErrStaleSnapshot
	}

	var compacted int64
	err := database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		var headSeq, snapshotSeq int64
		err := tx.QueryRow(ctx,
			`SELECT head_seq, snapshot_seq FROM collab_documents WHERE id = $1 FOR UPDATE`,
			documentID,
		).Scan(&headSeq, &snapshotSeq)
		if errors.Is(err, pgx.ErrNoRows) {
			return ws.ErrRoomNotFound
		}
		if err != nil {
			return fmt.Errorf("lock collaboration document: %w", err)
		}
		if throughSeq <= snapshotSeq || throughSeq > headSeq {
			return ErrStaleSnapshot
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO collab_snapshots (document_id, seq, actor_id, payload)
			 VALUES ($1, $2, $3, $4)`,
			documentID, throughSeq, nullableID(actorID), payload,
		); err != nil {
			return fmt.Errorf("store collaboration snapshot: %w", err)
		}

		if _, err := tx.Exec(ctx,
			`UPDATE collab_documents SET snapshot_seq = $2, updated_at = NOW() WHERE id = $1`,
			documentID, throughSeq,
		); err != nil {
			return fmt.Errorf("advance collaboration snapshot position: %w", err)
		}

		tag, err := tx.Exec(ctx,
			`DELETE FROM collab_updates WHERE document_id = $1 AND seq <= $2`,
			documentID, throughSeq)
		if err != nil {
			return fmt.Errorf("compact collaboration log: %w", err)
		}
		compacted = tag.RowsAffected()

		// Keep a short history: a snapshot comes from a client, so the previous
		// good one is the only thing standing between a bad client and a
		// destroyed document.
		if _, err := tx.Exec(ctx,
			`DELETE FROM collab_snapshots
			  WHERE document_id = $1
			    AND seq NOT IN (
			        SELECT seq FROM collab_snapshots
			         WHERE document_id = $1 ORDER BY seq DESC LIMIT $2)`,
			documentID, snapshotRetention,
		); err != nil {
			return fmt.Errorf("prune collaboration snapshots: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return compacted, nil
}

// DeleteDocument removes a document and, by cascade, its log and snapshots.
// Callers must revoke the room as well, or the sockets already in it keep
// relaying updates to each other for a document that no longer exists.
func (r *Repository) DeleteDocument(ctx context.Context, documentID string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `DELETE FROM collab_documents WHERE id = $1`, documentID)
	if err != nil {
		return false, fmt.Errorf("delete collaboration document: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
