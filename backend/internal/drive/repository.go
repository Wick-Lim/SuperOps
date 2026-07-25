package drive

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/drive/registry"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

// Repository owns every write to drive_folders, and every write to `files` that
// is about Drive rather than about a chat attachment.
//
// docs/plans/02-drive.md §10 asks for that boundary by name: `files` now serves
// two lifecycles, and a single-purpose table gaining a second purpose is exactly
// how the `message_id IS NULL` ambiguity happened. Nothing outside this package
// and internal/file writes it, and every predicate that touches folder_id,
// message_id or trashed_at names all three.
type Repository struct {
	pool  *pgxpool.Pool
	authz *authz.Checker
	kinds *registry.Registry
	// retention is how long a trashed object is kept; see SetRetention.
	retention time.Duration
}

func NewRepository(pool *pgxpool.Pool, az *authz.Checker, kinds *registry.Registry) *Repository {
	return &Repository{pool: pool, authz: az, kinds: kinds, retention: DefaultRetention}
}

const folderColumns = `id::text, workspace_id::text, parent_id::text, name, is_root,
	created_by::text, trashed_at, created_at, updated_at`

func scanFolder(row pgx.Row) (*Folder, error) {
	var f Folder
	err := row.Scan(&f.ID, &f.WorkspaceID, &f.ParentID, &f.Name, &f.IsRoot,
		&f.CreatedBy, &f.TrashedAt, &f.CreatedAt, &f.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan folder: %w", err)
	}
	return &f, nil
}

const fileColumns = `id::text, workspace_id::text, folder_id::text, name, file_type,
	content_type, size_bytes, current_version,
	(thumbnail_key IS NOT NULL AND thumbnail_key <> '') AS has_thumb,
	user_id::text, trashed_at, created_at, updated_at, storage_key`

func scanFile(row pgx.Row) (*File, error) {
	var f File
	err := row.Scan(&f.ID, &f.WorkspaceID, &f.FolderID, &f.Name, &f.FileType,
		&f.ContentType, &f.SizeBytes, &f.Version, &f.HasThumb,
		&f.CreatedBy, &f.TrashedAt, &f.CreatedAt, &f.UpdatedAt, &f.storageKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan file: %w", err)
	}
	return &f, nil
}

// ---------------------------------------------------------------------------
// The root folder
// ---------------------------------------------------------------------------

// EnsureRoot returns the workspace's Drive root, creating it if it is missing.
//
// Creating a workspace's Drive is THREE writes and they are one transaction:
//
//  1. the drive_folders row
//  2. the acl_object row — folders are ACL-native, so authz.Register owns the
//     path, which is what makes moving a folder a prefix rewrite
//  3. the grant that makes it shared. Without it the root is an ordinary
//     ACL-native object, which is deny-by-default, and Drive is invisible to
//     everyone including the person who created the workspace.
//
// See docs/plans/README.md ruling 5. Idempotent, because it is called from
// workspace creation AND lazily from the first Drive request: a workspace that
// predates Drive, or one whose creation raced this, must not be permanently
// without a Drive.
func (r *Repository) EnsureRoot(ctx context.Context, workspaceID, actorID string) (*Folder, error) {
	if existing, err := r.Root(ctx, workspaceID); err != nil || existing != nil {
		return existing, err
	}

	var out *Folder
	err := database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		// ON CONFLICT against the partial unique index: two concurrent first
		// requests must not produce two roots, and the loser reads the winner's
		// row rather than failing the user's request.
		var id string
		err := tx.QueryRow(ctx,
			`INSERT INTO drive_folders (workspace_id, name, is_root, created_by)
			 VALUES ($1, 'Drive', TRUE, $2)
			 ON CONFLICT (workspace_id) WHERE is_root DO NOTHING
			 RETURNING id::text`,
			workspaceID, nullable(actorID)).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			// Lost the race. The winner's transaction has committed the row and
			// its ACL, so reading it outside this transaction is correct.
			return nil
		}
		if err != nil {
			return fmt.Errorf("create drive root: %w", err)
		}

		if err := r.authz.RegisterTx(ctx, tx, authz.FolderObject(id), authz.WorkspaceObject(workspaceID)); err != nil {
			return fmt.Errorf("register drive root: %w", err)
		}
		if err := r.authz.GrantTx(ctx, tx, authz.UserSubject(actorID),
			authz.WorkspaceSubject(workspaceID), authz.FolderObject(id), authz.CapAdmin); err != nil {
			return fmt.Errorf("share drive root with the workspace: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out != nil {
		return out, nil
	}
	return r.Root(ctx, workspaceID)
}

