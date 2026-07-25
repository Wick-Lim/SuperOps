package drive

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

// Embeds are the security face of "the server cannot read the document".
//
// A document embeds a Drive file. The document is shared with somebody who
// cannot read that file. The body is an opaque blob the server cannot filter,
// so the only defence is that THE BODY NEVER CONTAINS ANYTHING WORTH LEAKING.
//
// The rule, enforced in the schema and here: an embed node carries
// {ref_type, ref_id} and nothing else. No title, no filename, no thumbnail URL,
// no issue summary. The client renders a placeholder and asks this endpoint,
// which resolves each ref against THE CALLER's capability and answers either a
// preview or {"access":"denied"} with no title field present at all.
//
// What that buys:
//
//   - The document renders with grey placeholders for a caller who cannot read
//     the target. That is the correct UX and also the honest one.
//   - Copying a document, sharing it into a channel or exporting it cannot leak
//     a title, because no copy of the title exists outside the target object.
//   - file_projection_refs is a STORED CLAIM BY A CLIENT and authorizes nothing.
//     It finds candidates; every one of them is checked here.
//
// What it does not buy: a user who types the file's name as literal text has
// leaked it. That is not a problem we can solve and not one to pretend to.

// maxResolveRefs bounds one resolve request. A document may hold 1000 refs but
// only what is on screen needs resolving, and each one costs a capability
// check.
const maxResolveRefs = 100

// refResolver names one ref type: how to build its authz object, and how to
// read its title once the caller is allowed to see it.
type refResolver struct {
	// object builds the acl_object to check. nil means this type is not an
	// acl_object at all — see sharesWorkspace.
	object func(id string) authz.ObjectRef
	// sharesWorkspace resolves by co-membership instead of by capability. A
	// `user` ref is a legitimate @mention and is NOT an acl_object type, so
	// handing it to Capability returns ErrNotRegistered and a handler that did
	// not special-case it would answer 500 for a perfectly ordinary mention.
	sharesWorkspace bool
	title           string
}

var refResolvers = map[string]refResolver{
	authz.TypeFile:   {object: authz.FileObject, title: `SELECT name FROM files WHERE id = $1`},
	authz.TypeFolder: {object: authz.FolderObject, title: `SELECT name FROM drive_folders WHERE id = $1`},
	authz.TypeIssue:  {object: authz.IssueObject, title: `SELECT title FROM issues WHERE id = $1`},
	"user":           {sharesWorkspace: true, title: `SELECT COALESCE(NULLIF(full_name, ''), username) FROM users WHERE id = $1`},
}

// ResolvedRef is one answer.
//
// Title is a POINTER and omitted when access is denied. Not "" — an empty
// string in the JSON would tell a caller the field exists and happens to be
// blank, and the difference between "you may not see this" and "this has no
// name" is the difference this whole endpoint is built on.
type ResolvedRef struct {
	RefType string  `json:"ref_type"`
	RefID   string  `json:"ref_id"`
	Access  string  `json:"access"` // "granted" | "denied"
	Title   *string `json:"title,omitempty"`
}

const (
	accessGranted = "granted"
	accessDenied  = "denied"
)

// ResolveRefs is POST /api/v1/drive/files/{file_id}/refs/resolve.
func (h *Handler) ResolveRefs(w http.ResponseWriter, r *http.Request) {
	fileID, err := httputil.RequirePathParam(r, "file_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	// Reading the document is enough to ask what its embeds resolve to — the
	// answers are per-caller, so a reader learns nothing a writer would not.
	if _, ok := h.authorize(w, r, authz.FileObject(fileID), authz.CapRead); !ok {
		return
	}

	var req struct {
		Refs []ProjectionRef `json:"refs"`
	}
	if err := httputil.DecodeJSONLimit(r, &req, 1<<16); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}
	if len(req.Refs) > maxResolveRefs {
		httputil.JSONError(w, http.StatusBadRequest, "TOO_MANY_REFS",
			fmt.Sprintf("resolve at most %d refs per request", maxResolveRefs))
		return
	}

	ctx := r.Context()
	actor := authctx.UserID(ctx)
	out := make([]ResolvedRef, 0, len(req.Refs))
	for _, ref := range req.Refs {
		out = append(out, h.resolveRef(ctx, actor, ref))
	}
	httputil.JSON(w, http.StatusOK, out)
}

