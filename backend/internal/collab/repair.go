package collab

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Projection repair.
//
// THE SERVER CANNOT PRODUCE A PROJECTION. It never interprets a CRDT update, so
// a document whose stored text sits behind its log can only be repaired by a
// client that has the document in memory. Three paths do that — the editor's
// debounce, its flush on unmount, and its catch-up on open — and every one of
// them needs somebody to be there.
//
// This is the fourth, for the document that was edited, closed by a browser
// that was killed before it could flush, and then never opened again: stale in
// search, with nothing else in the system able to notice.

// StaleProjection is one document whose stored projection is behind its log.
type StaleProjection struct {
	DocumentID    string
	HeadSeq       int64
	ProjectionSeq int64
}

// Gap is how far behind the projection is.
func (s StaleProjection) Gap() int64 { return s.HeadSeq - s.ProjectionSeq }

// FindStaleProjections lists documents worth asking a client to re-project.
//
// THE LEFT JOIN IS THE POINT. A document that has never been projected has no
// row in file_projections at all, and an inner join would hide exactly the
// documents that are most wrong — the ones with no searchable text whatsoever.
//
// `minAge` excludes documents that are merely mid-edit: a document being typed
// in is always somewhat behind its log, and asking it to project is work the
// debounce two seconds away was about to do anyway.
func FindStaleProjections(ctx context.Context, pool *pgxpool.Pool,
	minGap int64, minAge time.Duration, limit int) ([]StaleProjection, error) {

	rows, err := pool.Query(ctx, `
		SELECT d.id::text, d.head_seq, COALESCE(p.projection_seq, 0)
		  FROM collab_documents d
		  LEFT JOIN file_projections p ON p.file_id = d.resource_id
		 WHERE d.head_seq - COALESCE(p.projection_seq, 0) >= $1
		   AND d.updated_at < NOW() - $2::interval
		 ORDER BY d.updated_at
		 LIMIT $3`, minGap, minAge.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("scan stale projections: %w", err)
	}
	defer rows.Close()

	var out []StaleProjection
	for rows.Next() {
		var s StaleProjection
		if err := rows.Scan(&s.DocumentID, &s.HeadSeq, &s.ProjectionSeq); err != nil {
			return nil, fmt.Errorf("scan stale projection: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
