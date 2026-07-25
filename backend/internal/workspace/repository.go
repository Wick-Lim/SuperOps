package workspace

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
)

// ErrSlugTaken reports a collision on the globally unique workspaces.slug.
// Checking with GetBySlug before inserting was a TOCTOU: the loser of a
// concurrent create got a 500 instead of the intended 409.
var ErrSlugTaken = errors.New("workspace slug already taken")

// ErrMemberNotFound reports that the (workspace, user) row a write targeted
// does not exist. UpdateMemberRole ignored RowsAffected, so updating a
// non-member answered 200.
var ErrMemberNotFound = errors.New("workspace member not found")

// ErrWorkspaceGone reports that a workspace disappeared between the
// authorization check and the write.
var ErrWorkspaceGone = errors.New("workspace not found")

const workspaceColumns = `id, name, slug, description, icon_url, owner_id, created_at, updated_at`

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// CreateWithOwner inserts a workspace and its owner's membership row
// atomically. As two statements, a failed AddMember left a workspace with no
// members: unreachable through ListByUser, undeletable (Delete requires an
// owner membership row) and holding its globally unique slug forever.
func (r *Repository) CreateWithOwner(ctx context.Context, w *Workspace) error {
	return database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`INSERT INTO workspaces (id, name, slug, description, icon_url, owner_id)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 RETURNING `+workspaceColumns,
			w.ID, w.Name, w.Slug, w.Description, w.IconURL, w.OwnerID,
		).Scan(&w.ID, &w.Name, &w.Slug, &w.Description, &w.IconURL, &w.OwnerID, &w.CreatedAt, &w.UpdatedAt)
		if isUniqueViolation(err) {
			return ErrSlugTaken
		}
		if err != nil {
			return fmt.Errorf("create workspace: %w", err)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, $3)`,
			w.ID, w.OwnerID, RoleOwner,
		); err != nil {
			return fmt.Errorf("add workspace owner: %w", err)
		}
		return nil
	})
}

func (r *Repository) GetByID(ctx context.Context, id string) (*Workspace, error) {
	return r.scanWorkspace(r.pool.QueryRow(ctx,
		`SELECT `+workspaceColumns+` FROM workspaces WHERE id = $1`, id))
}

func (r *Repository) GetBySlug(ctx context.Context, slug string) (*Workspace, error) {
	return r.scanWorkspace(r.pool.QueryRow(ctx,
		`SELECT `+workspaceColumns+` FROM workspaces WHERE slug = $1`, slug))
}

func (r *Repository) ListByUser(ctx context.Context, userID string) ([]*Workspace, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT w.id, w.name, w.slug, w.description, w.icon_url, w.owner_id, w.created_at, w.updated_at
		 FROM workspaces w
		 JOIN workspace_members wm ON w.id = wm.workspace_id
		 WHERE wm.user_id = $1
		 ORDER BY w.name, w.id`, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	defer rows.Close()

	workspaces := []*Workspace{}
	for rows.Next() {
		w := &Workspace{}
		if err := rows.Scan(&w.ID, &w.Name, &w.Slug, &w.Description, &w.IconURL, &w.OwnerID, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan workspace: %w", err)
		}
		workspaces = append(workspaces, w)
	}
	return workspaces, rows.Err()
}

// Update writes the mutable presentation fields. updated_at is maintained by
// the BEFORE UPDATE trigger added in migration 009.
func (r *Repository) Update(ctx context.Context, w *Workspace) error {
	err := r.pool.QueryRow(ctx,
		`UPDATE workspaces SET name = $2, description = $3, icon_url = $4
		 WHERE id = $1
		 RETURNING `+workspaceColumns,
		w.ID, w.Name, w.Description, w.IconURL,
	).Scan(&w.ID, &w.Name, &w.Slug, &w.Description, &w.IconURL, &w.OwnerID, &w.CreatedAt, &w.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrWorkspaceGone
	}
	if err != nil {
		return fmt.Errorf("update workspace: %w", err)
	}
	return nil
}

func (r *Repository) Delete(ctx context.Context, id string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM workspaces WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete workspace: %w", err)
	}
	return nil
}