// resolveRef answers one ref. It FAILS CLOSED on every path: an unknown type, a
// malformed id, an authorization error and a missing row all produce "denied"
// with no title, because the only thing worse than a placeholder is a title
// leaked by an error path.
func (h *Handler) resolveRef(ctx context.Context, actor string, ref ProjectionRef) ResolvedRef {
	denied := ResolvedRef{RefType: ref.RefType, RefID: ref.RefID, Access: accessDenied}

	resolver, known := refResolvers[ref.RefType]
	if !known {
		// A ref type this deployment does not resolve. Denied, not 500: a
		// client is free to put a node in a document that this server has never
		// heard of, and the honest answer is a placeholder.
		return denied
	}

	var (
		allowed bool
		err     error
	)
	switch {
	case resolver.sharesWorkspace:
		allowed, err = h.sharesAWorkspace(ctx, actor, ref.RefID)
	default:
		var got authz.Capability
		got, err = h.authz.Capability(ctx, authz.UserSubject(actor), resolver.object(ref.RefID))
		allowed = got.Implies(authz.CapRead)
	}
	if err != nil || !allowed {
		return denied
	}

	var title string
	if err := h.repo.pool.QueryRow(ctx, resolver.title, ref.RefID).Scan(&title); err != nil {
		// The target is gone, or was never there. Denied rather than granted
		// with an empty title: a deleted file's embed should render as a dead
		// placeholder, not as a nameless live one.
		return denied
	}
	return ResolvedRef{RefType: ref.RefType, RefID: ref.RefID, Access: accessGranted, Title: &title}
}

