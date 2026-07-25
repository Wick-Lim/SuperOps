package drive

import (
	"context"
	"errors"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/audit"
	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/drive/registry"
	"github.com/Wick-Lim/SuperOps/backend/internal/storage"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

// presignTTL bounds a content URL. The URL IS the grant for its lifetime — a
// revoked share does not invalidate one already issued, which is a permanent
// property of presigning rather than an oversight — so it is short.
const presignTTL = 5 * time.Minute

// CollabLookup resolves a file to its collaborative document. Drive knows the
// file id; internal/collab owns the mapping. Declared here so the dependency
// runs drive -> interface <- collab.
type CollabLookup interface {
	DocumentIDForResource(ctx context.Context, resourceType, resourceID string) (string, error)
}

// CollabRevoker drops live editor sessions when a share is withdrawn.
//
// A separate interface from CollabLookup because it is satisfied by a different
// type — the lookup is the collab repository, this is the collab service, which
// is the only thing holding the hub — and because a nil one must degrade to the
// old behaviour rather than to a panic. A deployment with no collaboration
// layer has no rooms to revoke.
//
// It takes an acl_object PATH, not an id: revoking a share on a folder must cut
// every editor session beneath it, and the path is what expresses "beneath".
type CollabRevoker interface {
	// RevokeFileAccess cuts userID (or everyone, when empty) out of every
	// editor room at or under path. Returns the count and whether the fan-out
	// was truncated.
	RevokeFileAccess(ctx context.Context, userID, path string) (int, bool)
}

type Handler struct {
	repo    *Repository
	authz   *authz.Checker
	kinds   *registry.Registry
	storage storage.Backend
	collab  CollabLookup
	revoker CollabRevoker
	audit   *audit.Service
	events  *Publisher
}

func NewHandler(pool *pgxpool.Pool, az *authz.Checker, kinds *registry.Registry,
	store storage.Backend, collab CollabLookup, auditor *audit.Service, events *Publisher) *Handler {
	return &Handler{
		repo:    NewRepository(pool, az, kinds),
		authz:   az,
		kinds:   kinds,
		storage: store,
		collab:  collab,
		audit:   auditor,
		events:  events,
	}
}

// SetCollabRevoker is late binding, for the same reason app.go's liveRevoker is:
// the collaboration service authorizes through the checker and Drive revokes
// through the collaboration service, so one of the two references has to be
// filled in after construction.
func (h *Handler) SetCollabRevoker(r CollabRevoker) { h.revoker = r }

func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	// The registry is served to every authenticated caller so the client
	// hardcodes nothing: which editors exist is a deployment decision.
	mux.Handle("GET /api/v1/drive/registry", authMw(http.HandlerFunc(h.Registry)))

	mux.Handle("GET /api/v1/workspaces/{workspace_id}/drive/root", authMw(http.HandlerFunc(h.Root)))
	mux.Handle("POST /api/v1/workspaces/{workspace_id}/drive/folders", authMw(http.HandlerFunc(h.CreateFolder)))
	mux.Handle("POST /api/v1/workspaces/{workspace_id}/drive/files", authMw(http.HandlerFunc(h.CreateFile)))
	mux.Handle("POST /api/v1/workspaces/{workspace_id}/drive/files/upload", authMw(http.HandlerFunc(h.UploadFile)))
	mux.Handle("GET /api/v1/workspaces/{workspace_id}/drive/usage", authMw(http.HandlerFunc(h.Usage)))
	mux.Handle("PUT /api/v1/workspaces/{workspace_id}/drive/quota", authMw(http.HandlerFunc(h.SetQuota)))
	mux.Handle("GET /api/v1/workspaces/{workspace_id}/drive/trash", authMw(http.HandlerFunc(h.ListTrash)))
	mux.Handle("DELETE /api/v1/workspaces/{workspace_id}/drive/trash", authMw(http.HandlerFunc(h.EmptyTrash)))
	mux.Handle("POST /api/v1/drive/{object_type}/{object_id}/restore", authMw(http.HandlerFunc(h.Restore)))

	mux.Handle("GET /api/v1/drive/folders/{folder_id}", authMw(http.HandlerFunc(h.GetFolder)))
	mux.Handle("GET /api/v1/drive/folders/{folder_id}/children", authMw(http.HandlerFunc(h.ListChildren)))
	mux.Handle("PATCH /api/v1/drive/folders/{folder_id}", authMw(http.HandlerFunc(h.RenameFolder)))
	mux.Handle("POST /api/v1/drive/folders/{folder_id}/move", authMw(http.HandlerFunc(h.MoveFolder)))
	mux.Handle("DELETE /api/v1/drive/folders/{folder_id}", authMw(http.HandlerFunc(h.TrashFolder)))

	mux.Handle("GET /api/v1/drive/files/{file_id}", authMw(http.HandlerFunc(h.GetFile)))
	mux.Handle("PATCH /api/v1/drive/files/{file_id}", authMw(http.HandlerFunc(h.RenameFile)))
	mux.Handle("POST /api/v1/drive/files/{file_id}/move", authMw(http.HandlerFunc(h.MoveFile)))
	mux.Handle("DELETE /api/v1/drive/files/{file_id}", authMw(http.HandlerFunc(h.TrashFile)))
	mux.Handle("POST /api/v1/drive/files/{file_id}/content", authMw(http.HandlerFunc(h.ReplaceContent)))
	// The projection: the client-published rendering of a document the server
	// cannot read. In internal/drive rather than in a per-editor package,
	// because docs, spreadsheets and designs need the identical thing and three
	// implementations of it is the failure the registry exists to prevent.
	mux.Handle("POST /api/v1/drive/files/{file_id}/projection", authMw(http.HandlerFunc(h.PutProjection)))
	// Embeds resolve PER CALLER. An embed node carries {ref_type, ref_id} and
	// nothing else, so a document shared with somebody who cannot read its
	// targets renders placeholders rather than leaking their names.
	mux.Handle("POST /api/v1/drive/files/{file_id}/refs/resolve", authMw(http.HandlerFunc(h.ResolveRefs)))
	mux.Handle("GET /api/v1/drive/files/{file_id}/backlinks", authMw(http.HandlerFunc(h.ListFileBacklinks)))
	mux.Handle("GET /api/v1/drive/refs/{ref_type}/{ref_id}/files", authMw(http.HandlerFunc(h.ListBacklinks)))
	// The anchor half of a comment on a range. The comment itself is
	// internal/comment's, unchanged — see ruling 7.
	mux.Handle("PUT /api/v1/drive/files/{file_id}/comments/{comment_id}/anchor",
		authMw(http.HandlerFunc(h.PutAnchor)))
	mux.Handle("GET /api/v1/drive/files/{file_id}/anchors", authMw(http.HandlerFunc(h.ListAnchors)))

	mux.Handle("GET /api/v1/drive/{object_type}/{object_id}/shares", authMw(http.HandlerFunc(h.ListShares)))
	mux.Handle("PUT /api/v1/drive/{object_type}/{object_id}/shares", authMw(http.HandlerFunc(h.PutShare)))
	mux.Handle("DELETE /api/v1/drive/{object_type}/{object_id}/shares/{subject_type}/{subject_id}",
		authMw(http.HandlerFunc(h.DeleteShare)))
	mux.Handle("POST /api/v1/drive/{object_type}/{object_id}/links", authMw(http.HandlerFunc(h.CreateLink)))
	mux.Handle("GET /api/v1/drive/{object_type}/{object_id}/links", authMw(http.HandlerFunc(h.ListLinks)))
	mux.Handle("DELETE /api/v1/drive/links/{link_id}", authMw(http.HandlerFunc(h.RevokeLink)))
	mux.Handle("GET /api/v1/drive/files/{file_id}/versions", authMw(http.HandlerFunc(h.ListVersions)))
	mux.Handle("GET /api/v1/drive/files/{file_id}/versions/{version}/content", authMw(http.HandlerFunc(h.VersionContent)))
	mux.Handle("POST /api/v1/drive/files/{file_id}/versions/{version}/restore", authMw(http.HandlerFunc(h.RestoreVersion)))
}

