package issue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
)

type Repository struct {
	pool  *pgxpool.Pool
	authz *authz.Checker
}

func NewRepository(pool *pgxpool.Pool, az *authz.Checker) *Repository {
	return &Repository{pool: pool, authz: az}
}

// ---------------------------------------------------------------------------
// Projects
// ---------------------------------------------------------------------------

const projectColumns = `p.id::text, p.workspace_id::text, p.name, p.key, p.description,
	p.archived_at, p.created_by::text, p.created_at, p.updated_at`

func scanProject(row pgx.Row) (*Project, error) {
	var p Project
	err := row.Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.Key, &p.Description,
		&p.ArchivedAt, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan project: %w", err)
	}
	return &p, nil
}

// CreateProject writes a project and everything that makes it usable, in ONE
// transaction:
//
//	the projects row
//	its issue counter
//	its five default states — a project with none is a project nothing can be
//	  created in, so "create a project" would be an incomplete action
//	the acl_object row (RegisterTx), because a project is ACL-NATIVE
//	the workspace grant, which is what makes it visible to anybody
//
// The last two are the shape docs/plans/README.md ruling 5 requires: deferring
// the ACL to the hourly job means the project is missing from its own listing
// until that job runs, and then appears on its own.
func (r *Repository) CreateProject(ctx context.Context, workspaceID, name, key, description, actorID string) (*Project, error) {
	name = strings.TrimSpace(name)
	key = strings.ToUpper(strings.TrimSpace(key))
	if name == "" {
		return nil, fmt.Errorf("%w: a project needs a name", ErrBadInput)
	}
	if !projectKeyShape.MatchString(key) {
		return nil, fmt.Errorf("%w: a project key is 2-10 characters, starting with a letter, "+
			"uppercase letters and digits only", ErrBadInput)
	}

	var out *Project
	err := database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		var id string
		err := tx.QueryRow(ctx, `
			INSERT INTO projects (workspace_id, name, key, description, created_by)
			VALUES ($1, $2, $3, $4, $5) RETURNING id::text`,
			workspaceID, name, key, description, nullable(actorID)).Scan(&id)
		if err != nil {
			if strings.Contains(err.Error(), "idx_projects_key") {
				return ErrDuplicateKey
			}
			return fmt.Errorf("create project: %w", err)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO issue_counters (project_id) VALUES ($1)`, id); err != nil {
			return fmt.Errorf("create issue counter: %w", err)
		}

		for i, s := range DefaultStates {
			if _, err := tx.Exec(ctx, `
				INSERT INTO issue_states (project_id, name, category, position, color)
				VALUES ($1, $2, $3, $4, $5)`, id, s.Name, s.Category, i, s.Color); err != nil {
				return fmt.Errorf("create default state %q: %w", s.Name, err)
			}
		}

		if err := r.authz.RegisterTx(ctx, tx,
			authz.ProjectObject(id), authz.WorkspaceObject(workspaceID)); err != nil {
			return fmt.Errorf("register project: %w", err)
		}
		// Admin, capped by role: grantedCapability bounds a workspace grant at
		// what the recipient's role is worth, so this resolves to admin for an
		// owner, write for a member and read for a guest. Granting `write` would
		// flatten everyone at write and then nobody could ever share a project
		// outward, because `share` sits above it.
		if err := r.authz.GrantTx(ctx, tx, authz.UserSubject(actorID),
			authz.WorkspaceSubject(workspaceID), authz.ProjectObject(id), authz.CapAdmin); err != nil {
			return fmt.Errorf("share project with the workspace: %w", err)
		}

		out, err = scanProject(tx.QueryRow(ctx,
			`SELECT `+strings.ReplaceAll(projectColumns, "p.", "projects.")+
				` FROM projects WHERE id = $1`, id))
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Repository) Project(ctx context.Context, id string) (*Project, error) {
	return scanProject(r.pool.QueryRow(ctx,
		`SELECT `+projectColumns+` FROM projects p WHERE p.id = $1`, id))
}

// Projects lists what the caller can read.
//
// Keys once, then one indexed EXISTS per row — never a JOIN. A join duplicates a
// row when an object holds two matching keys, which silently halves a page and
// makes the cursor skip.
func (r *Repository) Projects(ctx context.Context, workspaceID string, keys []string, includeArchived bool) ([]Project, error) {
	if len(keys) == 0 {
		return []Project{}, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT `+projectColumns+`
		  FROM projects p
		 WHERE p.workspace_id = $1
		   AND ($3 OR p.archived_at IS NULL)
		   AND EXISTS (SELECT 1 FROM acl_key k
		                WHERE k.object_type = 'project' AND k.object_id = p.id
		                  AND k.key = ANY($2))
		 ORDER BY p.name ASC`, workspaceID, keys, includeArchived)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	out := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.Key, &p.Description,
			&p.ArchivedAt, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Repository) States(ctx context.Context, projectID string) ([]State, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id::text, project_id::text, name, category, position, color, created_at
		  FROM issue_states WHERE project_id = $1 ORDER BY position, created_at`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list states: %w", err)
	}
	defer rows.Close()

	out := []State{}
	for rows.Next() {
		var s State
		if err := rows.Scan(&s.ID, &s.ProjectID, &s.Name, &s.Category,
			&s.Position, &s.Color, &s.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan state: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Issues
// ---------------------------------------------------------------------------

const issueColumns = `i.id::text, i.workspace_id::text, i.project_id::text, i.number,
	p.key || '-' || i.number::text AS issue_key,
	i.title, i.description, i.state_id::text, i.assignee_id::text, i.cycle_id::text,
	i.priority, i.rank, i.completed_at, i.archived_at,
	i.created_by::text, i.created_at, i.updated_at`

func scanIssue(row pgx.Row) (*Issue, error) {
	var i Issue
	err := row.Scan(&i.ID, &i.WorkspaceID, &i.ProjectID, &i.Number, &i.Key,
		&i.Title, &i.Description, &i.StateID, &i.AssigneeID, &i.CycleID,
		&i.Priority, &i.Rank, &i.CompletedAt, &i.ArchivedAt,
		&i.CreatedBy, &i.CreatedAt, &i.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan issue: %w", err)
	}
	return &i, nil
}

// CreateIssue allocates a number, a rank and an ACL row in one transaction.
func (r *Repository) CreateIssue(ctx context.Context, projectID, title, description, stateID, assigneeID, actorID string, priority int) (*Issue, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, fmt.Errorf("%w: an issue needs a title", ErrBadInput)
	}
	if priority < 0 || priority > 4 {
		return nil, fmt.Errorf("%w: priority is 0 (none) to 4 (low)", ErrBadInput)
	}

	var out *Issue
	err := database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		var workspaceID string
		var archived *time.Time
		if err := tx.QueryRow(ctx,
			`SELECT workspace_id::text, archived_at FROM projects WHERE id = $1`,
			projectID).Scan(&workspaceID, &archived); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if archived != nil {
			return ErrProjectArchived
		}

		// The state must belong to THIS project. The composite foreign key
		// enforces it too; this is the check that produces a message.
		if stateID == "" {
			if err := tx.QueryRow(ctx,
				`SELECT id::text FROM issue_states WHERE project_id = $1
				  ORDER BY position, created_at LIMIT 1`, projectID).Scan(&stateID); err != nil {
				return fmt.Errorf("resolve default state: %w", err)
			}
		} else {
			var n int
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM issue_states WHERE project_id = $1 AND id = $2`,
				projectID, stateID).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return ErrCrossProject
			}
		}

		// The number, transactionally. FOR UPDATE rather than a sequence: a
		// rolled-back issue must return its number, because a gap in PROJ-13 is
		// indistinguishable from a deletion and there is no answer to "where did
		// it go".
		var number int
		if err := tx.QueryRow(ctx, `
			UPDATE issue_counters SET next_value = next_value + 1
			 WHERE project_id = $1 RETURNING next_value - 1`, projectID).Scan(&number); err != nil {
			return fmt.Errorf("allocate issue number: %w", err)
		}

		// New issues go to the TOP of the project's order. A new thing at the
		// bottom of a long backlog is a new thing nobody sees.
		var lowest *string
		if err := tx.QueryRow(ctx,
			`SELECT min(rank) FROM issues WHERE project_id = $1 AND archived_at IS NULL`,
			projectID).Scan(&lowest); err != nil {
			return err
		}
		rank, err := Between("", derefOr(lowest, ""))
		if err != nil {
			return fmt.Errorf("allocate rank: %w", err)
		}

		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO issues (workspace_id, project_id, number, title, description,
			                    state_id, assignee_id, priority, rank, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id::text`,
			workspaceID, projectID, number, title, description, stateID,
			nullable(assigneeID), priority, rank, nullable(actorID)).Scan(&id); err != nil {
			return fmt.Errorf("create issue: %w", err)
		}

		if err := r.authz.RegisterTx(ctx, tx,
			authz.IssueObject(id), authz.ProjectObject(projectID)); err != nil {
			return fmt.Errorf("register issue: %w", err)
		}

		out, err = scanIssue(tx.QueryRow(ctx,
			`SELECT `+issueColumns+` FROM issues i JOIN projects p ON p.id = i.project_id
			  WHERE i.id = $1`, id))
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Repository) Issue(ctx context.Context, id string) (*Issue, error) {
	return scanIssue(r.pool.QueryRow(ctx,
		`SELECT `+issueColumns+` FROM issues i JOIN projects p ON p.id = i.project_id
		  WHERE i.id = $1`, id))
}

// ListOptions is a board or backlog query.
type ListOptions struct {
	ProjectID string
	StateID   string
	// Cursor is the (rank, id) of the last row of the previous page. Rank, not
	// time: the list is ordered by rank, and paginating a rank-ordered list by
	// created_at skips and repeats rows at every boundary.
	CursorRank string
	CursorID   string
	Limit      int
}

// List returns issues in RANK order.
//
// Authorization is two things and both are load-bearing. The caller has already
// been checked against the PROJECT — that is the 404 gate. This adds a per-row
// EXISTS against acl_key, which is what an issue shared individually with
// somebody who cannot read the project needs, and what stops the reverse.
//
// EXISTS, not JOIN. A join duplicates a row that holds two matching keys, so
// LIMIT 51 returns 25 distinct cards and the cursor built from the 51st row
// skips everything between.
func (r *Repository) List(ctx context.Context, keys []string, opts ListOptions) ([]Issue, string, string, bool, error) {
	if len(keys) == 0 {
		return []Issue{}, "", "", false, nil
	}
	if opts.Limit <= 0 {
		opts.Limit = 50
	}

	rows, err := r.pool.Query(ctx, `
		SELECT `+issueColumns+`
		  FROM issues i
		  JOIN projects p ON p.id = i.project_id
		 WHERE i.project_id = $1
		   AND i.archived_at IS NULL
		   AND ($2::uuid IS NULL OR i.state_id = $2)
		   AND ($3::text IS NULL OR (i.rank, i.id::text) > ($3::text, $4::text))
		   AND EXISTS (SELECT 1 FROM acl_key k
		                WHERE k.object_type = 'issue' AND k.object_id = i.id
		                  AND k.key = ANY($5))
		 ORDER BY i.rank ASC, i.id ASC
		 LIMIT $6`,
		opts.ProjectID, nullable(opts.StateID), nullable(opts.CursorRank),
		opts.CursorID, keys, opts.Limit+1)
	if err != nil {
		return nil, "", "", false, fmt.Errorf("list issues: %w", err)
	}
	defer rows.Close()

	out := []Issue{}
	for rows.Next() {
		var i Issue
		if err := rows.Scan(&i.ID, &i.WorkspaceID, &i.ProjectID, &i.Number, &i.Key,
			&i.Title, &i.Description, &i.StateID, &i.AssigneeID, &i.CycleID,
			&i.Priority, &i.Rank, &i.CompletedAt, &i.ArchivedAt,
			&i.CreatedBy, &i.CreatedAt, &i.UpdatedAt); err != nil {
			return nil, "", "", false, fmt.Errorf("scan issue: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, "", "", false, fmt.Errorf("list issues: %w", err)
	}

	hasMore := len(out) > opts.Limit
	if hasMore {
		out = out[:opts.Limit]
	}
	var nextRank, nextID string
	if hasMore && len(out) > 0 {
		nextRank, nextID = out[len(out)-1].Rank, out[len(out)-1].ID
	}
	return out, nextRank, nextID, hasMore, nil
}

// Move places an issue between two others, and optionally into another state.
//
// THE CLIENT NEVER SENDS A RANK. It names the neighbours it dropped between,
// which is what it actually knows; a client-computed rank would be a second
// implementation of the algebra, computed against a board that may have moved
// underneath it.
//
// The whole thing is one transaction with the issue row locked, so two people
// dragging the same card serialise instead of both computing a rank against the
// same neighbours and landing on the same value.
func (r *Repository) Move(ctx context.Context, issueID, afterID, beforeID, stateID, actorID string) (*Issue, error) {
	var out *Issue
	err := database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		var projectID, currentState string
		if err := tx.QueryRow(ctx,
			`SELECT project_id::text, state_id::text FROM issues WHERE id = $1 FOR UPDATE`,
			issueID).Scan(&projectID, &currentState); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}

		target := currentState
		if stateID != "" && stateID != currentState {
			var n int
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM issue_states WHERE project_id = $1 AND id = $2`,
				projectID, stateID).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return ErrCrossProject
			}
			target = stateID
		}

		// THE NEIGHBOURS ARE RESOLVED AGAINST THE DATABASE, not taken from the
		// request verbatim, and that is what makes repeated drops into one gap
		// work.
		//
		// Between is a PURE FUNCTION: given the same two ranks it returns the
		// same answer. A client that drops five cards "between a and c" sends
		// the same pair five times, so taking it literally would give all five
		// the same rank — and duplicate ranks are a board that renders in a
		// different order on every read.
		//
		// So the request names an ANCHOR and the server finds the real
		// neighbour next to it. "After a" means "immediately after a", which is
		// what the user did, and it is a different card each time as the gap
		// fills.
		currentRank, err := currentRankOf(ctx, tx, issueID)
		if err != nil {
			return err
		}

		var rank string
		switch {
		case afterID == "" && beforeID == "":
			// No anchor: this is a state change, not a reorder. KEEPING the rank
			// is the whole point — recomputing it here reset every dragged card
			// to the top of its column the moment somebody changed its status.
			rank = currentRank

		default:
			lo, err := rankOf(ctx, tx, projectID, afterID)
			if err != nil {
				return err
			}
			hi, err := rankOf(ctx, tx, projectID, beforeID)
			if err != nil {
				return err
			}
			// Tighten each bound to the ACTUAL adjacent row, excluding the card
			// being moved (it is still at its old rank until this commits).
			if afterID != "" {
				hi, err = nextRankAfter(ctx, tx, projectID, lo, issueID)
				if err != nil {
					return err
				}
			} else {
				lo, err = prevRankBefore(ctx, tx, projectID, hi, issueID)
				if err != nil {
					return err
				}
			}
			rank, err = Between(lo, hi)
			if err != nil {
				if !errors.Is(err, ErrRankExhausted) {
					return err
				}
				// Renormalise the whole project and retry once. A partial
				// renormalisation leaves rewritten rows next to unrewritten ones
				// with no guarantee about the gap — the same problem one insert
				// later.
				if err := renormalise(ctx, tx, projectID); err != nil {
					return err
				}
				lo, err = rankOf(ctx, tx, projectID, afterID)
				if err != nil {
					return err
				}
				hi, err = rankOf(ctx, tx, projectID, beforeID)
				if err != nil {
					return err
				}
				if afterID != "" {
					if hi, err = nextRankAfter(ctx, tx, projectID, lo, issueID); err != nil {
						return err
					}
				} else if lo, err = prevRankBefore(ctx, tx, projectID, hi, issueID); err != nil {
					return err
				}
				if rank, err = Between(lo, hi); err != nil {
					return err
				}
			}
		}

		completed := `CASE WHEN s.category = 'completed' THEN COALESCE(i.completed_at, NOW()) ELSE NULL END`
		if _, err := tx.Exec(ctx, `
			UPDATE issues i
			   SET rank = $2, state_id = $3,
			       completed_at = (SELECT `+completed+` FROM issue_states s WHERE s.id = $3)
			 WHERE i.id = $1`, issueID, rank, target); err != nil {
			return fmt.Errorf("move issue: %w", err)
		}

		if target != currentState {
			if _, err := tx.Exec(ctx, `
				INSERT INTO issue_activity (issue_id, actor_id, kind, from_value, to_value)
				VALUES ($1, $2, 'state.changed', $3, $4)`,
				issueID, nullable(actorID), currentState, target); err != nil {
				return fmt.Errorf("record state change: %w", err)
			}
		}

		out, err = scanIssue(tx.QueryRow(ctx,
			`SELECT `+issueColumns+` FROM issues i JOIN projects p ON p.id = i.project_id
			  WHERE i.id = $1`, issueID))
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// currentRankOf reads the rank of the issue being moved.
func currentRankOf(ctx context.Context, tx pgx.Tx, issueID string) (string, error) {
	var rank string
	if err := tx.QueryRow(ctx, `SELECT rank FROM issues WHERE id = $1`, issueID).Scan(&rank); err != nil {
		return "", err
	}
	return rank, nil
}

