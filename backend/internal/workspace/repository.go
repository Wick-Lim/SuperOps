package workspace

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) Create(ctx context.Context, w *Workspace) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO workspaces (id, name, slug, description, icon_url, owner_id)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		w.ID, w.Name, w.Slug, w.Description, w.IconURL, w.OwnerID,
	)
	if err != nil {
		return fmt.Errorf("create workspace: %w", err)
	}
	return nil
}

func (r *Repository) GetByID(ctx context.Context, id string) (*Workspace, error) {
	w := &Workspace{}
	err := r.pool.QueryRow(ctx,
		`SELECT id, name, slug, description, icon_url, owner_id, created_at, updated_at
		 FROM workspaces WHERE id = $1`, id,
	).Scan(&w.ID, &w.Name, &w.Slug, &w.Description, &w.IconURL, &w.OwnerID, &w.CreatedAt, &w.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workspace: %w", err)
	}
	return w, nil
}

func (r *Repository) GetBySlug(ctx context.Context, slug string) (*Workspace, error) {
	w := &Workspace{}
	err := r.pool.QueryRow(ctx,
		`SELECT id, name, slug, description, icon_url, owner_id, created_at, updated_at
		 FROM workspaces WHERE slug = $1`, slug,
	).Scan(&w.ID, &w.Name, &w.Slug, &w.Description, &w.IconURL, &w.OwnerID, &w.CreatedAt, &w.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workspace by slug: %w", err)
	}
	return w, nil
}

func (r *Repository) ListByUser(ctx context.Context, userID string) ([]*Workspace, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT w.id, w.name, w.slug, w.description, w.icon_url, w.owner_id, w.created_at, w.updated_at
		 FROM workspaces w
		 JOIN workspace_members wm ON w.id = wm.workspace_id
		 WHERE wm.user_id = $1
		 ORDER BY w.name`, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	defer rows.Close()

	var workspaces []*Workspace
	for rows.Next() {
		w := &Workspace{}
		if err := rows.Scan(&w.ID, &w.Name, &w.Slug, &w.Description, &w.IconURL, &w.OwnerID, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan workspace: %w", err)
		}
		workspaces = append(workspaces, w)
	}
	return workspaces, nil
}

func (r *Repository) Update(ctx context.Context, w *Workspace) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE workspaces SET name = $2, description = $3, icon_url = $4, updated_at = NOW()
		 WHERE id = $1`,
		w.ID, w.Name, w.Description, w.IconURL,
	)
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
	if err == pgx.ErrNoRows {
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
		 FROM workspace_members WHERE workspace_id = $1 ORDER BY joined_at`, workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	defer rows.Close()

	var members []*Member
	for rows.Next() {
		m := &Member{}
		if err := rows.Scan(&m.WorkspaceID, &m.UserID, &m.Role, &m.JoinedAt); err != nil {
			return nil, fmt.Errorf("scan member: %w", err)
		}
		members = append(members, m)
	}
	return members, nil
}

func (r *Repository) UpdateMemberRole(ctx context.Context, workspaceID, userID, role string) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE workspace_members SET role = $3 WHERE workspace_id = $1 AND user_id = $2`,
		workspaceID, userID, role,
	)
	if err != nil {
		return fmt.Errorf("update member role: %w", err)
	}
	return nil
}

func (r *Repository) RemoveMember(ctx context.Context, workspaceID, userID string) error {
	_, err := r.pool.Exec(ctx,
		`DELETE FROM workspace_members WHERE workspace_id = $1 AND user_id = $2`,
		workspaceID, userID,
	)
	if err != nil {
		return fmt.Errorf("remove member: %w", err)
	}
	return nil
}
