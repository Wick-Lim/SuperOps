package drive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

// The projection is how a document the server cannot read becomes searchable,
// renderable on a phone, previewable in a channel and exportable to markdown.
//
// The server cannot read it because the document is a CRDT: migration 015
// stores opaque updates and refuses a server-side merge, and a Go-side READER
// is the same class of work with the same failure mode — a reconstruction that
// disagreed with the client's would be a corruption bug debuggable from neither
// side. So the client that already has the document in memory renders it to
// text and posts that.
//
// Five rules make trusting a client safe, and each is enforced below rather
// than described:
//
//  1. MONOTONIC ON SEQ, IN ONE STATEMENT. Two clients projecting the same
//     document race harmlessly; the loser's write matches zero rows.
//  2. NEVER ABOVE THE LOG HEAD. A projection claiming a seq the log has not
//     reached is a bug or a client inventing content.
//  3. AUTHORIZED AT POST TIME, on write capability, over HTTP. The room's
//     membership check is cached in memory for the keystroke path; this one is
//     not, so a user revoked mid-session cannot land one last rewrite of the
//     searchable body.
//  4. BOUNDED. A projection is attacker-supplied by definition. Every limit is
//     a 400, never a truncation.
//  5. NEVER AUTHORITATIVE. Losing the whole table costs search and mobile
//     rendering until re-projection, and costs zero content.

// refTypeShape is internal/authz's object-type alphabet, NOT the registry's.
// The difference is the underscore: a ref_type reaches authz.ObjectRef, whose
// path is spliced into LIKE predicates, so authz forbids '_' and this must
// agree with it rather than with files.file_type's looser CHECK.
var refTypeShape = regexp.MustCompile(`^[a-z][a-z0-9]{0,31}$`)

// Projection bounds. Each is a refusal, not a clamp: silently storing half of
// what a client sent would make the index disagree with the document in a way
// nothing downstream could detect.
const (
	maxProjectionBodyBytes = 1 << 20 // 1 MiB ≈ 250 printed pages
	maxProjectionRefs      = 1000
	maxProjectionOutline   = 500
	maxBlockIDLen          = 64
	maxOutlineTextLen      = 512
	maxProjectionQuoteLen  = 2000

	// The request body carries the text, the outline and the refs, so it is
	// bounded above the text limit rather than at it.
	maxProjectionRequestBytes = 2 << 20
)

var (
	// ErrNotCollab is a projection posted to a file that has no collaborative
	// document. Not a 404: the file exists and the caller can read it, it just
	// has no log a projection could describe.
	ErrNotCollab = errors.New("drive: file has no collaborative document")
	// ErrSeqAheadOfLog is rule 2.
	ErrSeqAheadOfLog = errors.New("drive: projection seq is ahead of the log head")
	// ErrSchemaRegressed is a projection from a client older than the one that
	// last wrote. Its extractor silently drops nodes it does not know, so
	// accepting it would write a lossy body and a wrong ref set.
	ErrSchemaRegressed = errors.New("drive: projection schema version regressed")
)

// ProjectionRef is one typed object a document points at.
type ProjectionRef struct {
	RefType string `json:"ref_type"`
	RefID   string `json:"ref_id"`
	BlockID string `json:"block_id,omitempty"`
}

// OutlineEntry is one heading.
type OutlineEntry struct {
	BlockID string `json:"block_id"`
	Level   int    `json:"level"`
	Text    string `json:"text"`
}

// ProjectionInput is the request body.
type ProjectionInput struct {
	Seq           int64           `json:"seq"`
	SchemaVersion int             `json:"schema_version"`
	BodyText      string          `json:"body_text"`
	Outline       []OutlineEntry  `json:"outline"`
	Refs          []ProjectionRef `json:"refs"`
}

// ProjectionStatus is how far the projection has fallen behind the log.
//
// It rides on the file descriptor because that is where the client already
// looks. Not debug output: the client uses the gap to decide whether to
// re-project on open, and an operator uses it to see the pipeline failing
// before a user reports unsearchable documents.
type ProjectionStatus struct {
	HeadSeq       int64      `json:"head_seq"`
	ProjectionSeq int64      `json:"projection_seq"`
	SchemaVersion int        `json:"schema_version"`
	ProjectedAt   *time.Time `json:"projected_at"`
}

