package drive

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/drive/registry"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
)

// CreateFolder adds a folder under parentID.
//
// Two writes, one commit: the drive_folders row and authz.Register. A folder
// whose acl_object row was rolled back is readable by nobody and invisible to
// the drift verifier's expected-state views (folders are ACL-native, so nothing
// recomputes them) — it would need a manual repair.
//
// It does NOT write a grant. Only the root carries the workspace grant;
// everything below inherits it through the path, which is the entire reason
// sharing is expressed as inheritance rather than as a rule.
func (r *Repository) CreateFolder(ctx context.Context, workspaceID, parentID, name, actorID string) (*Folder, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > MaxNameLength {
		return nil, fmt.Errorf("%w: folder name must be 1-%d characters", errBadInput, MaxNameLength)
	}

	parent, err := r.Folder(ctx, parentID)
	if err != nil {
		return nil, err
	}
	if parent == nil {
		return nil, ErrNotFound
	}
	if parent.WorkspaceID != workspaceID {
		return nil, ErrCrossWorkspace
	}
	if parent.TrashedAt != nil {
		return nil, ErrTrashed
	}
	depth, err := r.depthOf(ctx, parentID)
	if err != nil {
		return nil, err
	}
	if depth+1 > MaxFolderDepth {
		return nil, ErrTooDeep
	}

	var out *Folder
	err = database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx,
			`INSERT INTO drive_folders (workspace_id, parent_id, name, created_by)
			 VALUES ($1, $2, $3, $4) RETURNING id::text`,
			workspaceID, parentID, name, nullable(actorID)).Scan(&id); err != nil {
			return fmt.Errorf("create folder: %w", err)
		}
		if err := r.authz.RegisterTx(ctx, tx, authz.FolderObject(id), authz.FolderObject(parentID)); err != nil {
			return fmt.Errorf("register folder: %w", err)
		}
		out, err = scanFolder(tx.QueryRow(ctx,
			`SELECT `+folderColumns+` FROM drive_folders WHERE id = $1`, id))
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// errBadInput marks a validation failure the handler turns into 400. Wrapped
// rather than returned bare so the message reaches the caller.
var errBadInput = errors.New("drive: invalid input")

// IsBadInput reports whether err is a validation failure.
func IsBadInput(err error) bool { return errors.Is(err, errBadInput) }

// CreateFile is "new from the registry": one transaction writing the files row,
// Kind.New and nothing else.
//
// A file is a DERIVED type, so its acl_object row and its keys are COMPUTED from
// folder_id rather than written by hand — authz.Register refuses a file outright,
// and a hand-placed row would be reverted by the next Rebuild.
//
// But computed does not mean "eventually". authz.MaterializeTx runs the same
// views Rebuild does, filtered to this one object, in this transaction. Without
// it the file exists, opens fine (Capability resolves it from files.folder_id
// directly) and is ABSENT FROM ITS OWN FOLDER'S LISTING until the hourly drift
// job runs — because every list path filters on acl_key. It then appears on its
// own, up to an hour later. That is the listing-versus-opening split
// docs/plans/README.md ruling 5 exists to prevent, and it shipped once.
func (r *Repository) CreateFile(ctx context.Context, workspaceID, folderID, name, fileType, actorID string) (*File, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > MaxNameLength {
		return nil, fmt.Errorf("%w: file name must be 1-%d characters", errBadInput, MaxNameLength)
	}
	kind, ok := r.kinds.Lookup(fileType)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownType, fileType)
	}
	if kind.New == nil {
		// A plain file is uploaded, not created: there is no way to put bytes
		// into the row this would produce.
		return nil, fmt.Errorf("%w: %q objects are uploaded, not created", errBadInput, fileType)
	}

	folder, err := r.Folder(ctx, folderID)
	if err != nil {
		return nil, err
	}
	if folder == nil {
		return nil, ErrNotFound
	}
	if folder.WorkspaceID != workspaceID {
		return nil, ErrCrossWorkspace
	}
	if folder.TrashedAt != nil {
		return nil, ErrTrashed
	}

	var out *File
	err = database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		var id string
		// storage_key is empty for a collab type: the bytes are not the truth,
		// and file.Orphan.Keys() already skips empty keys, so it costs nothing.
		if err := tx.QueryRow(ctx, `
			INSERT INTO files (workspace_id, user_id, folder_id, name, file_type,
			                   content_type, size_bytes, storage_key, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6, 0, '', $2)
			RETURNING id::text`,
			workspaceID, actorID, folderID, name, kind.Type, contentTypeFor(kind)).Scan(&id); err != nil {
			return fmt.Errorf("create drive file: %w", err)
		}

		if err := kind.New(ctx, tx, registry.NewRequest{
			WorkspaceID: workspaceID,
			FileID:      id,
			FolderID:    folderID,
			ActorID:     actorID,
			Name:        name,
		}); err != nil {
			return err
		}

		if err := authz.MaterializeTx(ctx, tx, authz.FileObject(id)); err != nil {
			return fmt.Errorf("materialize file ACL: %w", err)
		}

		out, err = scanFile(tx.QueryRow(ctx, `SELECT `+fileColumns+` FROM files WHERE id = $1`, id))
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// contentTypeFor is what a collab object reports as its media type.
//
// A vendor type per editor, not application/json or text/plain: GET /content
// serves the latest snapshot blob, which is a backup and portability artifact
// rather than a document, and labelling it as something a browser might render
// would invite exactly the wrong handling.
func contentTypeFor(k registry.Kind) string {
	if k.Storage == registry.StorageCollab {
		return "application/vnd.superops." + k.Type
	}
	return "application/octet-stream"
}