// nextRankAfter returns the smallest rank strictly greater than lo, ignoring the
// card being moved — which still sits at its old rank until the update commits,
// and would otherwise be its own neighbour.
//
// An empty result means "nothing above", which Between reads as the end.
func nextRankAfter(ctx context.Context, tx pgx.Tx, projectID, lo, movingID string) (string, error) {
	var next *string
	err := tx.QueryRow(ctx, `
		SELECT min(rank) FROM issues
		 WHERE project_id = $1 AND archived_at IS NULL AND id <> $3 AND rank > $2`,
		projectID, lo, movingID).Scan(&next)
	if err != nil {
		return "", fmt.Errorf("resolve the next rank: %w", err)
	}
	return derefOr(next, ""), nil
}

// prevRankBefore is nextRankAfter's mirror.
func prevRankBefore(ctx context.Context, tx pgx.Tx, projectID, hi, movingID string) (string, error) {
	var prev *string
	err := tx.QueryRow(ctx, `
		SELECT max(rank) FROM issues
		 WHERE project_id = $1 AND archived_at IS NULL AND id <> $3 AND rank < $2`,
		projectID, hi, movingID).Scan(&prev)
	if err != nil {
		return "", fmt.Errorf("resolve the previous rank: %w", err)
	}
	return derefOr(prev, ""), nil
}