// validate is rule 4, stated once so the handler and any future caller cannot
// disagree about the limits.
func (in *ProjectionInput) validate() error {
	if in.Seq < 0 {
		return fmt.Errorf("seq must not be negative")
	}
	if in.SchemaVersion < 1 {
		return fmt.Errorf("schema_version must be at least 1")
	}
	if len(in.BodyText) > maxProjectionBodyBytes {
		return fmt.Errorf("body_text is %d bytes, over the %d-byte limit",
			len(in.BodyText), maxProjectionBodyBytes)
	}
	if len(in.Outline) > maxProjectionOutline {
		return fmt.Errorf("outline has %d entries, over the %d limit", len(in.Outline), maxProjectionOutline)
	}
	if len(in.Refs) > maxProjectionRefs {
		return fmt.Errorf("refs has %d entries, over the %d limit", len(in.Refs), maxProjectionRefs)
	}
	for i, e := range in.Outline {
		if len(e.BlockID) > maxBlockIDLen {
			return fmt.Errorf("outline[%d].block_id is over %d bytes", i, maxBlockIDLen)
		}
		if len(e.Text) > maxOutlineTextLen {
			return fmt.Errorf("outline[%d].text is over %d bytes", i, maxOutlineTextLen)
		}
		if e.Level < 1 || e.Level > 6 {
			return fmt.Errorf("outline[%d].level is %d, want 1..6", i, e.Level)
		}
	}
	for i, ref := range in.Refs {
		// The ref type shape is authz's, not files.file_type's: a ref_type is
		// handed to the resolver's object constructor, and authz forbids '_'
		// because an object path is spliced into LIKE predicates. Refusing it
		// here rather than at the database means the client is told which entry
		// is wrong.
		if !refTypeShape.MatchString(ref.RefType) {
			return fmt.Errorf("refs[%d].ref_type %q does not match %s", i, ref.RefType, refTypeShape)
		}
		if _, err := uuid.Parse(ref.RefID); err != nil {
			return fmt.Errorf("refs[%d].ref_id is not a uuid", i)
		}
		if len(ref.BlockID) > maxBlockIDLen {
			return fmt.Errorf("refs[%d].block_id is over %d bytes", i, maxBlockIDLen)
		}
	}
	return nil
}

