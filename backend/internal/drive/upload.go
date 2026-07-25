package drive

import (
	"errors"
	"fmt"
	"net/http"
	"path"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/file"
	"github.com/Wick-Lim/SuperOps/backend/internal/quota"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

const (
	// MaxUploadBytes is the ceiling on one Drive upload, enforced on the wire by
	// MaxBytesReader so a hostile client cannot make the server buffer more than
	// this before it is refused. Higher than the chat-attachment limit (50MB)
	// because Drive is where a design file or a video actually lives.
	MaxUploadBytes = 100 << 20

	// multipartMemory bounds how much of a multipart request is held in RAM.
	// It must NOT be MaxUploadBytes: ParseMultipartForm treats its argument as
	// maxMemory, so passing the size cap buffers every upload entirely in the
	// heap. Anything above this spills to a temp file.
	multipartMemory = 1 << 20
)

// UploadFile stores bytes into a Drive folder.
//
// The transaction is the same four-part shape file.Handler.Upload uses, and it
// has to be: charge, row, version row, ACL. Any one of them outside the
// transaction is a way for the four to disagree — an uncharged file, a file with
// no version row (invisible to the quota invariant, therefore free storage), or
// a file missing from its own folder's listing.
func (h *Handler) UploadFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := authctx.UserID(ctx)

	workspaceID, err := httputil.RequirePathParam(r, "workspace_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	if h.storage == nil {
		// Drive's routes are registered unconditionally while the chat-attachment
		// routes are not, so this is the one place a deployment without object
		// storage reaches a handler that needs it. 503, because the request is
		// fine and the capability is absent.
		httputil.JSONError(w, http.StatusServiceUnavailable, "STORAGE_NOT_CONFIGURED",
			"object storage is not configured on this deployment")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, MaxUploadBytes)
	if err := r.ParseMultipartForm(multipartMemory); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httputil.JSONError(w, http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE",
				fmt.Sprintf("file too large (max %dMB)", MaxUploadBytes>>20))
			return
		}
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid multipart body")
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	part, header, err := r.FormFile("file")
	if err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "missing file field")
		return
	}
	defer part.Close()

	folderID := r.FormValue("folder_id")
	if folderID == "" {
		root, err := h.repo.EnsureRoot(ctx, workspaceID, actor)
		if err != nil || root == nil {
			fail(w, orNoRoot(err))
			return
		}
		folderID = root.ID
	}
	// Write on the FOLDER: uploading is a write into the container, and the
	// container is where the capability lives.
	if _, ok := h.authorize(w, r, authz.FolderObject(folderID), authz.CapWrite); !ok {
		return
	}

	folder, err := h.repo.Folder(ctx, folderID)
	if err != nil {
		fail(w, err)
		return
	}
	switch {
	case folder == nil:
		fail(w, ErrNotFound)
		return
	case folder.WorkspaceID != workspaceID:
		fail(w, ErrCrossWorkspace)
		return
	case folder.TrashedAt != nil:
		fail(w, ErrTrashed)
		return
	}

	// Classified server-side, from the bytes. The part header is
	// attacker-controlled and never reaches a response.
	contentType, err := httputil.SniffContentType(part)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	name := httputil.SanitizeFileName(header.Filename)
	if v := r.FormValue("name"); v != "" {
		name = httputil.SanitizeFileName(v)
	}

	fileID := newUUID()
	storageKey := file.StorageKey(workspaceID, fileID, path.Ext(name))

	// Object first. The row is what makes the object reachable, so an object
	// with no row is a leak the collector cleans up, while a row with no object
	// is a download that 404s forever.
	if err := h.storage.Put(ctx, storageKey, part, header.Size, contentType); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	var out *File
	if err := database.WithTx(ctx, h.repo.pool, func(tx pgx.Tx) error {
		if _, err := quota.ChargeTx(ctx, tx, workspaceID, header.Size); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO files (id, workspace_id, user_id, folder_id, name, file_type,
			                   content_type, size_bytes, storage_key, updated_by)
			VALUES ($1, $2, $3, $4, $5, 'file', $6, $7, $8, $3)`,
			fileID, workspaceID, actor, folderID, name, contentType, header.Size, storageKey); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO file_versions (file_id, version, storage_key, size_bytes, content_type, created_by)
			VALUES ($1, 1, $2, $3, $4, $5)`,
			fileID, storageKey, header.Size, contentType, actor); err != nil {
			return err
		}
		if err := authz.MaterializeTx(ctx, tx, authz.FileObject(fileID)); err != nil {
			return err
		}
		var scanErr error
		out, scanErr = scanFile(tx.QueryRow(ctx, `SELECT `+fileColumns+` FROM files WHERE id = $1`, fileID))
		return scanErr
	}); err != nil {
		// The object is only reachable through the row, so a failed transaction
		// must take it with it.
		_ = h.storage.Delete(ctx, storageKey)
		if errors.Is(err, quota.ErrExceeded) {
			usage, readErr := quota.Read(ctx, h.repo.pool, workspaceID)
			if readErr == nil {
				httputil.JSON(w, http.StatusInsufficientStorage, map[string]interface{}{
					"error": map[string]interface{}{
						"code": "QUOTA_EXCEEDED",
						"message": "the workspace has no storage left; empty the trash, " +
							"delete old versions, or raise the quota",
					},
					"quota_bytes":     usage.QuotaBytes,
					"bytes_used":      usage.BytesUsed,
					"available_bytes": usage.Available(),
				})
				return
			}
			httputil.JSONError(w, http.StatusInsufficientStorage, "QUOTA_EXCEEDED",
				"the workspace has no storage left")
			return
		}
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	h.record(ctx, workspaceID, "drive.file_uploaded", "file", fileID,
		map[string]interface{}{"name": name, "size_bytes": header.Size, "folder_id": folderID})
	h.events.PublishFile(ctx, ActionUploaded, out)

	desc, err := h.describe(ctx, out, authz.CapAdmin)
	if err != nil {
		fail(w, err)
		return
	}
	httputil.JSON(w, http.StatusCreated, desc)
}