// Root reads the workspace's root folder, or (nil, nil).
func (r *Repository) Root(ctx context.Context, workspaceID string) (*Folder, error) {
	return scanFolder(r.pool.QueryRow(ctx,
		`SELECT `+folderColumns+` FROM drive_folders WHERE workspace_id = $1 AND is_root`,
		workspaceID))
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

func (r *Repository) Folder(ctx context.Context, id string) (*Folder, error) {
	return scanFolder(r.pool.QueryRow(ctx,
		`SELECT `+folderColumns+` FROM drive_folders WHERE id = $1`, id))
}

func (r *Repository) File(ctx context.Context, id string) (*File, error) {
	return scanFile(r.pool.QueryRow(ctx,
		`SELECT `+fileColumns+` FROM files WHERE id = $1`, id))
}

// Breadcrumb returns the chain from the root down to and including id.
//
// It walks drive_folders.parent_id rather than parsing acl_object.path, because
// the path carries ids and the breadcrumb needs names. The depth cap bounds the
// recursion; a cycle is impossible by construction (Move refuses one) but the
// cap makes a corrupt row a truncated breadcrumb rather than a hung request.
func (r *Repository) Breadcrumb(ctx context.Context, id string) ([]Folder, error) {
	rows, err := r.pool.Query(ctx, `
		WITH RECURSIVE up AS (
		    SELECT `+folderColumns+`, 1 AS depth
		      FROM drive_folders WHERE id = $1
		    UNION ALL
		    SELECT p.id::text, p.workspace_id::text, p.parent_id::text, p.name, p.is_root,
		           p.created_by::text, p.trashed_at, p.created_at, p.updated_at, up.depth + 1
		      FROM drive_folders p
		      JOIN up ON up.parent_id = p.id::text
		     WHERE up.depth < $2
		)
		SELECT id, workspace_id, parent_id, name, is_root, created_by, trashed_at, created_at, updated_at
		  FROM up ORDER BY depth DESC`, id, MaxFolderDepth+2)
	if err != nil {
		return nil, fmt.Errorf("read breadcrumb: %w", err)
	}
	defer rows.Close()

	out := []Folder{}
	for rows.Next() {
		var f Folder
		if err := rows.Scan(&f.ID, &f.WorkspaceID, &f.ParentID, &f.Name, &f.IsRoot,
			&f.CreatedBy, &f.TrashedAt, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan breadcrumb: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Children lists one folder's contents, folders first, then files, both by
// (created_at, id) descending.
//
// THE LISTING NEVER CALLS Can PER ROW. Both halves filter on the caller's
// access keys — one indexed join for the whole page — which is the N+1 that
// docs/plans/00-permissions.md exists to prevent and that Drive is the first
// endpoint to hit.
//
// Folders sort before files regardless of age because that is what a file
// browser does; the cursor therefore carries the kind implicitly, by ordering
// folders under a sentinel that always sorts first.
func (r *Repository) Children(ctx context.Context, folderID string, keys []string, page httputil.PaginationParams) ([]Entry, string, bool, error) {
	if len(keys) == 0 {
		return []Entry{}, "", false, nil
	}

	// One extra row decides has_more without a second count query.
	limit := page.Limit + 1

	var (
		cursorAt time.Time
		cursorID string
	)
	if !page.Cursor.IsZero() {
		cursorAt = page.Cursor.CreatedAt
		// The cursor's id field carries "<kind>:<uuid>" because the listing
		// orders by (kind, created_at, id) and httputil.Cursor has room for
		// two columns. The keyset comparison needs the bare id; passing the
		// prefixed form would compare "file:9f..." against a uuid and page
		// incorrectly at every boundary.
		if _, bare, ok := strings.Cut(page.Cursor.ID, ":"); ok {
			cursorID = bare
		} else {
			cursorID = page.Cursor.ID
		}
	}

	rows, err := r.pool.Query(ctx, `
		WITH visible_folders AS (
		    SELECT f.id::text AS id, f.workspace_id::text AS workspace_id,
		           f.parent_id::text AS container, f.name AS name, f.is_root AS is_root,
		           f.created_by::text AS created_by, f.trashed_at AS trashed_at,
		           f.created_at AS created_at, f.updated_at AS updated_at
		      FROM drive_folders f
		     WHERE f.parent_id = $1
		       AND f.trashed_at IS NULL
		       AND EXISTS (SELECT 1 FROM acl_key k
		                    WHERE k.object_type = 'folder' AND k.object_id = f.id
		                      AND k.key = ANY($2))
		), visible_files AS (
		    SELECT f.id::text AS id, f.workspace_id::text AS workspace_id,
		           f.folder_id::text AS container, f.name AS name, f.file_type AS file_type,
		           f.content_type AS content_type, f.size_bytes AS size_bytes,
		           f.current_version AS current_version,
		           (f.thumbnail_key IS NOT NULL AND f.thumbnail_key <> '') AS has_thumb,
		           f.user_id::text AS created_by, f.trashed_at AS trashed_at,
		           f.created_at AS created_at, f.updated_at AS updated_at,
		           f.storage_key AS storage_key
		      FROM files f
		     WHERE f.folder_id = $1
		       AND f.trashed_at IS NULL
		       AND EXISTS (SELECT 1 FROM acl_key k
		                    WHERE k.object_type = 'file' AND k.object_id = f.id
		                      AND k.key = ANY($2))
		)
		SELECT * FROM (
		    SELECT 'folder'::text AS kind, id, workspace_id, container, name,
		           ''::text AS file_type, ''::text AS content_type, 0::bigint AS size_bytes,
		           0::int AS current_version, FALSE AS has_thumb, created_by, is_root,
		           trashed_at, created_at, updated_at, ''::text AS storage_key
		      FROM visible_folders
		    UNION ALL
		    SELECT 'file'::text, id, workspace_id, container, name,
		           file_type, content_type, size_bytes,
		           current_version, has_thumb, created_by, FALSE,
		           trashed_at, created_at, updated_at, storage_key
		      FROM visible_files
		) e
		 -- The keyset. It CANNOT be a single row comparison: the ordering mixes
		 -- ASC (kind) with DESC (created_at, id), and a tuple compare applies one
		 -- direction to every column. Written as a tuple it silently skips rows
		 -- at exactly the page boundary — which is the bug nobody sees until a
		 -- folder has more items than fit on one screen.
		 WHERE $3::timestamptz IS NULL
		    OR e.kind > $5::text
		    OR (e.kind = $5::text AND (e.created_at, e.id) < ($3::timestamptz, $4::text))
		 ORDER BY e.kind ASC, e.created_at DESC, e.id DESC
		 LIMIT $6`,
		folderID, keys, nullableTime(cursorAt), cursorID, cursorKind(page), limit)
	if err != nil {
		return nil, "", false, fmt.Errorf("list folder children: %w", err)
	}
	defer rows.Close()

	entries := []Entry{}
	for rows.Next() {
		var (
			kind, id, workspaceID, name, fileType, contentType, createdBy, storageKey string
			container                                                                 *string
			size                                                                      int64
			version                                                                   int
			hasThumb, isRoot                                                          bool
			trashedAt                                                                 *time.Time
			createdAt, updatedAt                                                      time.Time
		)
		if err := rows.Scan(&kind, &id, &workspaceID, &container, &name,
			&fileType, &contentType, &size, &version, &hasThumb, &createdBy, &isRoot,
			&trashedAt, &createdAt, &updatedAt, &storageKey); err != nil {
			return nil, "", false, fmt.Errorf("scan folder child: %w", err)
		}
		e := Entry{Kind: kind, sortAt: createdAt, sortID: id}
		if kind == EntryFolder {
			e.Folder = &Folder{ID: id, WorkspaceID: workspaceID, ParentID: container, Name: name,
				IsRoot: isRoot, CreatedBy: &createdBy, TrashedAt: trashedAt,
				CreatedAt: createdAt, UpdatedAt: updatedAt}
		} else {
			e.File = &File{ID: id, WorkspaceID: workspaceID, FolderID: container, Name: name,
				FileType: fileType, ContentType: contentType, SizeBytes: size, Version: version,
				HasThumb: hasThumb, CreatedBy: createdBy, TrashedAt: trashedAt,
				CreatedAt: createdAt, UpdatedAt: updatedAt, storageKey: storageKey}
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", false, fmt.Errorf("list folder children: %w", err)
	}

	hasMore := len(entries) > page.Limit
	if hasMore {
		entries = entries[:page.Limit]
	}
	cursor := ""
	if hasMore && len(entries) > 0 {
		last := entries[len(entries)-1]
		cursor = encodeEntryCursor(last)
	}
	return entries, cursor, hasMore, nil
}

// cursorKind extracts the kind half of a paging cursor.
//
// The listing orders by (kind, created_at DESC, id DESC), so the keyset
// comparison needs all three. The kind rides in the cursor's id field, prefixed,
// because httputil.Cursor is (time, id) and adding a third column to it would
// change every other paginated endpoint in the product for one caller.
func cursorKind(page httputil.PaginationParams) string {
	if page.Cursor.IsZero() {
		return ""
	}
	if kind, _, ok := strings.Cut(page.Cursor.ID, ":"); ok {
		return kind
	}
	return EntryFolder
}

func encodeEntryCursor(e Entry) string {
	return httputil.EncodeCursor(e.sortAt, e.Kind+":"+e.sortID)
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
