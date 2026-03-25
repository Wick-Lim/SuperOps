package file

import (
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

const maxUploadSize = 50 << 20 // 50MB

type Handler struct {
	storage *Storage
	pool    *pgxpool.Pool
}

func NewHandler(storage *Storage, pool *pgxpool.Pool) *Handler {
	return &Handler{storage: storage, pool: pool}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("POST /api/v1/files/upload", authMw(http.HandlerFunc(h.Upload)))
	mux.Handle("GET /api/v1/files/{file_id}", authMw(http.HandlerFunc(h.Download)))
	mux.Handle("DELETE /api/v1/files/{file_id}", authMw(http.HandlerFunc(h.Delete)))
}

func (h *Handler) Upload(w http.ResponseWriter, r *http.Request) {
	userID := authctx.UserID(r.Context())

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "file too large (max 50MB)")
		return
	}

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

	fileID := uuid.NewString()
	ext := filepath.Ext(header.Filename)
	storageKey := fmt.Sprintf("%s/%s/%s%s", workspaceID, time.Now().Format("2006/01/02"), fileID, ext)
	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	if err := h.storage.Upload(r.Context(), storageKey, file, header.Size, contentType); err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	// Save metadata to DB
	_, err = h.pool.Exec(r.Context(),
		`INSERT INTO files (id, workspace_id, user_id, name, content_type, size_bytes, storage_key)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		fileID, workspaceID, userID, header.Filename, contentType, header.Size, storageKey,
	)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	httputil.JSON(w, http.StatusCreated, map[string]interface{}{
		"id":           fileID,
		"name":         header.Filename,
		"content_type": contentType,
		"size_bytes":   header.Size,
		"storage_key":  storageKey,
	})
}

func (h *Handler) Download(w http.ResponseWriter, r *http.Request) {
	fileID := r.PathValue("file_id")

	var name, contentType, storageKey string
	err := h.pool.QueryRow(r.Context(),
		`SELECT name, content_type, storage_key FROM files WHERE id = $1`, fileID,
	).Scan(&name, &contentType, &storageKey)
	if err != nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "file not found")
		return
	}

	reader, ct, err := h.storage.Download(r.Context(), storageKey)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	defer reader.Close()

	if ct != "" {
		contentType = ct
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="%s"`, name))

	http.ServeContent(w, r, name, time.Now(), reader.(readSeeker))
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	fileID := r.PathValue("file_id")
	userID := authctx.UserID(r.Context())

	var storageKey string
	var ownerID string
	err := h.pool.QueryRow(r.Context(),
		`SELECT storage_key, user_id FROM files WHERE id = $1`, fileID,
	).Scan(&storageKey, &ownerID)
	if err != nil {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "file not found")
		return
	}

	if ownerID != userID {
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "can only delete your own files")
		return
	}

	h.storage.Delete(r.Context(), storageKey)
	h.pool.Exec(r.Context(), `DELETE FROM files WHERE id = $1`, fileID)

	httputil.JSON(w, http.StatusOK, map[string]string{"message": "file deleted"})
}

type readSeeker interface {
	Read(p []byte) (n int, err error)
	Seek(offset int64, whence int) (int64, error)
}