// RegisterLinkRoutes is separate from RegisterRoutes because this one route is
// UNAUTHENTICATED and needs a rate limiter the others do not: its path contains
// a guessable secret, so the composition root wraps it in a FIXED-bucket
// limiter. See ratelimit.MiddlewareByIPBucket.
func (h *Handler) RegisterLinkRoutes(mux *http.ServeMux, mw func(http.Handler) http.Handler) {
	// POST, not GET. A token in a GET lands in a Referer, in browser history
	// and in a prefetch.
	mux.Handle("POST /api/v1/drive/links/{token}/resolve", mw(http.HandlerFunc(h.ResolveLink)))
}

func (h *Handler) Registry(w http.ResponseWriter, _ *http.Request) {
	httputil.JSON(w, http.StatusOK, h.kinds.Descriptors())
}

// ---------------------------------------------------------------------------
// Authorization helpers
// ---------------------------------------------------------------------------
//
// Every handler below resolves ONE object and asks Can once. That is the
// per-object path and it is correct here; the LIST path never calls it — see
// Repository.Children.

// authorize resolves the caller's capability on an object and refuses below
// want. It answers 404 rather than 403 for something the caller cannot read at
// all, because a 403 on an object you cannot see confirms it exists.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, obj authz.ObjectRef, want authz.Capability) (authz.Capability, bool) {
	actor := authctx.UserID(r.Context())
	got, err := h.authz.Capability(r.Context(), authz.UserSubject(actor), obj)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return authz.CapNone, false
	}
	switch {
	case !got.Implies(authz.CapRead):
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "object not found")
		return authz.CapNone, false
	case !got.Implies(want):
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "insufficient capability for this action")
		return got, false
	}
	return got, true
}