// RenameFolder and RenameFile change one column. Rename does not touch the ACL:
// a name is not part of the path (the path carries ids), which is the reason
// renaming a folder with 20k descendants is one UPDATE.
func (r *Repository) RenameFolder(ctx context.Context, id, name string) (*Folder, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > MaxNameLength {
		return nil, fmt.Errorf("%w: folder name must be 1-%d characters", errBadInput, MaxNameLength)
	}
	f, err := scanFolder(r.pool.QueryRow(ctx,
		`UPDATE drive_folders SET name = $2 WHERE id = $1 AND trashed_at IS NULL
		 RETURNING `+folderColumns, id, name))
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, ErrNotFound
	}
	return f, nil
}

func (r *Repository) RenameFile(ctx context.Context, id, name, actorID string) (*File, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > MaxNameLength {
		return nil, fmt.Errorf("%w: file name must be 1-%d characters", errBadInput, MaxNameLength)
	}
	f, err := scanFile(r.pool.QueryRow(ctx,
		`UPDATE files SET name = $2, updated_by = $3 WHERE id = $1 AND trashed_at IS NULL
		 RETURNING `+fileColumns, id, name, nullable(actorID)))
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, ErrNotFound
	}
	return f, nil
}

// MoveFolder relocates a subtree.
//
// The cycle check and the depth check are INSIDE the transaction that performs
// the move, because both are read-then-write: two concurrent moves that each
// see a legal tree can produce an unreachable subtree between them, and an
// unreachable subtree is data the user can no longer find and the GC no longer
// collects.
//
// authz.Move does the path rewrite — one UPDATE over the subtree — and drives
// the Revoker, because what the whole subtree inherits has just changed.
func (r *Repository) MoveFolder(ctx context.Context, actorID, id, newParentID string) (*Folder, error) {
	if id == newParentID {
		return nil, ErrCycle
	}

	folder, err := r.Folder(ctx, id)
	if err != nil {
		return nil, err
	}
	if folder == nil {
		return nil, ErrNotFound
	}
	if folder.IsRoot {
		return nil, fmt.Errorf("%w: the Drive root cannot be moved", errBadInput)
	}
	parent, err := r.Folder(ctx, newParentID)
	if err != nil {
		return nil, err
	}
	if parent == nil {
		return nil, ErrNotFound
	}
	if parent.WorkspaceID != folder.WorkspaceID {
		return nil, ErrCrossWorkspace
	}
	if parent.TrashedAt != nil {
		return nil, ErrTrashed
	}

	// authz.Move refuses a move into the object's own subtree and one that would
	// push a descendant past the depth cap, both inside its own transaction and
	// under the same locks it takes for the rewrite. Doing the same checks here
	// would be a second opinion that can disagree; translating its errors is
	// what keeps one answer.
	if err := r.authz.Move(ctx, authz.UserSubject(actorID),
		authz.FolderObject(id), authz.FolderObject(newParentID)); err != nil {
		return nil, translateMoveError(err)
	}

	if _, err := r.pool.Exec(ctx,
		`UPDATE drive_folders SET parent_id = $2 WHERE id = $1`, id, newParentID); err != nil {
		return nil, fmt.Errorf("move folder: %w", err)
	}
	return r.Folder(ctx, id)
}

// translateMoveError maps authz's refusals onto this package's sentinels so the
// handler answers 409 rather than 500 for a legal request that lost a race.
func translateMoveError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, authz.ErrMoveCycle):
		return ErrCycle
	case errors.Is(err, authz.ErrMoveTooDeep):
		return ErrTooDeep
	case errors.Is(err, authz.ErrCrossWorkspaceMove):
		return ErrCrossWorkspace
	case errors.Is(err, authz.ErrNotRegistered):
		return ErrNotFound
	default:
		return err
	}
}

