package file

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/audit"
	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/quota"
	"github.com/Wick-Lim/SuperOps/backend/internal/storage"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

const (
	maxUploadSize = 50 << 20 // 50MB, enforced on the wire by MaxBytesReader
	// multipartMemory bounds how much of a multipart request is held in RAM.
	// It must NOT be maxUploadSize: ParseMultipartForm treats its argument as
	// maxMemory, so passing the size cap buffered every 50MB upload entirely in
	// the heap and a handful of concurrent uploads OOM'd the process. With a
	// small value the file part spills to a temp file and is streamed to MinIO
	// from there.
	multipartMemory = 1 << 20 // 1MB
	// sniffLen is the number of leading bytes http.DetectContentType inspects.
	sniffLen = 512
)

// Auditor records egress. *audit.Service is the implementation; the interface
// keeps this package from importing it for one method and makes "nil means no
// auditing" a compile-time-visible choice rather than a config lookup.
type Auditor interface {
	Buffer(ctx context.Context, e audit.Entry)
}

type Handler struct {
	storage storage.Backend
	pool    *pgxpool.Pool
	authz   *authz.Checker
	audit   Auditor
}

func NewHandler(storage storage.Backend, pool *pgxpool.Pool, az *authz.Checker, auditor Auditor) *Handler {
	return &Handler{storage: storage, pool: pool, authz: az, audit: auditor}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("POST /api/v1/files/upload", authMw(http.HandlerFunc(h.Upload)))
	mux.Handle("GET /api/v1/files/{file_id}", authMw(http.HandlerFunc(h.Download)))
	mux.Handle("DELETE /api/v1/files/{file_id}", authMw(http.HandlerFunc(h.Delete)))
}

// --- content type policy -----------------------------------------------------

// inlineTypes may be rendered by the browser on our own origin. The SPA and the
// API share an origin in the shipped deployment and the client passes its access
// token in the URL query, so anything scriptable served inline is stored XSS
// plus token exfiltration. text/html, image/svg+xml and friends are absent on
// purpose; everything not listed is forced to a download.
var inlineTypes = map[string]bool{
	"image/png":                true,
	"image/jpeg":               true,
	"image/gif":                true,
	"image/webp":               true,
	"image/bmp":                true,
	"image/x-icon":             true,
	"image/vnd.microsoft.icon": true,
	"application/pdf":          true,
	"text/plain":               true,
	"audio/mpeg":               true,
	"audio/wav":                true,
	"video/mp4":                true,
	"video/webm":               true,
}

// mediaType strips parameters and normalises case. An unparseable value is
// treated as the octet-stream default rather than passed through.
func mediaType(ct string) string {
	base, _, err := mime.ParseMediaType(ct)
	if err != nil || base == "" {
		return "application/octet-stream"
	}
	return strings.ToLower(base)
}

// canServeInline reports whether a content type may be sent with
// `Content-Disposition: inline`.
func canServeInline(ct string) bool {
	return inlineTypes[mediaType(ct)]
}

// sniffContentType classifies the upload server-side. The multipart part header
// is attacker-controlled and was previously echoed straight back on download,
// so it is discarded entirely.
func sniffContentType(f io.ReadSeeker) (string, error) {
	buf := make([]byte, sniffLen)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", fmt.Errorf("read upload head: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind upload: %w", err)
	}
	return mediaType(http.DetectContentType(buf[:n])), nil
}

// --- handlers ----------------------------------------------------------------