// fail maps this package's sentinels onto statuses. One place, so a new caller
// cannot invent a different answer for the same condition.
func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrNoRoot):
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "object not found")
	case errors.Is(err, ErrCycle):
		httputil.JSONError(w, http.StatusConflict, "MOVE_CYCLE",
			"a folder cannot be moved into its own subtree")
	case errors.Is(err, ErrTooDeep):
		httputil.JSONError(w, http.StatusConflict, "TREE_TOO_DEEP",
			"the folder tree would exceed the maximum depth")
	case errors.Is(err, ErrTrashed):
		httputil.JSONError(w, http.StatusConflict, "OBJECT_TRASHED",
			"the object is in the trash; restore it first")
	case errors.Is(err, ErrCrossWorkspace):
		httputil.JSONError(w, http.StatusBadRequest, "CROSS_WORKSPACE",
			"objects cannot move between workspaces")
	case errors.Is(err, ErrNotVersioned):
		httputil.JSONError(w, http.StatusConflict, "NOT_VERSIONED",
			"this object's content is edited collaboratively; uploaded bytes would be "+
				"discarded by the next merge")
	case errors.Is(err, ErrLinkInvalid):
		// ONE answer for unknown, revoked, expired and used-up alike.
		// Distinguishing them tells somebody walking the token space which
		// guesses landed on a real row.
		httputil.JSONError(w, http.StatusNotFound, "LINK_INVALID",
			"this link is not valid")
	case errors.Is(err, ErrLinkPassword):
		httputil.JSONError(w, http.StatusUnauthorized, "LINK_PASSWORD_REQUIRED",
			"this link requires a password")
	case errors.Is(err, ErrEscalation):
		httputil.JSONError(w, http.StatusForbidden, "ESCALATION", err.Error())
	case errors.Is(err, ErrNotAMember):
		httputil.JSONError(w, http.StatusBadRequest, "NOT_A_MEMBER", err.Error())
	case errors.Is(err, ErrUnknownType):
		httputil.JSONError(w, http.StatusBadRequest, "UNKNOWN_FILE_TYPE",
			"no editor is registered for that file type on this deployment")
	case IsBadInput(err):
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
	default:
		httputil.HandleError(w, httputil.NewInternal(err))
	}
}

// ---------------------------------------------------------------------------
// Folders
// ---------------------------------------------------------------------------

