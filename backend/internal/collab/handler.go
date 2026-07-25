package collab

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/Wick-Lim/SuperOps/backend/internal/ws"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

// The HTTP surface is deliberately small. Editing happens on the WebSocket; the
// three things that cannot are here:
//
//   - opening a document (needs a request/response, happens once per session),
//   - loading state (a snapshot is megabytes and has no business in a frame),
//   - and the two bulk writes — an offline backlog and a compaction snapshot —
//     which are exactly the payloads the socket's size bound refuses.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("POST /api/v1/workspaces/{workspace_id}/collab/documents", authMw(http.HandlerFunc(h.Open)))
	mux.Handle("GET /api/v1/collab/documents/{document_id}", authMw(http.HandlerFunc(h.Get)))
	mux.Handle("GET /api/v1/collab/documents/{document_id}/state", authMw(http.HandlerFunc(h.State)))
	mux.Handle("POST /api/v1/collab/documents/{document_id}/updates", authMw(http.HandlerFunc(h.Append)))
	mux.Handle("POST /api/v1/collab/documents/{document_id}/snapshot", authMw(http.HandlerFunc(h.Snapshot)))
}

// base64BodyLimit converts a raw payload budget into a JSON body budget:
// encoding/json renders []byte as base64, which is 4/3 the size, plus the
// envelope.
func base64BodyLimit(raw int64) int64 { return raw*4/3 + 4096 }

// writeError maps the shared sentinels onto statuses. It is the HTTP twin of
// Client.sendRoomError, and it exists so the two transports cannot drift.
func writeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ws.ErrRoomNotFound):
		httputil.JSONError(w, http.StatusNotFound, "NOT_FOUND", "collaboration document not found")
	case errors.Is(err, ws.ErrRoomForbidden):
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "not allowed to open this collaboration document")
	case errors.Is(err, ws.ErrRoomReadOnly):
		httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "read-only access to this collaboration document")
	case errors.Is(err, ws.ErrRoomInvalidPayload):
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "invalid collaboration payload")
	case errors.Is(err, ws.ErrRoomPayloadTooLarge):
		httputil.JSONError(w, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "collaboration payload too large")
	case errors.Is(err, ErrStaleSnapshot):
		// Not a failure: another compaction won, and the caller should simply
		// drop the snapshot it built.
		httputil.JSONError(w, http.StatusConflict, "STALE_SNAPSHOT", "another snapshot has already compacted past this position")
	case errors.Is(err, ErrResourceConflict):
		httputil.JSONError(w, http.StatusConflict, "CONFLICT", "this object already has a collaboration document in another workspace")
	default:
		httputil.HandleError(w, httputil.NewInternal(err))
	}
}

// documentID reads and validates the path parameter. Without the UUID check the
// type error surfaces from Postgres as a 500.
func documentID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("document_id")
	if uuid.Validate(id) != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "document_id must be a UUID")
		return "", false
	}
	return id, true
}

func (h *Handler) Open(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := authctx.UserID(ctx)

	workspaceID := r.PathValue("workspace_id")
	if uuid.Validate(workspaceID) != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "workspace_id must be a UUID")
		return
	}

	var input struct {
		ResourceType string `json:"resource_type"`
		ResourceID   string `json:"resource_id"`
	}
	if err := httputil.DecodeJSON(r, &input); err != nil {
		httputil.HandleError(w, err)
		return
	}
	if !isResourceType(input.ResourceType) {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST",
			"resource_type must be lowercase alphanumeric, 1-32 characters")
		return
	}
	if uuid.Validate(input.ResourceID) != nil {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "resource_id must be a UUID")
		return
	}

	doc, err := h.svc.OpenDocument(ctx, workspaceID, input.ResourceType, input.ResourceID, userID)
	if err != nil {
		writeError(w, err)
		return
	}
	httputil.JSON(w, http.StatusOK, doc)
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := documentID(w, r)
	if !ok {
		return
	}

	doc, access, err := h.svc.Authorize(ctx, id, authctx.UserID(ctx), false)
	if err != nil {
		writeError(w, err)
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]interface{}{
		"document":  doc,
		"can_write": access.CanWrite,
	})
}

// State serves the load and reconnect path: everything the caller needs to
// catch up from `since`. A caller with no state passes since=0 and gets the
// snapshot plus its tail.
func (h *Handler) State(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := documentID(w, r)
	if !ok {
		return
	}

	var since int64
	if raw := r.URL.Query().Get("since"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "since must be a non-negative integer")
			return
		}
		since = parsed
	}

	doc, _, err := h.svc.Authorize(ctx, id, authctx.UserID(ctx), false)
	if err != nil {
		writeError(w, err)
		return
	}
	if since > doc.HeadSeq {
		// A watermark ahead of the log is a client holding state from a
		// document that no longer exists in this form. Silently returning
		// nothing would leave it permanently and invisibly out of date.
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "since is ahead of the document")
		return
	}

	state, err := h.svc.Load(ctx, id, since)
	if err != nil {
		writeError(w, err)
		return
	}
	httputil.JSON(w, http.StatusOK, state)
}

// Append is the bulk write path: an update too large for a frame, or a backlog
// a client accumulated while it was offline.
func (h *Handler) Append(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := authctx.UserID(ctx)
	id, ok := documentID(w, r)
	if !ok {
		return
	}

	var input struct {
		Update []byte `json:"update"`
	}
	if err := httputil.DecodeJSONLimit(r, &input, base64BodyLimit(MaxUpdateBytes)); err != nil {
		httputil.HandleError(w, err)
		return
	}

	if _, _, err := h.svc.Authorize(ctx, id, userID, true); err != nil {
		writeError(w, err)
		return
	}

	// connID is empty: this update did not come from a socket, so no connection
	// can recognise it as its own echo.
	seq, err := h.svc.Append(ctx, id, userID, "", input.Update)
	if err != nil {
		writeError(w, err)
		return
	}
	httputil.JSON(w, http.StatusCreated, map[string]interface{}{"seq": seq})
}

// Snapshot receives a client-produced compaction. The server cannot build one
// itself — it never interprets an update — so this endpoint is the entire
// compaction mechanism.
func (h *Handler) Snapshot(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := authctx.UserID(ctx)
	id, ok := documentID(w, r)
	if !ok {
		return
	}

	var input struct {
		ThroughSeq int64  `json:"through_seq"`
		Snapshot   []byte `json:"snapshot"`
	}
	if err := httputil.DecodeJSONLimit(r, &input, base64BodyLimit(MaxSnapshotBytes)); err != nil {
		httputil.HandleError(w, err)
		return
	}
	if input.ThroughSeq <= 0 {
		httputil.JSONError(w, http.StatusBadRequest, "BAD_REQUEST", "through_seq must be positive")
		return
	}

	if _, _, err := h.svc.Authorize(ctx, id, userID, true); err != nil {
		writeError(w, err)
		return
	}

	compacted, err := h.svc.SaveSnapshot(ctx, id, input.ThroughSeq, userID, input.Snapshot)
	if err != nil {
		writeError(w, err)
		return
	}
	httputil.JSON(w, http.StatusOK, map[string]interface{}{
		"through_seq":     input.ThroughSeq,
		"updates_removed": compacted,
	})
}

// isResourceType mirrors the CHECK constraint in migration 015. Validating here
// keeps a bad editor key a 400 instead of a constraint violation surfacing as a
// 500.
func isResourceType(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	if s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}
