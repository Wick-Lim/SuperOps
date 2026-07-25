package comment

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
)

// DBTitler names a comment target by reading whatever table owns it.
//
// It is the ONE place in this package that knows a file is a file, and it is
// deliberately a lookup table rather than an interface each pillar implements:
// a pillar that had to register a titler would be a pillar that can forget to,
// and the failure is a notification reading "somebody commented on 5d3f…".
//
// An unregistered type falls back to the type name. Worse than a title, much
// better than a uuid, and it never errors — a missing title must not stop a
// notification that is otherwise correct.
type DBTitler struct {
	pool *pgxpool.Pool
}

func NewTitler(pool *pgxpool.Pool) *DBTitler { return &DBTitler{pool: pool} }

// titleQueries maps an acl_object type to the query that names it. Adding a
// commentable type is one line here.
var titleQueries = map[string]string{
	authz.TypeFile:    `SELECT name FROM files WHERE id = $1`,
	authz.TypeFolder:  `SELECT name FROM drive_folders WHERE id = $1`,
	authz.TypeChannel: `SELECT name FROM channels WHERE id = $1`,
}

func (t *DBTitler) Title(ctx context.Context, obj authz.ObjectRef) (string, error) {
	query, ok := titleQueries[obj.Type]
	if !ok {
		return obj.Type, nil
	}
	var name string
	if err := t.pool.QueryRow(ctx, query, obj.ID).Scan(&name); err != nil {
		// Best effort by design: a title that could not be read degrades the
		// wording, and refusing to notify because of it would be absurd.
		return obj.Type, nil
	}
	return name, nil
}
