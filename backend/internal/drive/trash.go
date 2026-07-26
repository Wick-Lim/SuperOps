package drive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/quota"
	"github.com/Wick-Lim/SuperOps/backend/internal/storage"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

// DefaultRetention is how long a trashed object is kept. Thirty days is what
// every product a user has met does, which makes it the number they will assume
// whatever we write in the UI.
//
// 0 disables the automatic purge entirely and leaves DELETE /drive/trash
// working, for a deployment that would rather decide by hand.
const DefaultRetention = 30 * 24 * time.Hour

// TrashEntry is one row of the trash listing.
//
// Only the TOP of each trashed tree appears. A folder whose parent is also
// trashed did not get trashed, it went with its ancestor — listing it separately
// would offer a restore that cannot mean anything on its own, and would show
// somebody who deleted one folder a page of two hundred entries.
type TrashEntry struct {
	Kind       string     `json:"kind"`
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	TrashedAt  time.Time  `json:"trashed_at"`
	TrashedBy  *string    `json:"trashed_by"`
	PurgeAfter *time.Time `json:"purge_after"`
	SizeBytes  int64      `json:"size_bytes"`
	// ItemCount is how much goes with it: for a folder, the descendants that
	// will be purged alongside. A restore or a purge of one row is rarely one
	// object and the number is what makes that visible before the click.
	ItemCount int `json:"item_count"`
}

// ListTrash returns the workspace's trash.
func (h *Handler) ListTrash(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workspaceID, err := httputil.RequirePathParam(r, "workspace_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	if _, ok := h.authorize(w, r, authz.WorkspaceObject(workspaceID), authz.CapRead); !ok {
		return
	}
	keys, err := h.authz.KeysFor(ctx, authz.UserSubject(authctx.UserID(ctx)), workspaceID)
	if err != nil {
		fail(w, err)
		return
	}
	entries, err := h.repo.Trash(ctx, workspaceID, keys)
	if err != nil {
		fail(w, err)
		return
	}
	httputil.JSON(w, http.StatusOK, entries)
}

// Restore brings a trashed object back.
func (h *Handler) Restore(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := authctx.UserID(ctx)

	objectType := httputil.PathParam(r, "object_type")
	id, err := httputil.RequirePathParam(r, "object_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	switch objectType {
	case EntryFolder, EntryFile:
	default:
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST",
			`object_type must be "folder" or "file"`)
		return
	}

	ref := authz.FolderObject(id)
	if objectType == EntryFile {
		ref = authz.FileObject(id)
	}
	if _, ok := h.authorize(w, r, ref, authz.CapWrite); !ok {
		return
	}

	result, err := h.repo.Restore(ctx, objectType, id, actor)
	if err != nil {
		fail(w, err)
		return
	}
	// Re-index what came back. The trash unindexed these; nothing else would
	// ever put them back, so a restored document was permanently unsearchable.
	for _, fid := range result.Restored {
		if file, err := h.repo.File(ctx, fid); err == nil && file != nil {
			h.events.PublishFile(ctx, ActionUpdated, file)
		}
	}
	h.record(ctx, result.WorkspaceID, "drive."+objectType+"_restored", objectType, id,
		map[string]interface{}{"relocated": result.Relocated})

	httputil.JSON(w, http.StatusOK, map[string]interface{}{
		"id":        id,
		"kind":      objectType,
		"folder_id": result.ParentID,
		// Stated, not silent. Restore walks up, and if any ancestor is gone the
		// object lands in the Drive root — which is a different place from where
		// the user deleted it, and they will look in the old one.
		"relocated": result.Relocated,
		"note":      result.Note,
	})
}