// sharesAWorkspace is the `user` ref's authorization: you may see somebody's
// display name if you are in a workspace with them. Nothing narrower is
// available — a user is not an acl_object — and nothing wider is acceptable,
// because a mention would otherwise expose the directory of every tenant.
func (h *Handler) sharesAWorkspace(ctx context.Context, actor, target string) (bool, error) {
	var shared bool
	err := h.repo.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM workspace_members a
			  JOIN workspace_members b ON b.workspace_id = a.workspace_id
			 WHERE a.user_id = $1 AND b.user_id = $2
		)`, actor, target).Scan(&shared)
	return shared, err
}

// ---------------------------------------------------------------------------
// Backlinks
// ---------------------------------------------------------------------------

// Backlink is one document that points at something.
type Backlink struct {
	FileID    string    `json:"file_id"`
	Name      string    `json:"name"`
	FileType  string    `json:"file_type"`
	FolderID  *string   `json:"folder_id"`
	BlockID   string    `json:"block_id"`
	UpdatedAt time.Time `json:"updated_at"`
}

// BacklinksTo lists the documents that reference an object AND that the caller
// may read.
//
// The visibility filter is EXISTS over acl_key with one KeysFor per request,
// never a per-row Can and never a JOIN. A join against acl_key multiplies a row
// by its key count, and this keyset cursor is taken from the last row of a
// page, so duplicated rows make a page hold fewer distinct files than it claims
// and the cursor then skips the difference.
func (r *Repository) BacklinksTo(ctx context.Context, subject authz.SubjectRef, workspaceID,
	refType, refID string, cur httputil.Cursor, limit int) ([]Backlink, error) {

	keys, err := r.authz.KeysFor(ctx, subject, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("resolve access keys: %w", err)
	}
	if len(keys) == 0 {
		return []Backlink{}, nil
	}

	// The keyset anchor. Zero cursor means "first page", and the predicate is
	// disabled with a boolean rather than by building a second query string —
	// two spellings of the same filter is how a page boundary silently starts
	// dropping rows.
	hasCur := !cur.IsZero()
	cursorAt, cursorID := cur.CreatedAt, cur.ID
	if !hasCur {
		cursorID = "00000000-0000-0000-0000-000000000000"
	}

	rows, err := r.pool.Query(ctx, `
		SELECT f.id::text, f.name, f.file_type, f.folder_id::text,
		       MIN(x.block_id) AS block_id, f.updated_at
		  FROM file_projection_refs x
		  JOIN files f ON f.id = x.file_id
		 WHERE x.ref_type = $1
		   AND x.ref_id = $2
		   AND f.workspace_id = $3
		   AND f.trashed_at IS NULL
		   AND (NOT $4::bool OR (f.updated_at, f.id) < ($5::timestamptz, $6::uuid))
		   AND EXISTS (SELECT 1 FROM acl_key k
		                WHERE k.object_type = 'file' AND k.object_id = f.id
		                  AND k.key = ANY($7))
		 GROUP BY f.id, f.name, f.file_type, f.folder_id, f.updated_at
		 ORDER BY f.updated_at DESC, f.id DESC
		 LIMIT $8`,
		refType, refID, workspaceID, hasCur, cursorAt, cursorID, keys, limit)
	if err != nil {
		return nil, fmt.Errorf("query backlinks: %w", err)
	}
	defer rows.Close()

	out := make([]Backlink, 0, limit)
	for rows.Next() {
		var b Backlink
		if err := rows.Scan(&b.FileID, &b.Name, &b.FileType, &b.FolderID, &b.BlockID, &b.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan backlink: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListBacklinks is GET /api/v1/drive/refs/{ref_type}/{ref_id}/files — "which
// documents point at this thing".
func (h *Handler) ListBacklinks(w http.ResponseWriter, r *http.Request) {
	refType, err := httputil.RequirePathParam(r, "ref_type")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	refID, err := httputil.RequirePathParam(r, "ref_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	if !refTypeShape.MatchString(refType) {
		httputil.JSONError(w, http.StatusBadRequest, "INVALID_REF_TYPE",
			"ref_type must match "+refTypeShape.String())
		return
	}

	ctx := r.Context()
	actor := authctx.UserID(ctx)

	// You must be able to read the TARGET before you learn what points at it.
	// Without this, backlinks are an oracle: ask for a file id you cannot read
	// and the answer tells you which documents mention it, and by whom.
	if resolver, known := refResolvers[refType]; known {
		switch {
		case resolver.sharesWorkspace:
			shared, err := h.sharesAWorkspace(ctx, actor, refID)
			if err != nil {
				httputil.HandleError(w, httputil.NewInternal(err))
				return
			}
			if !shared {
				httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "object not found")
				return
			}
		default:
			if _, ok := h.authorize(w, r, resolver.object(refID), authz.CapRead); !ok {
				return
			}
		}
	} else {
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "object not found")
		return
	}

	workspaceID, err := h.workspaceOfRef(ctx, refType, refID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "object not found")
			return
		}
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	page, err := httputil.ParsePagination(r)
	if err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "INVALID_CURSOR", err.Error())
		return
	}

	links, err := h.repo.BacklinksTo(ctx, authz.UserSubject(actor), workspaceID, refType, refID,
		page.Cursor, page.Limit)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	writeBacklinkPage(w, links, page.Limit)
}

// writeBacklinkPage renders one keyset page. Shared by both backlink routes so
// the two cannot disagree about when there is a next page.
func writeBacklinkPage(w http.ResponseWriter, links []Backlink, limit int) {
	var next string
	hasMore := limit > 0 && len(links) == limit
	if hasMore {
		last := links[len(links)-1]
		next = httputil.EncodeCursor(last.UpdatedAt, last.FileID)
	}
	httputil.JSONList(w, http.StatusOK, links, next, hasMore)
}

// workspaceOfRef scopes the backlink query to one tenant. It is asked of the
// TARGET rather than taken from the request, so a caller cannot ask which
// documents in workspace B mention an object in workspace A.
func (h *Handler) workspaceOfRef(ctx context.Context, refType, refID string) (string, error) {
	var query string
	switch refType {
	case authz.TypeFile:
		query = `SELECT workspace_id::text FROM files WHERE id = $1`
	case authz.TypeFolder:
		query = `SELECT workspace_id::text FROM drive_folders WHERE id = $1`
	case authz.TypeIssue:
		query = `SELECT workspace_id::text FROM issues WHERE id = $1`
	default:
		return "", pgx.ErrNoRows
	}
	var ws string
	err := h.repo.pool.QueryRow(ctx, query, refID).Scan(&ws)
	return ws, err
}

// ListFileBacklinks is GET /api/v1/drive/files/{file_id}/backlinks, the same
// question asked the other way round: "what points at this file".
func (h *Handler) ListFileBacklinks(w http.ResponseWriter, r *http.Request) {
	fileID, err := httputil.RequirePathParam(r, "file_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	if _, ok := h.authorize(w, r, authz.FileObject(fileID), authz.CapRead); !ok {
		return
	}

	ctx := r.Context()
	file, err := h.repo.File(ctx, fileID)
	if err != nil {
		fail(w, err)
		return
	}

	page, err := httputil.ParsePagination(r)
	if err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "INVALID_CURSOR", err.Error())
		return
	}

	links, err := h.repo.BacklinksTo(ctx, authz.UserSubject(authctx.UserID(ctx)),
		file.WorkspaceID, authz.TypeFile, fileID, page.Cursor, page.Limit)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	writeBacklinkPage(w, links, page.Limit)
}

// ---------------------------------------------------------------------------
// Comment anchors
// ---------------------------------------------------------------------------

// AnchorInput is the request body for attaching a range to a comment.
type AnchorInput struct {
	// Yjs encoded relative positions, base64 over the wire. OPAQUE: the server
	// never decodes one, for the same reason it never merges an update.
	AnchorStart []byte `json:"anchor_start"`
	AnchorEnd   []byte `json:"anchor_end"`
	// Quote is the text at creation time. Denormalized on purpose — it is the
	// only thing left when the anchored range is deleted, and an orphaned
	// comment must stay readable or resolving a discussion destroys its
	// subject.
	Quote   string `json:"quote"`
	BlockID string `json:"block_id"`
}

// PutAnchor is PUT /api/v1/drive/files/{file_id}/comments/{comment_id}/anchor.
//
// TWO REQUESTS, not one. The comment itself is created by internal/comment,
// unchanged, and this attaches a position to it. Adding an `anchor` field to
// comment.Create is precisely the change ruling 7 forbids: a new commentable
// type is a new acl_object type, not a change to the comment package. A failed
// second request degrades to an unanchored document-level comment, which is a
// correct and visible state rather than a lost one.
func (h *Handler) PutAnchor(w http.ResponseWriter, r *http.Request) {
	fileID, err := httputil.RequirePathParam(r, "file_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	commentID, err := httputil.RequirePathParam(r, "comment_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}

	// CapComment, the rung the ladder carries for exactly this: somebody may
	// discuss a document they may not change.
	if _, ok := h.authorize(w, r, authz.FileObject(fileID), authz.CapComment); !ok {
		return
	}

	var in AnchorInput
	if err := httputil.DecodeJSONLimit(r, &in, 1<<16); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}
	switch {
	case len(in.AnchorStart) == 0 || len(in.AnchorStart) > 4096,
		len(in.AnchorEnd) == 0 || len(in.AnchorEnd) > 4096:
		httputil.JSONError(w, http.StatusBadRequest, "INVALID_ANCHOR",
			"anchor_start and anchor_end must each be 1..4096 bytes")
		return
	case len(in.Quote) > maxProjectionQuoteLen:
		httputil.JSONError(w, http.StatusBadRequest, "INVALID_ANCHOR",
			fmt.Sprintf("quote is over %d bytes", maxProjectionQuoteLen))
		return
	case len(in.BlockID) > maxBlockIDLen:
		httputil.JSONError(w, http.StatusBadRequest, "INVALID_ANCHOR",
			fmt.Sprintf("block_id is over %d bytes", maxBlockIDLen))
		return
	}

	// The comment must belong to THIS file. Without this check the capability
	// above is on the wrong object: a caller who may comment on document A
	// could anchor a comment that lives on document B, and the anchor would
	// then be served to B's readers.
	if err := h.repo.PutCommentAnchor(r.Context(), fileID, commentID, in); err != nil {
		if errors.Is(err, ErrNotFound) {
			httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND",
				"no such comment on this file")
			return
		}
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PutCommentAnchor stores an anchor, verifying the comment targets this file.
func (r *Repository) PutCommentAnchor(ctx context.Context, fileID, commentID string, in AnchorInput) error {
	tag, err := r.pool.Exec(ctx, `
		INSERT INTO comment_anchors (comment_id, anchor_start, anchor_end, quote, block_id)
		SELECT c.id, $3, $4, $5, $6
		  FROM comments c
		 WHERE c.id = $1
		   AND c.object_type = 'file'
		   AND c.object_id = $2
		   AND c.deleted_at IS NULL
		ON CONFLICT (comment_id) DO UPDATE
		   SET anchor_start = EXCLUDED.anchor_start,
		       anchor_end   = EXCLUDED.anchor_end,
		       quote        = EXCLUDED.quote,
		       block_id     = EXCLUDED.block_id`,
		commentID, fileID, in.AnchorStart, in.AnchorEnd, in.Quote, in.BlockID)
	if err != nil {
		return fmt.Errorf("store comment anchor: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// The SELECT matched nothing: the comment does not exist, is deleted,
		// or targets a different object. All three are "not found" to the
		// caller — telling them which would confirm a comment id exists.
		return ErrNotFound
	}
	return nil
}

// Anchor is one stored range, returned alongside a document's comments.
type Anchor struct {
	CommentID   string `json:"comment_id"`
	AnchorStart []byte `json:"anchor_start"`
	AnchorEnd   []byte `json:"anchor_end"`
	Quote       string `json:"quote"`
	BlockID     string `json:"block_id"`
}

// ListAnchors is GET /api/v1/drive/files/{file_id}/anchors.
//
// A separate request from the comments themselves, so internal/comment stays
// unaware that ranges exist. The client joins them by comment id; an anchor
// with no comment is impossible (FK), and a comment with no anchor renders at
// document level, which is exactly what an orphaned or unanchored one should
// do.
func (h *Handler) ListAnchors(w http.ResponseWriter, r *http.Request) {
	fileID, err := httputil.RequirePathParam(r, "file_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}
	if _, ok := h.authorize(w, r, authz.FileObject(fileID), authz.CapRead); !ok {
		return
	}

	anchors, err := h.repo.AnchorsForFile(r.Context(), fileID)
	if err != nil {
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}
	httputil.JSON(w, http.StatusOK, anchors)
}

// AnchorsForFile reads every anchor whose comment targets this file.
func (r *Repository) AnchorsForFile(ctx context.Context, fileID string) ([]Anchor, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT a.comment_id::text, a.anchor_start, a.anchor_end, a.quote, a.block_id
		  FROM comment_anchors a
		  JOIN comments c ON c.id = a.comment_id
		 WHERE c.object_type = 'file'
		   AND c.object_id = $1
		   AND c.deleted_at IS NULL
		 ORDER BY a.created_at`, fileID)
	if err != nil {
		return nil, fmt.Errorf("query anchors: %w", err)
	}
	defer rows.Close()

	out := make([]Anchor, 0, 32)
	for rows.Next() {
		var a Anchor
		if err := rows.Scan(&a.CommentID, &a.AnchorStart, &a.AnchorEnd, &a.Quote, &a.BlockID); err != nil {
			return nil, fmt.Errorf("scan anchor: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
