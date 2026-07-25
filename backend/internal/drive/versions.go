package drive

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/drive/registry"
	"github.com/Wick-Lim/SuperOps/backend/internal/file"
	"github.com/Wick-Lim/SuperOps/backend/internal/quota"
	"github.com/Wick-Lim/SuperOps/backend/internal/storage"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

// Version is one entry in a file's history.
type Version struct {
	FileID      string    `json:"file_id"`
	Version     int       `json:"version"`
	SizeBytes   int64     `json:"size_bytes"`
	ContentType string    `json:"content_type"`
	Checksum    string    `json:"checksum,omitempty"`
	CreatedBy   *string   `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	IsCurrent   bool      `json:"is_current"`

	storageKey string
}

// versionable resolves a file and refuses the operation for a type whose bytes
// the server cannot replace.
//
// The refusal is 409 rather than 400 or a silent success, and it is the same
// answer in all four routes. A PUT into a CRDT-backed object would be discarded
// by the next merge, so accepting bytes we intend to drop is the worst of the
// three possible answers — the client believes it saved.
//
// A file whose type is not registered on THIS deployment can still LIST its
// versions and download them: an editor somebody turned off must not make
// existing data unreachable. It just cannot accept new bytes, because nothing
// here knows whether it should.
func (h *Handler) versionable(w http.ResponseWriter, r *http.Request, forWrite bool) (*File, bool) {
	id, err := httputil.RequirePathParam(r, "file_id")
	if err != nil {
		httputil.HandleError(w, err)
		return nil, false
	}
	want := authz.CapRead
	if forWrite {
		want = authz.CapWrite
	}
	if _, ok := h.authorize(w, r, authz.FileObject(id), want); !ok {
		return nil, false
	}
	f, err := h.repo.File(r.Context(), id)
	if err != nil {
		fail(w, err)
		return nil, false
	}
	if f == nil {
		fail(w, ErrNotFound)
		return nil, false
	}
	if forWrite {
		kind, known := h.kinds.Lookup(f.FileType)
		if !known || !kind.Versioned {
			fail(w, ErrNotVersioned)
			return nil, false
		}
		if f.TrashedAt != nil {
			fail(w, ErrTrashed)
			return nil, false
		}
	}
	return f, true
}

// ListVersions returns the history, newest first.
func (h *Handler) ListVersions(w http.ResponseWriter, r *http.Request) {
	f, ok := h.versionable(w, r, false)
	if !ok {
		return
	}
	versions, err := h.repo.Versions(r.Context(), f.ID)
	if err != nil {
		fail(w, err)
		return
	}
	httputil.JSON(w, http.StatusOK, versions)
}

// VersionContent serves one historical version.
func (h *Handler) VersionContent(w http.ResponseWriter, r *http.Request) {
	f, ok := h.versionable(w, r, false)
	if !ok {
		return
	}
	n, err := strconv.Atoi(httputil.PathParam(r, "version"))
	if err != nil || n <= 0 {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "version must be a positive integer")
		return
	}
	v, err := h.repo.Version(r.Context(), f.ID, n)
	if err != nil {
		fail(w, err)
		return
	}
	if v == nil {
		fail(w, ErrNotFound)
		return
	}
	if h.storage == nil || v.storageKey == "" {
		fail(w, ErrNotFound)
		return
	}

	// Presigned as an attachment with an opaque type, like every other original.
	// A presigned URL cannot carry a CSP header, so forcing the download is what
	// keeps user content from rendering on the bucket's origin.
	url, err := h.storage.PresignGet(r.Context(), v.storageKey, presignTTL, storage.PresignOptions{
		ContentType: "application/octet-stream",
		Disposition: httputil.ContentDisposition(
			fmt.Sprintf("%s.v%d%s", trimExt(f.Name), n, path.Ext(f.Name)), false),
	})
	if err != nil || url == "" {
		httputil.HandleError(w, httputil.NewInternal(fmt.Errorf("presign version %d of %s: %w", n, f.ID, err)))
		return
	}
	http.Redirect(w, r, url, http.StatusFound)
}

func trimExt(name string) string {
	if ext := path.Ext(name); ext != "" {
		return name[:len(name)-len(ext)]
	}
	return name
}

// ReplaceContent stores a new version.
//
// The file id is STABLE across versions, and that is what the whole design is
// for: a message attachment, a collab_documents.resource_id and a search
// document all keep pointing at the same thing when somebody uploads a revision.
// A restore that minted a new id would break all three at once.
func (h *Handler) ReplaceContent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := authctx.UserID(ctx)

	f, ok := h.versionable(w, r, true)
	if !ok {
		return
	}
	if h.storage == nil {
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

	contentType, err := httputil.SniffContentType(part)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	// A NEW object under a NEW key. Overwriting the old key would destroy the
	// version the history is about to offer to restore, and the bucket has no
	// undo.
	storageKey := file.StorageKey(f.WorkspaceID, newUUID(), path.Ext(f.Name))
	if err := h.storage.Put(ctx, storageKey, part, header.Size, contentType); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	var out *File
	if err := database.WithTx(ctx, h.repo.pool, func(tx pgx.Tx) error {
		// FOR UPDATE on the files row: two concurrent uploads must serialise
		// rather than both compute the same next version number and collide on
		// the primary key — which would surface to one of them as a 500 for a
		// request that was perfectly legal.
		var maxVersion int
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE((SELECT MAX(version) FROM file_versions WHERE file_id = f.id), 0)
			   FROM files f WHERE f.id = $1 FOR UPDATE OF f`, f.ID).Scan(&maxVersion); err != nil {
			return err
		}
		next := maxVersion + 1

		if _, err := quota.ChargeTx(ctx, tx, f.WorkspaceID, header.Size); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO file_versions (file_id, version, storage_key, size_bytes, content_type, created_by)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			f.ID, next, storageKey, header.Size, contentType, actor); err != nil {
			return err
		}
		// thumbnail_key is cleared: the old preview is of the old bytes, and
		// serving it beside new content is worse than serving none.
		if _, err := tx.Exec(ctx, `
			UPDATE files
			   SET storage_key = $2, size_bytes = $3, content_type = $4,
			       current_version = $5, thumbnail_key = NULL, updated_by = $6
			 WHERE id = $1`,
			f.ID, storageKey, header.Size, contentType, next, actor); err != nil {
			return err
		}
		var scanErr error
		out, scanErr = scanFile(tx.QueryRow(ctx, `SELECT `+fileColumns+` FROM files WHERE id = $1`, f.ID))
		return scanErr
	}); err != nil {
		_ = h.storage.Delete(ctx, storageKey)
		if errors.Is(err, quota.ErrExceeded) {
			httputil.JSONError(w, http.StatusInsufficientStorage, "QUOTA_EXCEEDED",
				"the workspace has no storage left; a new version is charged in addition to the old one")
			return
		}
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	h.record(ctx, f.WorkspaceID, "drive.version_created", "file", f.ID,
		map[string]interface{}{"version": out.Version, "size_bytes": header.Size})
	h.events.PublishFile(ctx, ActionUpdated, out)
	// New bytes, new preview. thumbnail_key was cleared in the transaction, so
	// until this lands the file simply has none — which is correct, and better
	// than a preview of what it used to be.
	h.events.RequestThumbnail(ctx, out)

	desc, err := h.describe(ctx, out, authz.CapAdmin)
	if err != nil {
		fail(w, err)
		return
	}
	httputil.JSON(w, http.StatusCreated, desc)
}