// rankOf reads a neighbour's rank, refusing one from another project — which
// would produce a rank that sorts nowhere meaningful in this one.
func rankOf(ctx context.Context, tx pgx.Tx, projectID, issueID string) (string, error) {
	if issueID == "" {
		return "", nil
	}
	var rank, owner string
	err := tx.QueryRow(ctx, `SELECT rank, project_id::text FROM issues WHERE id = $1`,
		issueID).Scan(&rank, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if owner != projectID {
		return "", ErrCrossProject
	}
	return rank, nil
}

// renormalise rewrites every rank in a project, evenly spread.
func renormalise(ctx context.Context, tx pgx.Tx, projectID string) error {
	rows, err := tx.Query(ctx,
		`SELECT id::text FROM issues WHERE project_id = $1 ORDER BY rank, id FOR UPDATE`, projectID)
	if err != nil {
		return fmt.Errorf("read ranks for renormalisation: %w", err)
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	ranks, err := Renormalise(len(ids))
	if err != nil {
		return err
	}
	for i, id := range ids {
		if _, err := tx.Exec(ctx, `UPDATE issues SET rank = $2 WHERE id = $1`, id, ranks[i]); err != nil {
			return fmt.Errorf("renormalise rank: %w", err)
		}
	}
	return nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func derefOr(p *string, def string) string {
	if p == nil {
		return def
	}
	return *p
}

// Update changes the mutable fields of an issue and records what moved.
//
// Every field is a pointer so "not supplied" and "cleared" are different: a
// PATCH that sent assignee_id: null must unassign, and one that omitted it must
// not.
func (r *Repository) Update(ctx context.Context, id string, title, description, assigneeID *string, priority *int, actorID string) (*Issue, error) {
	if priority != nil && (*priority < 0 || *priority > 4) {
		return nil, fmt.Errorf("%w: priority is 0 (none) to 4 (low)", ErrBadInput)
	}

	var out *Issue
	err := database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		var previousAssignee *string
		if err := tx.QueryRow(ctx,
			`SELECT assignee_id::text FROM issues WHERE id = $1 FOR UPDATE`,
			id).Scan(&previousAssignee); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}

		if _, err := tx.Exec(ctx, `
			UPDATE issues
			   SET title       = COALESCE($2, title),
			       description = COALESCE($3, description),
			       assignee_id = CASE WHEN $4::bool THEN $5::uuid ELSE assignee_id END,
			       priority    = COALESCE($6, priority)
			 WHERE id = $1`,
			id, title, description,
			assigneeID != nil, nullablePtr(assigneeID), priority); err != nil {
			return fmt.Errorf("update issue: %w", err)
		}

		if assigneeID != nil && derefOr(previousAssignee, "") != *assigneeID {
			if _, err := tx.Exec(ctx, `
				INSERT INTO issue_activity (issue_id, actor_id, kind, from_value, to_value)
				VALUES ($1, $2, 'assignee.set', $3, $4)`,
				id, nullable(actorID), derefOr(previousAssignee, ""), *assigneeID); err != nil {
				return fmt.Errorf("record assignment: %w", err)
			}
		}

		var scanErr error
		out, scanErr = scanIssue(tx.QueryRow(ctx,
			`SELECT `+issueColumns+` FROM issues i JOIN projects p ON p.id = i.project_id
			  WHERE i.id = $1`, id))
		return scanErr
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Activity returns an issue's timeline, newest first.
func (r *Repository) Activity(ctx context.Context, issueID string) ([]Activity, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id::text, issue_id::text, actor_id::text, kind, from_value, to_value, created_at
		  FROM issue_activity WHERE issue_id = $1 ORDER BY created_at DESC LIMIT 200`, issueID)
	if err != nil {
		return nil, fmt.Errorf("list activity: %w", err)
	}
	defer rows.Close()

	out := []Activity{}
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.IssueID, &a.ActorID, &a.Kind,
			&a.From, &a.To, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan activity: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// nullablePtr turns an optional clearing value into a nullable column value.
func nullablePtr(p *string) *string {
	if p == nil || *p == "" {
		return nil
	}
	return p
}