func (h *Handler) Root(w http.ResponseWriter, r *http.Request) {
	workspaceID, err := httputil.RequirePathParam(r, "workspace_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	// Membership first: EnsureRoot creates a row, and creating one for a
	// workspace the caller is not in would be a write by a stranger.
	if _, ok := h.authorize(w, r, authz.WorkspaceObject(workspaceID), authz.CapRead); !ok {
		return
	}
	root, err := h.repo.EnsureRoot(r.Context(), workspaceID, authctx.UserID(r.Context()))
	if err != nil {
		fail(w, err)
		return
	}
	if root == nil {
		fail(w, ErrNoRoot)
		return
	}
	httputil.JSON(w, http.StatusOK, root)
}

func (h *Handler) GetFolder(w http.ResponseWriter, r *http.Request) {
	id, err := httputil.RequirePathParam(r, "folder_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	if _, ok := h.authorize(w, r, authz.FolderObject(id), authz.CapRead); !ok {
		return
	}
	folder, err := h.repo.Folder(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	if folder == nil {
		fail(w, ErrNotFound)
		return
	}
	crumbs, err := h.repo.Breadcrumb(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]interface{}{
		"folder":     folder,
		"breadcrumb": crumbs,
	})
}

func (h *Handler) ListChildren(w http.ResponseWriter, r *http.Request) {
	id, err := httputil.RequirePathParam(r, "folder_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	page, err := httputil.ParsePagination(r)
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	if _, ok := h.authorize(w, r, authz.FolderObject(id), authz.CapRead); !ok {
		return
	}

	folder, err := h.repo.Folder(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	if folder == nil {
		fail(w, ErrNotFound)
		return
	}

	// ONE key set for the whole page, not one Can per row. This is the call
	// docs/plans/00-permissions.md exists to make possible.
	keys, err := h.authz.KeysFor(r.Context(),
		authz.UserSubject(authctx.UserID(r.Context())), folder.WorkspaceID)
	if err != nil {
		fail(w, err)
		return
	}

	entries, cursor, hasMore, err := h.repo.Children(r.Context(), id, keys, page)
	if err != nil {
		fail(w, err)
		return
	}
	httputil.JSONList(w, http.StatusOK, entries, cursor, hasMore)
}

type folderCreateRequest struct {
	ParentID string `json:"parent_id"`
	Name     string `json:"name"`
}

func (h *Handler) CreateFolder(w http.ResponseWriter, r *http.Request) {
	workspaceID, err := httputil.RequirePathParam(r, "workspace_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	var req folderCreateRequest
	if err := httputil.DecodeJSON(r, &req); err != nil {
		httputil.HandleError(w, err)
		return
	}

	parentID := req.ParentID
	if parentID == "" {
		root, err := h.repo.EnsureRoot(r.Context(), workspaceID, authctx.UserID(r.Context()))
		if err != nil || root == nil {
			fail(w, orNoRoot(err))
			return
		}
		parentID = root.ID
	}
	// Write on the PARENT, not on the workspace: creating a folder is a write
	// into the container, and the container is where the capability lives.
	if _, ok := h.authorize(w, r, authz.FolderObject(parentID), authz.CapWrite); !ok {
		return
	}

	folder, err := h.repo.CreateFolder(r.Context(), workspaceID, parentID, req.Name, authctx.UserID(r.Context()))
	if err != nil {
		fail(w, err)
		return
	}
	h.record(r.Context(), workspaceID, "drive.folder_created", "folder", folder.ID,
		map[string]interface{}{"name": folder.Name, "parent_id": parentID})
	httputil.JSON(w, http.StatusCreated, folder)
}

type renameRequest struct {
	Name string `json:"name"`
}

func (h *Handler) RenameFolder(w http.ResponseWriter, r *http.Request) {
	id, err := httputil.RequirePathParam(r, "folder_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	var req renameRequest
	if err := httputil.DecodeJSON(r, &req); err != nil {
		httputil.HandleError(w, err)
		return
	}
	if _, ok := h.authorize(w, r, authz.FolderObject(id), authz.CapWrite); !ok {
		return
	}
	folder, err := h.repo.RenameFolder(r.Context(), id, req.Name)
	if err != nil {
		fail(w, err)
		return
	}
	h.record(r.Context(), folder.WorkspaceID, "drive.folder_renamed", "folder", folder.ID,
		map[string]interface{}{"name": folder.Name})
	httputil.JSON(w, http.StatusOK, folder)
}

type moveRequest struct {
	ParentID string `json:"parent_id"`
}

func (h *Handler) MoveFolder(w http.ResponseWriter, r *http.Request) {
	id, err := httputil.RequirePathParam(r, "folder_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	var req moveRequest
	if err := httputil.DecodeJSON(r, &req); err != nil {
		httputil.HandleError(w, err)
		return
	}
	if req.ParentID == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "parent_id is required")
		return
	}
	// BOTH ends. Write on the thing being moved is not enough — moving an
	// object INTO a folder is a write to that folder, and checking only the
	// source lets somebody file their own document into a folder they may only
	// read.
	if _, ok := h.authorize(w, r, authz.FolderObject(id), authz.CapWrite); !ok {
		return
	}
	if _, ok := h.authorize(w, r, authz.FolderObject(req.ParentID), authz.CapWrite); !ok {
		return
	}

	folder, err := h.repo.MoveFolder(r.Context(), authctx.UserID(r.Context()), id, req.ParentID)
	if err != nil {
		fail(w, err)
		return
	}
	h.record(r.Context(), folder.WorkspaceID, "drive.folder_moved", "folder", folder.ID,
		map[string]interface{}{"parent_id": req.ParentID})
	httputil.JSON(w, http.StatusOK, folder)
}

func (h *Handler) TrashFolder(w http.ResponseWriter, r *http.Request) {
	id, err := httputil.RequirePathParam(r, "folder_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	folder, err := h.repo.Folder(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	if folder == nil {
		fail(w, ErrNotFound)
		return
	}
	// Write, not share. Trashing is an edit — it is reversible from the trash
	// and it changes no grant — and requiring share would mean nobody could
	// tidy up a Drive that is shared with the workspace at write.
	if _, ok := h.authorize(w, r, authz.FolderObject(id), authz.CapWrite); !ok {
		return
	}
	if err := h.repo.TrashFolder(r.Context(), id, authctx.UserID(r.Context())); err != nil {
		fail(w, err)
		return
	}
	h.record(r.Context(), folder.WorkspaceID, "drive.folder_trashed", "folder", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Files
// ---------------------------------------------------------------------------

type fileCreateRequest struct {
	FolderID string `json:"folder_id"`
	Name     string `json:"name"`
	FileType string `json:"file_type"`
}

func (h *Handler) CreateFile(w http.ResponseWriter, r *http.Request) {
	workspaceID, err := httputil.RequirePathParam(r, "workspace_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	var req fileCreateRequest
	if err := httputil.DecodeJSON(r, &req); err != nil {
		httputil.HandleError(w, err)
		return
	}

	folderID := req.FolderID
	if folderID == "" {
		root, err := h.repo.EnsureRoot(r.Context(), workspaceID, authctx.UserID(r.Context()))
		if err != nil || root == nil {
			fail(w, orNoRoot(err))
			return
		}
		folderID = root.ID
	}
	if _, ok := h.authorize(w, r, authz.FolderObject(folderID), authz.CapWrite); !ok {
		return
	}

	file, err := h.repo.CreateFile(r.Context(), workspaceID, folderID, req.Name, req.FileType,
		authctx.UserID(r.Context()))
	if err != nil {
		fail(w, err)
		return
	}
	h.record(r.Context(), workspaceID, "drive.file_created", "file", file.ID,
		map[string]interface{}{"name": file.Name, "file_type": file.FileType, "folder_id": folderID})
	h.events.PublishFile(r.Context(), ActionUploaded, file)

	// The descriptor, not the row: the client's next action is to open it, and
	// a create that answered with less would force an immediate second request.
	desc, err := h.describe(r.Context(), file, authz.CapAdmin)
	if err != nil {
		fail(w, err)
		return
	}
	httputil.JSON(w, http.StatusCreated, desc)
}

// GetFile returns the OPEN DESCRIPTOR — the whole dispatch contract in one
// payload (docs/plans/02-drive.md §4).
func (h *Handler) GetFile(w http.ResponseWriter, r *http.Request) {
	id, err := httputil.RequirePathParam(r, "file_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	capability, ok := h.authorize(w, r, authz.FileObject(id), authz.CapRead)
	if !ok {
		return
	}
	file, err := h.repo.File(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	if file == nil {
		fail(w, ErrNotFound)
		return
	}
	desc, err := h.describe(r.Context(), file, capability)
	if err != nil {
		fail(w, err)
		return
	}
	httputil.JSON(w, http.StatusOK, desc)
}

// describe builds the open descriptor. Exactly one of CollabDocumentID and
// ContentURL is non-null, and which one is decided by the registry rather than
// by the file's MIME type — the client never branches on a media type.
func (h *Handler) describe(ctx context.Context, file *File, capability authz.Capability) (*Descriptor, error) {
	kind, ok := h.kinds.Lookup(file.FileType)
	if !ok {
		// A file whose type is not registered on THIS deployment. Its bytes are
		// still there and it is still listable; it simply has no surface, which
		// is a better answer than a 500 for an editor somebody turned off.
		kind = registry.Kind{Type: file.FileType, Storage: registry.StorageBlob}
	}

	desc := &Descriptor{
		File:        *file,
		StorageMode: string(kind.Storage),
		Capability:  capability.String(),
	}

	switch kind.Storage {
	case registry.StorageCollab:
		// How far the searchable body has fallen behind the log. The client
		// re-projects on open when the gap is non-zero, which is the only
		// backstop that exists for a document edited and closed before its
		// debounce fired — the server cannot produce content and pretending
		// otherwise needs the Node sidecar this design rejects.
		status, err := h.repo.ProjectionStatusFor(ctx, file.ID)
		if err != nil {
			return nil, err
		}
		desc.Projection = status
		if h.collab != nil {
			docID, err := h.collab.DocumentIDForResource(ctx, file.FileType, file.ID)
			if err != nil {
				return nil, err
			}
			if docID != "" {
				desc.CollabDocumentID = &docID
			}
		}
		// ContentURL stays null. There is no byte stream the server can produce
		// that IS the document — see plan 02 §8. A materialized blob kept
		// "up to date" without a server-side merge is how you hand a user a
		// three-day-old copy and call it a download.
	default:
		if url := h.presign(ctx, file); url != "" {
			desc.ContentURL = &url
		}
	}

	if file.HasThumb {
		if url := h.presignThumb(ctx, file); url != "" {
			desc.ThumbnailURL = &url
		}
	}
	return desc, nil
}

// presign mints a download URL, or "" when the backend cannot presign — the
// client then falls back to the proxied route rather than seeing an error.
//
// ALWAYS an attachment with an opaque type. A presigned URL cannot carry a CSP
// header, so forcing the download is what keeps user-uploaded content from
// rendering on the bucket's origin. See storage.PresignOptions.
func (h *Handler) presign(ctx context.Context, file *File) string {
	if h.storage == nil || file.storageKey == "" {
		return ""
	}
	url, err := h.storage.PresignGet(ctx, file.storageKey, presignTTL, storage.PresignOptions{
		ContentType: "application/octet-stream",
		Disposition: httputil.ContentDisposition(file.Name, false),
	})
	if err != nil {
		return ""
	}
	return url
}

// thumbMediaTypes is the closed set of thumbnail formats and the type each is
// served as. A key whose extension is absent gets NO presigned URL.
//
// Closed, and fail-closed, because the thumbnail is the ONE object Drive serves
// INLINE from a presigned URL — where no CSP header can travel. The safety
// property is that the served type is single, known-safe, and produced by the
// server. Deriving it from the key rather than hardcoding one value is what
// keeps that true if the generator's format ever changes: the previous version
// of this function labelled ANY key found in the column image/webp, which would
// have served a JPEG (or anything else) under a lie.
//
// ".webp" is reserved for the day a pure-Go WebP encoder is worth the
// dependency; golang.org/x/image/webp is decode-only today, which is why
// internal/thumb writes JPEG.
var thumbMediaTypes = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
}

func (h *Handler) presignThumb(ctx context.Context, file *File) string {
	if h.storage == nil {
		return ""
	}
	var key string
	if err := h.repo.pool.QueryRow(ctx,
		`SELECT COALESCE(thumbnail_key, '') FROM files WHERE id = $1`, file.ID).Scan(&key); err != nil || key == "" {
		return ""
	}
	mediaType, known := thumbMediaTypes[strings.ToLower(path.Ext(key))]
	if !known {
		return ""
	}
	url, err := h.storage.PresignGet(ctx, key, presignTTL, storage.PresignOptions{
		ContentType: mediaType,
		Disposition: "inline",
	})
	if err != nil {
		return ""
	}
	return url
}

func (h *Handler) RenameFile(w http.ResponseWriter, r *http.Request) {
	id, err := httputil.RequirePathParam(r, "file_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	var req renameRequest
	if err := httputil.DecodeJSON(r, &req); err != nil {
		httputil.HandleError(w, err)
		return
	}
	if _, ok := h.authorize(w, r, authz.FileObject(id), authz.CapWrite); !ok {
		return
	}
	file, err := h.repo.RenameFile(r.Context(), id, req.Name, authctx.UserID(r.Context()))
	if err != nil {
		fail(w, err)
		return
	}
	h.record(r.Context(), file.WorkspaceID, "drive.file_renamed", "file", file.ID,
		map[string]interface{}{"name": file.Name})
	h.events.PublishFile(r.Context(), ActionUpdated, file)
	httputil.JSON(w, http.StatusOK, file)
}

func (h *Handler) MoveFile(w http.ResponseWriter, r *http.Request) {
	id, err := httputil.RequirePathParam(r, "file_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	var req struct {
		FolderID string `json:"folder_id"`
	}
	if err := httputil.DecodeJSON(r, &req); err != nil {
		httputil.HandleError(w, err)
		return
	}
	if req.FolderID == "" {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "folder_id is required")
		return
	}
	if _, ok := h.authorize(w, r, authz.FileObject(id), authz.CapWrite); !ok {
		return
	}
	if _, ok := h.authorize(w, r, authz.FolderObject(req.FolderID), authz.CapWrite); !ok {
		return
	}
	file, err := h.repo.MoveFile(r.Context(), id, req.FolderID, authctx.UserID(r.Context()))
	if err != nil {
		fail(w, err)
		return
	}
	h.record(r.Context(), file.WorkspaceID, "drive.file_moved", "file", file.ID,
		map[string]interface{}{"folder_id": req.FolderID})
	h.events.PublishFile(r.Context(), ActionUpdated, file)
	httputil.JSON(w, http.StatusOK, file)
}

func (h *Handler) TrashFile(w http.ResponseWriter, r *http.Request) {
	id, err := httputil.RequirePathParam(r, "file_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	file, err := h.repo.File(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	if file == nil {
		fail(w, ErrNotFound)
		return
	}
	if _, ok := h.authorize(w, r, authz.FileObject(id), authz.CapWrite); !ok {
		return
	}
	if err := h.repo.TrashFile(r.Context(), id, authctx.UserID(r.Context())); err != nil {
		fail(w, err)
		return
	}
	h.record(r.Context(), file.WorkspaceID, "drive.file_trashed", "file", id, nil)
	// Trashed, not purged — but it must leave the index now. A trashed file that
	// stays searchable is the user finding what they just deleted.
	h.events.PublishFile(r.Context(), ActionDeleted, file)
	w.WriteHeader(http.StatusNoContent)
}

// record writes an audit entry, best effort. Try rather than Log: the action
// has already happened, so losing the trail must be visible in the logs without
// failing a request that succeeded.
func (h *Handler) record(ctx context.Context, workspaceID, action, resourceType, resourceID string, meta map[string]interface{}) {
	if h.audit == nil {
		return
	}
	h.audit.Try(ctx, audit.Entry{
		WorkspaceID:  workspaceID,
		ActorID:      authctx.UserID(ctx),
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		Metadata:     meta,
	})
}

func orNoRoot(err error) error {
	if err != nil {
		return err
	}
	return ErrNoRoot
}