// MoveFile relocates one file. A file has no subtree, so this is one UPDATE —
// but the file's acl_object PATH is derived from its folder, so the ACL follows
// on the next reconcile rather than in this transaction.
//
// That lag is why the read path does not depend on it: Capability resolves a
// file from files.folder_id directly (checker.resolve), so a moved file is
// authorized against its NEW folder immediately. Only the acl_key
// materialization — the LIST path — lags, and a stale key there can only make a
// file appear in a listing it has just left, never in one it was never in.
func (r *Repository) MoveFile(ctx context.Context, id, newFolderID, actorID string) (*File, error) {
	file, err := r.File(ctx, id)
	if err != nil {
		return nil, err
	}
	if file == nil {
		return nil, ErrNotFound
	}
	folder, err := r.Folder(ctx, newFolderID)
	if err != nil {
		return nil, err
	}
	if folder == nil {
		return nil, ErrNotFound
	}
	if folder.WorkspaceID != file.WorkspaceID {
		return nil, ErrCrossWorkspace
	}
	if folder.TrashedAt != nil {
		return nil, ErrTrashed
	}

	var out *File
	if err := database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		var scanErr error
		out, scanErr = scanFile(tx.QueryRow(ctx,
			`UPDATE files SET folder_id = $2, updated_by = $3 WHERE id = $1 AND trashed_at IS NULL
			 RETURNING `+fileColumns, id, newFolderID, nullable(actorID)))
		if scanErr != nil {
			return scanErr
		}
		if out == nil {
			return ErrTrashed
		}
		// The ACL follows in the SAME transaction, in both directions: the new
		// folder's key is added and the old folder's is removed. Deferring it to
		// the drift job would leave the file listed in the folder it just left —
		// which is worse than the create case, because it is a file appearing
		// somewhere it no longer belongs rather than nowhere at all.
		return authz.MaterializeTx(ctx, tx, authz.FileObject(id))
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// TrashFolder marks a folder and its whole subtree.
//
// The subtree, not just the folder: leaving descendants untrashed would put
// them in a folder the trash listing says is gone, reachable by id and absent
// from every listing. The purge job walks the same set later.
//
// It marks rather than deletes. Deletion goes through the purge job, which owns
// the object cleanup and the audit entry — and until then trashed rows are
// excluded from the object collector by name (internal/file's predicate).
func (r *Repository) TrashFolder(ctx context.Context, id, actorID string) error {
	folder, err := r.Folder(ctx, id)
	if err != nil {
		return err
	}
	if folder == nil {
		return ErrNotFound
	}
	if folder.IsRoot {
		return fmt.Errorf("%w: the Drive root cannot be trashed", errBadInput)
	}

	return database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		// The subtree is found through acl_object.path — the materialized path
		// exists precisely so this is one prefix scan rather than a recursive
		// walk of drive_folders.
		var path string
		if err := tx.QueryRow(ctx,
			`SELECT path FROM acl_object WHERE object_type = 'folder' AND object_id = $1`,
			id).Scan(&path); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("read folder path: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE drive_folders SET trashed_at = NOW(), trashed_by = $2
			 WHERE trashed_at IS NULL
			   AND id IN (SELECT object_id FROM acl_object
			               WHERE object_type = 'folder' AND path LIKE $1 || '%')`,
			path, nullable(actorID)); err != nil {
			return fmt.Errorf("trash folder subtree: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE files SET trashed_at = NOW(), trashed_by = $2, updated_by = $2
			 WHERE trashed_at IS NULL
			   AND folder_id IN (SELECT object_id FROM acl_object
			                      WHERE object_type = 'folder' AND path LIKE $1 || '%')`,
			path, nullable(actorID)); err != nil {
			return fmt.Errorf("trash folder contents: %w", err)
		}
		return nil
	})
}

func (r *Repository) TrashFile(ctx context.Context, id, actorID string) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE files SET trashed_at = NOW(), trashed_by = $2, updated_by = $2
		  WHERE id = $1 AND trashed_at IS NULL`, id, nullable(actorID))
	if err != nil {
		return fmt.Errorf("trash file: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// depthOf counts path segments below the workspace. A folder directly under the
// root is depth 2 (root is 1).
func (r *Repository) depthOf(ctx context.Context, folderID string) (int, error) {
	var path string
	err := r.pool.QueryRow(ctx,
		`SELECT path FROM acl_object WHERE object_type = 'folder' AND object_id = $1`,
		folderID).Scan(&path)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("read folder depth: %w", err)
	}
	// '/workspace:x/folder:y/' has three slashes and one folder segment.
	return strings.Count(path, "/") - 2, nil
}