func (h *Handler) Upload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := authctx.UserID(ctx)

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(multipartMemory); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httputil.JSONError(w, http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE", "file too large (max 50MB)")
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

	file, header, err := r.FormFile("file")
	if err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "file is required")
		return
	}
	defer file.Close()

	workspaceID := r.FormValue("workspace_id")
	if workspaceID == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "workspace_id is required")
		return
	}
	member, err := h.authz.Can(ctx, authz.UserSubject(userID), authz.WorkspaceObject(workspaceID), authz.CapRead)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !member {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "not a member of this workspace")
		return
	}

	contentType, err := sniffContentType(file)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	name := httputil.SanitizeFileName(header.Filename)
	fileID := uuid.NewString()
	ext := filepath.Ext(name)
	storageKey := StorageKey(workspaceID, fileID, ext)

	// file is a multipart.File: either the small in-memory buffer or a temp file
	// on disk. Either way this streams to MinIO instead of materialising the
	// whole object in the heap.
	if err := h.storage.Put(ctx, storageKey, file, header.Size, contentType); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	// The row and its ACL are one commit. A files row with no acl_key rows is
	// invisible to every list path — including search — until the hourly drift
	// job runs, and then it appears on its own. MaterializeTx runs the same
	// views that job does, filtered to this object.
	// The row, its version row, its ACL and the quota charge are ONE commit.
	//
	// The charge belongs here rather than on the Drive route alone: a quota any
	// member can bypass by posting to /api/v1/files/upload is not enforcement.
	// The version row belongs here because quota's invariant I1 sums
	// file_versions — a file with no version row is silently free storage.
	if err := database.WithTx(ctx, h.pool, func(tx pgx.Tx) error {
		if _, err := quota.ChargeTx(ctx, tx, workspaceID, header.Size); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO files (id, workspace_id, user_id, name, content_type, size_bytes, storage_key)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			fileID, workspaceID, userID, name, contentType, header.Size, storageKey,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO file_versions (file_id, version, storage_key, size_bytes, content_type, created_by)
			 VALUES ($1, 1, $2, $3, $4, $5)`,
			fileID, storageKey, header.Size, contentType, userID); err != nil {
			return err
		}
		return authz.MaterializeTx(ctx, tx, authz.FileObject(fileID))
	}); err != nil {
		// Do not leave the object behind if the metadata row could not be written.
		_ = h.storage.Delete(ctx, storageKey)
		if errors.Is(err, quota.ErrExceeded) {
			httputil.JSONError(w, http.StatusInsufficientStorage, "QUOTA_EXCEEDED",
				"the workspace has no storage left; free space or raise the quota")
			return
		}
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusCreated, map[string]interface{}{
		"id":           fileID,
		"name":         name,
		"content_type": contentType,
		"size_bytes":   header.Size,
		"storage_key":  storageKey,
	})
}

// fileRow is the authorization-relevant projection of a files row.
type fileRow struct {
	ID          string
	WorkspaceID string
	UserID      string
	MessageID   *string
	Name        string
	ContentType string
	StorageKey  string
	CreatedAt   time.Time
}

func (h *Handler) getFile(r *http.Request, id string) (*fileRow, error) {
	var f fileRow
	err := h.pool.QueryRow(r.Context(),
		`SELECT id, workspace_id, user_id, message_id, name, content_type, storage_key, created_at
		   FROM files WHERE id = $1`, id,
	).Scan(&f.ID, &f.WorkspaceID, &f.UserID, &f.MessageID, &f.Name, &f.ContentType, &f.StorageKey, &f.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get file: %w", err)
	}
	return &f, nil
}

// canRead asks the object model whether the caller may read this file.
//
// A file is an object with a place in the hierarchy — it hangs off the channel
// of the message it is attached to, or off its workspace while it is still
// unattached — so the whole of the old three-step dance (resolve the message,
// resolve its channel, authorize the channel, fall back to uploader-only when
// the message was hard-deleted) is the checker's file arm, and this is now one
// call. The rule it enforces is unchanged and is still not workspace-level:
// workspace-level authorization is what let any member fetch a file posted in a
// private channel or a DM given only its UUID.
//
// One deliberate narrowing comes with the move. The checker joins the message's
// channel on the FILE's workspace, so a files row pointing at a message in
// another tenant — corrupt data, not a reachable state — degrades to
// uploader-only instead of authorizing against the foreign channel. Fail-closed
// is the only sane reading of a cross-workspace attachment.
func (h *Handler) canRead(r *http.Request, f *fileRow, userID string) (bool, error) {
	return h.authz.Can(r.Context(), authz.UserSubject(userID), authz.FileObject(f.ID), authz.CapRead)
}

func (h *Handler) Download(w http.ResponseWriter, r *http.Request) {
	fileID := r.PathValue("file_id")
	userID := authctx.UserID(r.Context())

	f, err := h.getFile(r, fileID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if f == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "file not found")
		return
	}

	ok, err := h.canRead(r, f, userID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if !ok {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "not allowed to read this file")
		return
	}

	reader, _, err := h.storage.Get(r.Context(), f.StorageKey)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	defer reader.Close()

	// Egress. This is the category an auditor actually asks about and the one
	// that had no coverage at all before plan 01.
	//
	// Buffered (Tier 2), because a synchronous INSERT here is a latency
	// regression on a content path. Coalesced, because fifty downloads of one
	// file in an afternoon is one fact with a count, not fifty rows — and that
	// single decision is most of what keeps audit_logs smaller than `messages`.
	// It is recorded AFTER the authorization check and BEFORE the bytes go out,
	// so a download that fails mid-stream is still recorded as attempted.
	if h.audit != nil {
		h.audit.Buffer(r.Context(), audit.Entry{
			WorkspaceID:  f.WorkspaceID,
			ActorID:      userID,
			Action:       audit.ActionFileDownloaded,
			ResourceType: "file",
			ResourceID:   f.ID,
			Metadata: map[string]interface{}{
				// The name and size, never the contents. metadata carries no
				// content, ever: it lives in an append-only table with a
				// 365-day retention and no redaction path.
				"name": f.Name, "content_type": f.ContentType,
			},
			Coalesce: true,
		})
	}

	// Serve the type we sniffed at upload time, never the one MinIO echoes back
	// from the original request, and never without nosniff.
	contentType := mediaType(f.ContentType)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", httputil.ContentDisposition(f.Name, canServeInline(contentType)))
	// Defence in depth for the types that are still rendered inline.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; media-src 'self'; object-src 'none'; frame-ancestors 'none'; sandbox")
	w.Header().Set("Referrer-Policy", "no-referrer")

	// Range requests and conditional GETs need a seeker and a real modtime;
	// time.Now() made Last-Modified useless and the assertion was unchecked.
	if rs, ok := reader.(io.ReadSeeker); ok {
		http.ServeContent(w, r, f.Name, f.CreatedAt, rs)
		return
	}
	if _, err := io.Copy(w, reader); err != nil {
		return
	}
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	fileID := r.PathValue("file_id")
	userID := authctx.UserID(ctx)

	f, err := h.getFile(r, fileID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if f == nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "file not found")
		return
	}

	// Uploader, or an owner/admin of the workspace acting as a moderator.
	if f.UserID != userID {
		admin, err := h.authz.Can(ctx, authz.UserSubject(userID), authz.WorkspaceObject(f.WorkspaceID), authz.CapAdmin)
		if err != nil {
			httputil.HandleError(w, httputil.NewInternal(err))
			return
		}
		if !admin {
			httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "only the uploader or a workspace admin can delete this file")
			return
		}
	}

	// Object first: RemoveObject is idempotent, so a failure here leaves the row
	// in place and the delete can simply be retried. Dropping the row first and
	// failing here would strand the object with nothing pointing at it.
	if err := h.storage.Delete(ctx, f.StorageKey); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	// The refund is in the same transaction as the DELETE, and it reads the
	// version rows BEFORE they cascade away — file_versions is ON DELETE CASCADE
	// from files, so after the DELETE there is nothing left to sum.
	var tag pgconn.CommandTag
	if err := database.WithTx(ctx, h.pool, func(tx pgx.Tx) error {
		if err := quota.RefundForFilesTx(ctx, tx, []string{fileID}); err != nil {
			return err
		}
		var execErr error
		tag, execErr = tx.Exec(ctx, `DELETE FROM files WHERE id = $1`, fileID)
		return execErr
	}); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	if tag.RowsAffected() == 0 {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "file not found")
		return
	}

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "file deleted"})
}