// EmptyTrash purges everything now.
func (h *Handler) EmptyTrash(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workspaceID, err := httputil.RequirePathParam(r, "workspace_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	// Admin, not write. Emptying the trash is irreversible and workspace-wide:
	// it destroys objects somebody else trashed and may still want back.
	if _, ok := h.authorize(w, r, authz.WorkspaceObject(workspaceID), authz.CapAdmin); !ok {
		return
	}

	purged, err := Purge(ctx, h.repo, h.storage, PurgeOptions{
		WorkspaceID: workspaceID,
		Now:         time.Now(),
		// Everything trashed, regardless of its schedule: the operator asked.
		IgnoreSchedule: true,
	}, h.logger())
	if err != nil {
		fail(w, err)
		return
	}
	// Unindex what was destroyed. This is the ONLY moment those ids still
	// exist: cmd/reindex upserts from live rows and has no prune pass, so a
	// purged document that is not unindexed here answers searches for its own
	// title and body forever, with no operation able to remove it.
	h.events.PublishFileDeletions(ctx, purged.Removed)
	h.record(ctx, workspaceID, "drive.trash_emptied", "workspace", workspaceID,
		map[string]interface{}{"files": purged.Files, "folders": purged.Folders,
			"bytes_freed": purged.BytesFreed})

	httputil.JSON(w, http.StatusOK, map[string]interface{}{
		"files_purged":   purged.Files,
		"folders_purged": purged.Folders,
		"bytes_freed":    purged.BytesFreed,
	})
}

func (h *Handler) logger() *slog.Logger {
	if h.events != nil && h.events.logger != nil {
		return h.events.logger
	}
	return slog.Default()
}

// ---------------------------------------------------------------------------
// Repository
// ---------------------------------------------------------------------------

// Trash lists the top of every trashed tree the caller may read.
func (r *Repository) Trash(ctx context.Context, workspaceID string, keys []string) ([]TrashEntry, error) {
	if len(keys) == 0 {
		return []TrashEntry{}, nil
	}
	rows, err := r.pool.Query(ctx, `
		SELECT 'folder' AS kind, f.id::text, f.name, f.trashed_at, f.trashed_by::text,
		       f.purge_after, 0::bigint AS size_bytes,
		       (SELECT count(*) FROM acl_object d
		         WHERE d.path LIKE (SELECT o.path FROM acl_object o
		                             WHERE o.object_type = 'folder' AND o.object_id = f.id) || '%')::int - 1
		         AS item_count
		  FROM drive_folders f
		 WHERE f.workspace_id = $1
		   AND f.trashed_at IS NOT NULL
		   -- Only the top of the tree: a folder whose parent is also trashed
		   -- went with its ancestor and is not a separate thing to restore.
		   AND NOT EXISTS (SELECT 1 FROM drive_folders p
		                    WHERE p.id = f.parent_id AND p.trashed_at IS NOT NULL)
		   AND EXISTS (SELECT 1 FROM acl_key k
		                WHERE k.object_type = 'folder' AND k.object_id = f.id AND k.key = ANY($2))
		UNION ALL
		SELECT 'file', f.id::text, f.name, f.trashed_at, f.trashed_by::text,
		       f.purge_after,
		       COALESCE((SELECT SUM(v.size_bytes) FROM file_versions v WHERE v.file_id = f.id), 0),
		       0
		  FROM files f
		 WHERE f.workspace_id = $1
		   AND f.trashed_at IS NOT NULL
		   AND NOT EXISTS (SELECT 1 FROM drive_folders p
		                    WHERE p.id = f.folder_id AND p.trashed_at IS NOT NULL)
		   AND EXISTS (SELECT 1 FROM acl_key k
		                WHERE k.object_type = 'file' AND k.object_id = f.id AND k.key = ANY($2))
		 ORDER BY 4 DESC`, workspaceID, keys)
	if err != nil {
		return nil, fmt.Errorf("list trash: %w", err)
	}
	defer rows.Close()

	out := []TrashEntry{}
	for rows.Next() {
		var e TrashEntry
		if err := rows.Scan(&e.Kind, &e.ID, &e.Name, &e.TrashedAt, &e.TrashedBy,
			&e.PurgeAfter, &e.SizeBytes, &e.ItemCount); err != nil {
			return nil, fmt.Errorf("scan trash entry: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RestoreResult reports where the object ended up.
type RestoreResult struct {
	WorkspaceID string
	ParentID    string
	Relocated   bool
	Note        string

	// Restored names every file that came back, so the caller can re-index it.
	//
	// Restoring published nothing at all, so a restored object was fully usable
	// and permanently unsearchable — the trash had unindexed it and no path put
	// it back until somebody ran cmd/reindex by hand.
	Restored []string
}

// Restore clears trashed_at on an object and its subtree.
//
// The subtree, because trashing took it: restoring only the top would produce a
// folder whose contents are all still in the trash — an empty shell where the
// user expects their files.
//
// If any ancestor is gone the object lands in the Drive ROOT and says so. The
// alternative is refusing, which leaves the user with a file they can see in the
// trash and cannot have.
func (r *Repository) Restore(ctx context.Context, objectType, id, actorID string) (RestoreResult, error) {
	var res RestoreResult

	err := database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		var (
			workspaceID string
			parentID    *string
			trashed     *time.Time
		)
		switch objectType {
		case EntryFolder:
			err := tx.QueryRow(ctx,
				`SELECT workspace_id::text, parent_id::text, trashed_at
				   FROM drive_folders WHERE id = $1 FOR UPDATE`, id).
				Scan(&workspaceID, &parentID, &trashed)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
		default:
			err := tx.QueryRow(ctx,
				`SELECT workspace_id::text, folder_id::text, trashed_at
				   FROM files WHERE id = $1 FOR UPDATE`, id).
				Scan(&workspaceID, &parentID, &trashed)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
		}
		if trashed == nil {
			// Already live. Not an error: a double-click on restore should be a
			// no-op rather than a 409.
			res = RestoreResult{WorkspaceID: workspaceID, ParentID: derefStr(parentID)}
			return nil
		}
		res.WorkspaceID = workspaceID

		// Walk up. A parent that is missing OR still trashed means there is
		// nowhere to put this back, so it goes to the root.
		target := derefStr(parentID)
		usable := target != ""
		if usable {
			var parentTrashed *time.Time
			err := tx.QueryRow(ctx,
				`SELECT trashed_at FROM drive_folders WHERE id = $1`, target).Scan(&parentTrashed)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				usable = false
			case err != nil:
				return err
			case parentTrashed != nil:
				usable = false
			}
		}
		if !usable {
			var rootID string
			if err := tx.QueryRow(ctx,
				`SELECT id::text FROM drive_folders WHERE workspace_id = $1 AND is_root`,
				workspaceID).Scan(&rootID); err != nil {
				return ErrNoRoot
			}
			target = rootID
			res.Relocated = true
			res.Note = "the original folder is gone, so this was restored to the Drive root"
		}
		res.ParentID = target

		if objectType == EntryFile {
			if _, err := tx.Exec(ctx, `
				UPDATE files
				   SET trashed_at = NULL, trashed_by = NULL, purge_after = NULL,
				       folder_id = $2, updated_by = $3
				 WHERE id = $1`, id, target, nullable(actorID)); err != nil {
				return fmt.Errorf("restore file: %w", err)
			}
			res.Restored = []string{id}
			return authz.MaterializeTx(ctx, tx, authz.FileObject(id))
		}

		// A folder, and its whole subtree. The path prefix is what makes that
		// one statement rather than a recursive walk.
		var path string
		if err := tx.QueryRow(ctx,
			`SELECT path FROM acl_object WHERE object_type = 'folder' AND object_id = $1`,
			id).Scan(&path); err != nil {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, `
			UPDATE drive_folders
			   SET trashed_at = NULL, trashed_by = NULL, purge_after = NULL
			 WHERE trashed_at IS NOT NULL
			   AND id IN (SELECT object_id FROM acl_object
			               WHERE object_type = 'folder' AND path LIKE $1 || '%')`,
			path); err != nil {
			return fmt.Errorf("restore folder subtree: %w", err)
		}
		rows, err := tx.Query(ctx, `
			UPDATE files
			   SET trashed_at = NULL, trashed_by = NULL, purge_after = NULL, updated_by = $2
			 WHERE trashed_at IS NOT NULL
			   AND folder_id IN (SELECT object_id FROM acl_object
			                      WHERE object_type = 'folder' AND path LIKE $1 || '%')
			RETURNING id::text`,
			path, nullable(actorID))
		if err != nil {
			return fmt.Errorf("restore folder contents: %w", err)
		}
		res.Restored = res.Restored[:0]
		for rows.Next() {
			var fid string
			if err := rows.Scan(&fid); err != nil {
				rows.Close()
				return fmt.Errorf("scan restored file: %w", err)
			}
			res.Restored = append(res.Restored, fid)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// The folder itself may have to move, and its parent decides its path.
		if _, err := tx.Exec(ctx,
			`UPDATE drive_folders SET parent_id = $2 WHERE id = $1`, id, target); err != nil {
			return fmt.Errorf("reparent restored folder: %w", err)
		}
		if res.Relocated {
			if err := r.authz.MoveTx(ctx, tx, authz.FolderObject(id), authz.FolderObject(target)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return RestoreResult{}, err
	}
	return res, nil
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ---------------------------------------------------------------------------
// The purge job
// ---------------------------------------------------------------------------

// PurgeOptions is one run.
type PurgeOptions struct {
	// WorkspaceID scopes the run. Empty means every workspace, which is what
	// the periodic job does.
	WorkspaceID string
	Now         time.Time
	// IgnoreSchedule purges everything trashed rather than only what is due.
	// Set by "empty trash now", never by the job.
	IgnoreSchedule bool
	// Batch bounds one run.
	Batch int
}

// PurgeResult reports what a run removed.
type PurgeResult struct {
	Files      int
	Folders    int
	BytesFreed int64
	// ObjectFailures is objects whose delete errored. Their rows are already
	// gone, so these are leaks the bucket sweep collects — counted so a
	// permanently unwritable bucket is visible rather than silent.
	ObjectFailures int

	// Removed names every file whose row is gone, so the caller can unindex it.
	//
	// THE INDEX CANNOT RECOVER FROM A PURGE ON ITS OWN. cmd/reindex only upserts
	// from live rows — there is no prune pass — so a purged document that was
	// never unindexed stays in Meilisearch permanently, with its title and its
	// body, answering searches for content that has been destroyed. This list
	// is the only moment the ids still exist.
	Removed []FileRef
}

// Purge permanently removes trashed objects.
//
// TWO ORDERINGS ARE LOAD-BEARING AND NEITHER IS INTERCHANGEABLE.
//
// FILES BEFORE FOLDERS, and folders leaves-first. drive_folders.parent_id is
// ON DELETE RESTRICT and RESTRICT is checked PER ROW rather than deferred to the
// end of the statement, so deleting a parent before its children fails — and
// files.folder_id is RESTRICT too, so a folder cannot go while it still holds
// one.
//
// ROWS BEFORE OBJECTS, which is the OPPOSITE of what file.Collect does, and the
// difference is the restore path. The collector deletes objects first because a
// row it cannot reach is a leak; here the row IS the thing the user might get
// back, so committing it first means a crash mid-run leaves objects with no rows
// — which the bucket sweep collects — rather than rows pointing at objects that
// are already gone, which is a restore that hands somebody a zero-byte file.
func Purge(ctx context.Context, repo *Repository, store storage.Backend,
	opts PurgeOptions, l *slog.Logger) (PurgeResult, error) {
	var res PurgeResult
	if l == nil {
		l = slog.Default()
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if opts.Batch <= 0 {
		opts.Batch = 500
	}

	type doomed struct {
		id          string
		fileType    string
		workspaceID string
		keys        []string
	}
	var files []doomed

	// One transaction for the rows, and the quota refund inside it: the version
	// rows are what the refund sums and they cascade away with the file.
	if err := database.WithTx(ctx, repo.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT f.id::text, f.storage_key, COALESCE(f.thumbnail_key, ''),
			       COALESCE(array_agg(v.storage_key) FILTER (WHERE v.storage_key IS NOT NULL), '{}'),
			       f.file_type, f.workspace_id::text
			  FROM files f
			  LEFT JOIN file_versions v ON v.file_id = f.id
			 WHERE f.trashed_at IS NOT NULL
			   AND ($1::uuid IS NULL OR f.workspace_id = $1)
			   AND ($2 OR (f.purge_after IS NOT NULL AND f.purge_after <= $3))
			 GROUP BY f.id
			 LIMIT $4`,
			nullableUUID(opts.WorkspaceID), opts.IgnoreSchedule, opts.Now, opts.Batch)
		if err != nil {
			return fmt.Errorf("list purgeable files: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var (
				id, head, thumb       string
				fileType, workspaceID string
				versionKeys           []string
			)
			if err := rows.Scan(&id, &head, &thumb, &versionKeys, &fileType, &workspaceID); err != nil {
				return fmt.Errorf("scan purgeable file: %w", err)
			}
			keys := map[string]bool{}
			for _, k := range append(versionKeys, head, thumb) {
				if k != "" {
					keys[k] = true
				}
			}
			d := doomed{id: id, fileType: fileType, workspaceID: workspaceID}
			for k := range keys {
				d.keys = append(d.keys, k)
			}
			files = append(files, d)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(files) == 0 {
			return nil
		}

		ids := make([]string, 0, len(files))
		for _, f := range files {
			ids = append(ids, f.id)
		}

		var freed int64
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(v.size_bytes), 0)
			  FROM file_versions v WHERE v.file_id = ANY($1)`, ids).Scan(&freed); err != nil {
			return fmt.Errorf("measure purged bytes: %w", err)
		}
		res.BytesFreed = freed

		// Refund BEFORE the delete: file_versions is ON DELETE CASCADE from
		// files, so afterwards there is nothing left to sum.
		if err := quota.RefundForFilesTx(ctx, tx, ids); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM files WHERE id = ANY($1)`, ids)
		if err != nil {
			return fmt.Errorf("purge files: %w", err)
		}
		res.Files = int(tag.RowsAffected())
		res.Removed = make([]FileRef, 0, len(files))
		for _, f := range files {
			res.Removed = append(res.Removed, FileRef{
				ID: f.id, FileType: f.fileType, WorkspaceID: f.workspaceID,
				// Terminal: the row is gone, so this file can never be purged
				// again and no later event can collide with it.
				Revision: "purge",
			})
		}
		return nil
	}); err != nil {
		return res, err
	}

	// Objects only after the rows are committed. A failure here is a leak the
	// bucket sweep collects, which is the cheap direction.
	for _, f := range files {
		for _, key := range f.keys {
			if store == nil {
				continue
			}
			if err := store.Delete(ctx, key); err != nil {
				l.Warn("purge: delete object", "key", key, "error", err)
				res.ObjectFailures++
			}
		}
	}

	// Folders, leaves-first, bounded by the depth cap.
	for depth := 0; depth <= MaxFolderDepth+1; depth++ {
		var removed int
		if err := database.WithTx(ctx, repo.pool, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `
				SELECT f.id::text
				  FROM drive_folders f
				 WHERE f.trashed_at IS NOT NULL
				   AND NOT f.is_root
				   AND ($1::uuid IS NULL OR f.workspace_id = $1)
				   AND ($2 OR (f.purge_after IS NOT NULL AND f.purge_after <= $3))
				   -- A leaf: nothing left referencing it. parent_id and
				   -- folder_id are both ON DELETE RESTRICT, and RESTRICT is
				   -- checked per row, so a non-leaf DELETE fails outright.
				   AND NOT EXISTS (SELECT 1 FROM drive_folders c WHERE c.parent_id = f.id)
				   AND NOT EXISTS (SELECT 1 FROM files fi WHERE fi.folder_id = f.id)
				 LIMIT $4`,
				nullableUUID(opts.WorkspaceID), opts.IgnoreSchedule, opts.Now, opts.Batch)
			if err != nil {
				return fmt.Errorf("list purgeable folders: %w", err)
			}
			defer rows.Close()

			ids := []string{}
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return err
				}
				ids = append(ids, id)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if len(ids) == 0 {
				return nil
			}
			for _, id := range ids {
				// acl_object has no foreign key to drive_folders — it cannot,
				// object_id is polymorphic — so nothing removes these rows on
				// their own, and a grant on a folder that no longer exists keeps
				// answering the "what could this person reach" audit.
				if err := repo.authz.UnregisterTx(ctx, tx, authz.FolderObject(id)); err != nil {
					return err
				}
			}
			tag, err := tx.Exec(ctx, `DELETE FROM drive_folders WHERE id = ANY($1)`, ids)
			if err != nil {
				return fmt.Errorf("purge folders: %w", err)
			}
			removed = int(tag.RowsAffected())
			return nil
		}); err != nil {
			return res, err
		}
		if removed == 0 {
			break
		}
		res.Folders += removed
	}

	if res.Files > 0 || res.Folders > 0 {
		l.Info("drive trash purged", "files", res.Files, "folders", res.Folders,
			"bytes_freed", res.BytesFreed, "object_failures", res.ObjectFailures)
	}
	return res, nil
}

func nullableUUID(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