// Usage reports storage. The two halves are reported SEPARATELY with their own
// timestamps and there is deliberately no total: one is exact at every instant
// an upload can observe it and the other is recomputed by a job, and a single
// number would have an accuracy nobody could state.
func (h *Handler) Usage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workspaceID, err := httputil.RequirePathParam(r, "workspace_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	capability, ok := h.authorize(w, r, authz.WorkspaceObject(workspaceID), authz.CapRead)
	if !ok {
		return
	}

	usage, err := quota.Read(ctx, h.repo.pool, workspaceID)
	if err != nil {
		fail(w, err)
		return
	}

	payload := map[string]interface{}{
		"workspace_id":    usage.WorkspaceID,
		"quota_bytes":     usage.QuotaBytes,
		"available_bytes": usage.Available(),
		"blob": map[string]interface{}{
			"bytes":       usage.BytesUsed,
			"as_of":       usage.UpdatedAt,
			"consistency": "exact",
		},
		"collab": map[string]interface{}{
			"bytes":       usage.CollabBytes,
			"as_of":       usage.CollabBytesAt,
			"consistency": "eventual",
		},
		// Stated rather than implied. Users do not expect either of these and
		// both are the first support question a full workspace produces.
		"counts_trashed_files": true,
		"counts_old_versions":  true,
	}

	// The breakdown is admin-only: it is a full scan of the workspace's files,
	// and drift_bytes is an operational number rather than a user-facing one.
	if capability.Implies(authz.CapAdmin) {
		b, err := quota.ReadBreakdown(ctx, h.repo.pool, workspaceID)
		if err != nil {
			fail(w, err)
			return
		}
		payload["breakdown"] = b
	}

	httputil.JSON(w, http.StatusOK, payload)
}

// SetQuota caps a workspace. Admin only, and 0 means unlimited.
func (h *Handler) SetQuota(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	workspaceID, err := httputil.RequirePathParam(r, "workspace_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	var req struct {
		QuotaBytes *int64 `json:"quota_bytes"`
	}
	if err := httputil.DecodeJSON(r, &req); err != nil {
		httputil.HandleError(w, err)
		return
	}
	if req.QuotaBytes == nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST",
			"quota_bytes is required; send 0 for unlimited")
		return
	}
	if _, ok := h.authorize(w, r, authz.WorkspaceObject(workspaceID), authz.CapAdmin); !ok {
		return
	}

	if err := quota.SetQuota(ctx, h.repo.pool, workspaceID, *req.QuotaBytes); err != nil {
		if *req.QuotaBytes < 0 {
			httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST",
				"a negative quota is not meaningful; send 0 for unlimited")
			return
		}
		fail(w, err)
		return
	}
	h.record(ctx, workspaceID, "drive.quota_set", "workspace", workspaceID,
		map[string]interface{}{"quota_bytes": *req.QuotaBytes})

	usage, err := quota.Read(ctx, h.repo.pool, workspaceID)
	if err != nil {
		fail(w, err)
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]interface{}{
		"workspace_id":    usage.WorkspaceID,
		"quota_bytes":     usage.QuotaBytes,
		"bytes_used":      usage.BytesUsed,
		"available_bytes": usage.Available(),
	})
}

// newUUID mints the file id. The files row's DEFAULT cannot be used: the
// storage key embeds the id, so it has to exist before the INSERT.
func newUUID() string { return uuid.NewString() }