// RestoreVersion makes an existing version current again.
//
// It MOVES THE HEAD POINTER and writes no new version row. That is deliberate,
// and the reason is accounting: quota's invariant I1 sums file_versions, so a
// roll-forward restore that copied the row would add its bytes to the total
// while storing no new object — a workspace that filled up by pressing undo.
//
// Nothing is lost by it. The history already holds every version, and "restore
// v2" means v2 is the head again; a subsequent upload takes MAX(version)+1, so
// the numbering never collides with a version the head has been rolled back
// past.
func (h *Handler) RestoreVersion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	actor := authctx.UserID(ctx)

	f, ok := h.versionable(w, r, true)
	if !ok {
		return
	}
	n, err := strconv.Atoi(httputil.PathParam(r, "version"))
	if err != nil || n <= 0 {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "version must be a positive integer")
		return
	}

	var out *File
	if err := database.WithTx(ctx, h.repo.pool, func(tx pgx.Tx) error {
		var (
			key         string
			size        int64
			contentType string
		)
		err := tx.QueryRow(ctx, `
			SELECT v.storage_key, v.size_bytes, v.content_type
			  FROM file_versions v
			  JOIN files f ON f.id = v.file_id
			 WHERE v.file_id = $1 AND v.version = $2
			   FOR UPDATE OF f`, f.ID, n).Scan(&key, &size, &contentType)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE files
			   SET storage_key = $2, size_bytes = $3, content_type = $4,
			       current_version = $5, thumbnail_key = NULL, updated_by = $6
			 WHERE id = $1`,
			f.ID, key, size, contentType, n, actor); err != nil {
			return err
		}
		var scanErr error
		out, scanErr = scanFile(tx.QueryRow(ctx, `SELECT `+fileColumns+` FROM files WHERE id = $1`, f.ID))
		return scanErr
	}); err != nil {
		fail(w, err)
		return
	}

	h.record(ctx, f.WorkspaceID, "drive.version_restored", "file", f.ID,
		map[string]interface{}{"version": n})
	h.events.PublishFile(ctx, ActionUpdated, out)
	h.events.RequestThumbnail(ctx, out)

	desc, err := h.describe(ctx, out, authz.CapAdmin)
	if err != nil {
		fail(w, err)
		return
	}
	httputil.JSON(w, http.StatusOK, desc)
}

// ---------------------------------------------------------------------------
// Repository
// ---------------------------------------------------------------------------

const versionColumns = `v.file_id::text, v.version, v.size_bytes, v.content_type, v.checksum,
	v.created_by::text, v.created_at, v.storage_key, (v.version = f.current_version) AS is_current`

func scanVersion(row pgx.Row) (*Version, error) {
	var v Version
	err := row.Scan(&v.FileID, &v.Version, &v.SizeBytes, &v.ContentType, &v.Checksum,
		&v.CreatedBy, &v.CreatedAt, &v.storageKey, &v.IsCurrent)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan file version: %w", err)
	}
	return &v, nil
}

// Versions returns a file's whole history, newest first.
//
// Not paginated. A version list is bounded by how many times a human pressed
// upload on one file, and a cursor over it would be ceremony — if that ever
// stops being true it is a product problem (unbounded history) before it is a
// pagination one.
func (r *Repository) Versions(ctx context.Context, fileID string) ([]Version, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+versionColumns+`
		   FROM file_versions v JOIN files f ON f.id = v.file_id
		  WHERE v.file_id = $1 ORDER BY v.version DESC`, fileID)
	if err != nil {
		return nil, fmt.Errorf("list file versions: %w", err)
	}
	defer rows.Close()

	out := []Version{}
	for rows.Next() {
		var v Version
		if err := rows.Scan(&v.FileID, &v.Version, &v.SizeBytes, &v.ContentType, &v.Checksum,
			&v.CreatedBy, &v.CreatedAt, &v.storageKey, &v.IsCurrent); err != nil {
			return nil, fmt.Errorf("scan file version: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r *Repository) Version(ctx context.Context, fileID string, n int) (*Version, error) {
	return scanVersion(r.pool.QueryRow(ctx,
		`SELECT `+versionColumns+`
		   FROM file_versions v JOIN files f ON f.id = v.file_id
		  WHERE v.file_id = $1 AND v.version = $2`, fileID, n))
}

// registryVersioned is here so the compile fails if the registry ever stops
// exposing the flag the four routes above branch on.
var _ = func(k registry.Kind) bool { return k.Versioned }