// PutProjection stores one projection and rebuilds the document's refs.
//
// Returns applied=false when a newer projection is already stored — that is a
// stale projector losing the race in rule 1, which is normal and not an error.
// The genuinely invalid cases (no collab document, seq above the head, schema
// regression) come back as sentinels.
func (r *Repository) PutProjection(ctx context.Context, fileID string, in ProjectionInput) (ProjectionStatus, bool, error) {
	var (
		status  ProjectionStatus
		applied bool
	)
	sum := sha256.Sum256([]byte(in.BodyText))
	hash := hex.EncodeToString(sum[:])

	outline, err := json.Marshal(in.Outline)
	if err != nil {
		return status, false, fmt.Errorf("encode outline: %w", err)
	}
	if in.Outline == nil {
		outline = []byte("[]")
	}

	err = database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		// The log head and the workspace in one read. A file with no
		// collab_documents row has no log, so there is nothing a projection
		// could be a projection OF.
		var (
			headSeq     int64
			workspaceID string
		)
		err := tx.QueryRow(ctx, `
			SELECT d.head_seq, f.workspace_id::text
			  FROM collab_documents d
			  JOIN files f ON f.id = d.resource_id
			 WHERE d.resource_id = $1`, fileID).Scan(&headSeq, &workspaceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotCollab
		}
		if err != nil {
			return fmt.Errorf("read log head: %w", err)
		}
		status.HeadSeq = headSeq

		// Rule 2. Above the head is either a bug or a client inventing content;
		// there is no reading under which it is a stale race.
		if in.Seq > headSeq {
			return ErrSeqAheadOfLog
		}

		var storedSchema int
		err = tx.QueryRow(ctx,
			`SELECT schema_version FROM file_projections WHERE file_id = $1`, fileID).Scan(&storedSchema)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return fmt.Errorf("read stored schema version: %w", err)
		case in.SchemaVersion < storedSchema:
			return ErrSchemaRegressed
		}

		// Rule 1: ONE statement, conditional on the seq increasing. Broadcast
		// order is not commit order, so without the WHERE a slow projector
		// could overwrite a newer body with an older one.
		err = tx.QueryRow(ctx, `
			INSERT INTO file_projections
			       (file_id, workspace_id, projection_seq, schema_version,
			        body_text, body_hash, outline, projected_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
			ON CONFLICT (file_id) DO UPDATE
			   SET projection_seq = EXCLUDED.projection_seq,
			       schema_version = GREATEST(file_projections.schema_version, EXCLUDED.schema_version),
			       body_text      = EXCLUDED.body_text,
			       body_hash      = EXCLUDED.body_hash,
			       outline        = EXCLUDED.outline,
			       projected_at   = NOW()
			 WHERE file_projections.projection_seq < EXCLUDED.projection_seq
			RETURNING projection_seq, schema_version, projected_at`,
			fileID, workspaceID, in.Seq, in.SchemaVersion, in.BodyText, hash, outline,
		).Scan(&status.ProjectionSeq, &status.SchemaVersion, &status.ProjectedAt)

		if errors.Is(err, pgx.ErrNoRows) {
			// Lost the race. Report what IS stored so the caller can decide
			// whether it still needs to project.
			applied = false
			return tx.QueryRow(ctx, `
				SELECT projection_seq, schema_version, projected_at
				  FROM file_projections WHERE file_id = $1`, fileID,
			).Scan(&status.ProjectionSeq, &status.SchemaVersion, &status.ProjectedAt)
		}
		if err != nil {
			return fmt.Errorf("store projection: %w", err)
		}
		applied = true

		// Refs are rebuilt WHOLESALE, so they are exactly as fresh as the
		// projection and never more. Removing an embed removes the backlink,
		// which is the property a test asserts.
		if _, err := tx.Exec(ctx, `DELETE FROM file_projection_refs WHERE file_id = $1`, fileID); err != nil {
			return fmt.Errorf("clear refs: %w", err)
		}
		if len(in.Refs) > 0 {
			rows := make([][]any, 0, len(in.Refs))
			for _, ref := range in.Refs {
				rows = append(rows, []any{fileID, ref.RefType, ref.RefID, ref.BlockID})
			}
			if _, err := tx.CopyFrom(ctx,
				pgx.Identifier{"file_projection_refs"},
				[]string{"file_id", "ref_type", "ref_id", "block_id"},
				pgx.CopyFromRows(rows),
			); err != nil {
				return fmt.Errorf("write refs: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return status, false, err
	}
	return status, applied, nil
}

// ProjectionStatusFor reads the gap for the descriptor. Returns nil for a file
// with no collaborative document — a blob kind has no projection and reporting
// a zeroed one would read as "projected, and empty".
func (r *Repository) ProjectionStatusFor(ctx context.Context, fileID string) (*ProjectionStatus, error) {
	var st ProjectionStatus
	err := r.pool.QueryRow(ctx, `
		SELECT d.head_seq,
		       COALESCE(p.projection_seq, 0),
		       COALESCE(p.schema_version, 0),
		       p.projected_at
		  FROM collab_documents d
		  LEFT JOIN file_projections p ON p.file_id = d.resource_id
		 WHERE d.resource_id = $1`, fileID,
	).Scan(&st.HeadSeq, &st.ProjectionSeq, &st.SchemaVersion, &st.ProjectedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read projection status: %w", err)
	}
	return &st, nil
}

// BodyForFile is search.BodySource. The indexer reads the body from here rather
// than from the event, because a 1 MiB body on a JetStream message that fires
// on every rename and move is not a trade worth making.
//
// A file with no projection answers ("", nil): the object is indexed by title
// alone, which is the safe end. An error is an error — the indexer returns it
// and the event is redelivered.
func (r *Repository) BodyForFile(ctx context.Context, fileID string) (string, error) {
	var body string
	err := r.pool.QueryRow(ctx,
		`SELECT body_text FROM file_projections WHERE file_id = $1`, fileID).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read projected body for file %s: %w", fileID, err)
	}
	return body, nil
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// PutProjection is POST /api/v1/drive/files/{file_id}/projection.
func (h *Handler) PutProjection(w http.ResponseWriter, r *http.Request) {
	fileID, err := httputil.RequirePathParam(r, "file_id")
	if err != nil {
		httputil.HandleError(w, err)
		return
	}

	// Rule 3. CapWrite, over HTTP, every time — the room's in-memory membership
	// cache never reaches here, so a revoked editor cannot land one last
	// rewrite of the body everyone searches.
	if _, ok := h.authorize(w, r, authz.FileObject(fileID), authz.CapWrite); !ok {
		return
	}

	var in ProjectionInput
	if err := httputil.DecodeJSONLimit(r, &in, maxProjectionRequestBytes); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}
	if err := in.validate(); err != nil {
		httputil.JSONError(w, http.StatusBadRequest, "INVALID_PROJECTION", err.Error())
		return
	}

	status, applied, err := h.repo.PutProjection(r.Context(), fileID, in)
	switch {
	case errors.Is(err, ErrNotCollab):
		httputil.JSONError(w, http.StatusConflict, "NOT_COLLAB",
			"this file has no collaborative document, so there is no log a projection could describe")
		return
	case errors.Is(err, ErrSeqAheadOfLog):
		httputil.JSONError(w, http.StatusBadRequest, "SEQ_AHEAD_OF_LOG",
			"the projection claims a position the log has not reached")
		return
	case errors.Is(err, ErrSchemaRegressed):
		httputil.JSONError(w, http.StatusConflict, "SCHEMA_REGRESSED",
			"this document was last written by a newer client; reload before editing")
		return
	case err != nil:
		httputil.HandleError(w, httputil.NewInternal(err))
		return
	}

	// Re-index, best effort and post-commit, only when something changed. The
	// publish is deliberately not gated on `applied` alone: a losing projector
	// changed nothing, and re-publishing on every idle debounce is the storm
	// this avoids.
	if applied {
		file, err := h.repo.File(r.Context(), fileID)
		if err != nil {
			// Logged rather than swallowed: the projection is committed, and a
			// re-index that silently did not happen leaves the document stale
			// in search with no trace of why.
			h.logger().Error("projected file could not be re-indexed",
				"file_id", fileID, "error", err)
		}
		if err == nil {
			// Keyed on the projection seq, NOT on files.updated_at — this write
			// does not touch the files row, so every projection inside the
			// stream's two-minute duplicate window used to collapse onto the
			// first one and vanish.
			h.events.PublishFileProjection(r.Context(), file, status.ProjectionSeq)
		}
	}

	httputil.JSON(w, http.StatusOK, map[string]any{
		"applied": applied,
		"status":  status,
	})
}

// ChannelForFile resolves the channel a file hangs off, through the message it
// is attached to. Empty when it is not a chat attachment, which is every Drive
// file.
//
// It sits beside BodyForFile because they are the same kind of thing: a fact
// the index needs that belongs to the database rather than to an event. See
// search.ChannelSource for why the indexer reads it instead of trusting the
// publisher to have carried it.
func (r *Repository) ChannelForFile(ctx context.Context, fileID string) (string, error) {
	var channelID string
	err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(m.channel_id::text, '')
		  FROM files f
		  LEFT JOIN messages m ON m.id = f.message_id
		 WHERE f.id = $1`, fileID).Scan(&channelID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already purged. Not an error: the delete arm does not reach here, and
		// an index write for a row that is gone is one the next event corrects.
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve channel for file: %w", err)
	}
	return channelID, nil
}