// Members

func (r *Repository) AddMember(ctx context.Context, m *Member) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, $3)
		 ON CONFLICT (workspace_id, user_id) DO NOTHING`,
		m.WorkspaceID, m.UserID, m.Role,
	)
	if err != nil {
		return fmt.Errorf("add member: %w", err)
	}
	return nil
}

func (r *Repository) GetMember(ctx context.Context, workspaceID, userID string) (*Member, error) {
	m := &Member{}
	err := r.pool.QueryRow(ctx,
		`SELECT workspace_id, user_id, role, joined_at
		 FROM workspace_members WHERE workspace_id = $1 AND user_id = $2`,
		workspaceID, userID,
	).Scan(&m.WorkspaceID, &m.UserID, &m.Role, &m.JoinedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get member: %w", err)
	}
	return m, nil
}

func (r *Repository) ListMembers(ctx context.Context, workspaceID string) ([]*Member, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT workspace_id, user_id, role, joined_at
		 FROM workspace_members WHERE workspace_id = $1 ORDER BY joined_at, user_id`, workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	defer rows.Close()

	members := []*Member{}
	for rows.Next() {
		m := &Member{}
		if err := rows.Scan(&m.WorkspaceID, &m.UserID, &m.Role, &m.JoinedAt); err != nil {
			return nil, fmt.Errorf("scan member: %w", err)
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

// UpdateMemberRole sets a member's role, refusing to touch the owner row: the
// members table is the authoritative record of ownership, and owner transitions
// belong to TransferOwnership so workspaces.owner_id stays in step.
func (r *Repository) UpdateMemberRole(ctx context.Context, workspaceID, userID, role string) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE workspace_members SET role = $3
		  WHERE workspace_id = $1 AND user_id = $2 AND role <> 'owner'`,
		workspaceID, userID, role,
	)
	if err != nil {
		return fmt.Errorf("update member role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrMemberNotFound
	}
	return nil
}

// TransferOwnership moves ownership from the current owner to another member.
//
// Ownership was stored twice — workspaces.owner_id and the members row with
// role 'owner' — and never reconciled. The members table is authoritative;
// owner_id is kept in sync here, in the same transaction, so the two can never
// disagree.
func (r *Repository) TransferOwnership(ctx context.Context, workspaceID, fromUserID, toUserID string) error {
	return database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE workspace_members SET role = $3
			  WHERE workspace_id = $1 AND user_id = $2 AND role = 'owner'`,
			workspaceID, fromUserID, RoleAdmin,
		)
		if err != nil {
			return fmt.Errorf("demote previous owner: %w", err)
		}
		if tag.RowsAffected() == 0 {
			// Somebody else transferred ownership between the check and here.
			return ErrMemberNotFound
		}

		tag, err = tx.Exec(ctx,
			`UPDATE workspace_members SET role = $3
			  WHERE workspace_id = $1 AND user_id = $2`,
			workspaceID, toUserID, RoleOwner,
		)
		if err != nil {
			return fmt.Errorf("promote new owner: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrMemberNotFound
		}

		if _, err := tx.Exec(ctx,
			`UPDATE workspaces SET owner_id = $2 WHERE id = $1`, workspaceID, toUserID,
		); err != nil {
			return fmt.Errorf("update workspace owner: %w", err)
		}
		return nil
	})
}

func (r *Repository) RemoveMember(ctx context.Context, workspaceID, userID string) error {
	tag, err := r.pool.Exec(ctx,
		`DELETE FROM workspace_members
		  WHERE workspace_id = $1 AND user_id = $2 AND role <> 'owner'`,
		workspaceID, userID,
	)
	if err != nil {
		return fmt.Errorf("remove member: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrMemberNotFound
	}
	return nil
}

func (r *Repository) scanWorkspace(row pgx.Row) (*Workspace, error) {
	w := &Workspace{}
	err := row.Scan(&w.ID, &w.Name, &w.Slug, &w.Description, &w.IconURL, &w.OwnerID, &w.CreatedAt, &w.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workspace: %w", err)
	}
	return w, nil
}
