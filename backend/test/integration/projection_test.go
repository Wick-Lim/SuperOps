//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wick-Lim/SuperOps/backend/internal/collab"
)

// The projection is the one place the server stores something it cannot verify:
// a client with write capability describes the contents of a document the
// server cannot read. Five rules make that safe, and each one below is a test
// rather than a comment.

type projectionStatus struct {
	HeadSeq       int64   `json:"head_seq"`
	ProjectionSeq int64   `json:"projection_seq"`
	SchemaVersion int     `json:"schema_version"`
	ProjectedAt   *string `json:"projected_at"`
}

type projectionResult struct {
	Applied bool             `json:"applied"`
	Status  projectionStatus `json:"status"`
}

// newDocument creates a document in the workspace's Drive root and returns its
// descriptor.
func (h *harness) newDocument(t *testing.T, token, workspaceID, name string) driveDescriptor {
	t.Helper()
	return h.newDocumentIn(t, token, workspaceID, h.driveRoot(t, token, workspaceID).ID, name)
}

// newDocumentIn is newDocument in a named folder, which is what any test about
// subtree behaviour needs.
func (h *harness) newDocumentIn(t *testing.T, token, workspaceID, folderID, name string) driveDescriptor {
	t.Helper()
	resp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/workspaces/"+workspaceID+"/drive/files", token,
		map[string]string{"folder_id": folderID, "name": name, "file_type": "document"})
	var d driveDescriptor
	decodeInto(t, resp.Data, &d)
	return d
}

// appendUpdate pushes one opaque CRDT update into a document's log, so head_seq
// advances. The bytes are never interpreted — by this test or by the server —
// which is the whole premise.
func (h *harness) appendUpdate(t *testing.T, documentID, actorID string, payload []byte) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var seq int64
	if err := h.app.DB.QueryRow(ctx, `
		WITH bumped AS (
			UPDATE collab_documents SET head_seq = head_seq + 1, updated_at = NOW()
			 WHERE id = $1 RETURNING id, head_seq
		)
		INSERT INTO collab_updates (document_id, seq, payload, actor_id)
		SELECT id, head_seq, $2, $3 FROM bumped
		RETURNING seq`, documentID, payload, actorID).Scan(&seq); err != nil {
		t.Fatalf("append update: %v", err)
	}
	return seq
}

func (h *harness) project(t *testing.T, token, fileID string, body map[string]any) (int, apiResp) {
	t.Helper()
	return h.do(t, http.MethodPost, "/api/v1/drive/files/"+fileID+"/projection", token, body)
}

// RULE 1: monotonic on seq. Two clients projecting the same document race
// harmlessly — the loser's write matches zero rows and the newer body survives.
//
// This is not a nicety. Broadcast order is not commit order: a projector that
// was slow to POST can arrive after one that saw more of the log, and without
// the conditional write it would overwrite a newer body with an older one and
// leave the index describing a document that no longer exists.
func TestProjectionIsMonotonicOnSeq(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)

	doc := h.newDocument(t, admin, ws, fmt.Sprintf("monotonic-%d", time.Now().UnixNano()))
	if doc.CollabDocumentID == nil {
		t.Fatal("the document has no collaborative document")
	}
	for i := 0; i < 5; i++ {
		h.appendUpdate(t, *doc.CollabDocumentID, me, []byte{byte(i)})
	}

	// The newer projection lands first.
	code, resp := h.project(t, admin, doc.ID, map[string]any{
		"seq": 5, "schema_version": 1, "body_text": "the newer body",
	})
	if code != http.StatusOK {
		t.Fatalf("project seq=5 = %d (%+v)", code, resp.Error)
	}
	var newer projectionResult
	decodeInto(t, resp.Data, &newer)
	if !newer.Applied {
		t.Fatal("the first projection was not applied")
	}

	// The straggler arrives with an older view of the same document.
	code, resp = h.project(t, admin, doc.ID, map[string]any{
		"seq": 2, "schema_version": 1, "body_text": "the stale body",
	})
	if code != http.StatusOK {
		t.Fatalf("a losing projector must be a 200, not an error: %d (%+v)", code, resp.Error)
	}
	var older projectionResult
	decodeInto(t, resp.Data, &older)
	if older.Applied {
		t.Fatal("the stale projection was applied; a slow projector can overwrite a newer body")
	}
	if older.Status.ProjectionSeq != 5 {
		t.Errorf("the loser is told projection_seq=%d, want 5 — it needs to know it lost",
			older.Status.ProjectionSeq)
	}

	// And the stored body is still the newer one.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var stored string
	if err := h.app.DB.QueryRow(ctx,
		`SELECT body_text FROM file_projections WHERE file_id = $1`, doc.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "the newer body" {
		t.Fatalf("stored body = %q, want the newer one", stored)
	}
}

// RULE 2: never above the log head. A projection claiming a position the log
// has not reached is a bug or a client inventing content, and there is no
// reading under which it is a stale race — so it is a 400, distinct from the
// zero-rows case above.
func TestProjectionAboveTheLogHeadIsRefused(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)

	doc := h.newDocument(t, admin, ws, fmt.Sprintf("ahead-%d", time.Now().UnixNano()))
	h.appendUpdate(t, *doc.CollabDocumentID, me, []byte{1})

	code, resp := h.project(t, admin, doc.ID, map[string]any{
		"seq": 9999, "schema_version": 1, "body_text": "content the log never carried",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("projection above head_seq = %d, want 400 (%+v)", code, resp.Error)
	}
}

// RULE 3: authorized on write capability at POST time, over HTTP.
//
// The room's membership check is cached in memory for the keystroke path, which
// is a deliberate trade for latency. This endpoint is not on that path and
// re-checks, so a user whose share was revoked mid-session cannot land one last
// rewrite of the body everyone searches.
func TestProjectionRequiresWriteAndReChecksAfterRevocation(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)

	doc := h.newDocument(t, admin, ws, fmt.Sprintf("authz-%d", time.Now().UnixNano()))
	me := h.whoami(t, admin)
	h.appendUpdate(t, *doc.CollabDocumentID, me, []byte{1})

	// A GUEST, not a member. workspaceCapability maps RoleMember to CapWrite, so
	// every member already holds write on Drive through the workspace grant and
	// a per-object share is a floor rather than a ceiling — a member shared at
	// "read" is still a writer, and a test built on one would pass without
	// testing anything.
	reader := h.newGuest(t, admin, ws, "projection-reader")

	// A reader cannot project. Not 404 — they can see the file — but 403.
	code, resp := h.project(t, reader.token, doc.ID, map[string]any{
		"seq": 1, "schema_version": 1, "body_text": "written by a reader",
	})
	if code != http.StatusForbidden {
		t.Fatalf("a read-capability caller projecting = %d, want 403 (%+v)", code, resp.Error)
	}

	// A `comment` caller cannot either. This is the capability docs is the
	// first consumer to distinguish from write, and getting it wrong here means
	// a commenter can rewrite the document's searchable text.
	h.req(t, http.StatusOK, http.MethodPut, "/api/v1/drive/file/"+doc.ID+"/shares", admin,
		map[string]string{"subject_id": reader.id, "capability": "comment"})
	code, resp = h.project(t, reader.token, doc.ID, map[string]any{
		"seq": 1, "schema_version": 1, "body_text": "written by a commenter",
	})
	if code != http.StatusForbidden {
		t.Fatalf("a comment-capability caller projecting = %d, want 403 (%+v)", code, resp.Error)
	}

	// Given write, they can.
	h.req(t, http.StatusOK, http.MethodPut, "/api/v1/drive/file/"+doc.ID+"/shares", admin,
		map[string]string{"subject_id": reader.id, "capability": "write"})
	code, resp = h.project(t, reader.token, doc.ID, map[string]any{
		"seq": 1, "schema_version": 1, "body_text": "written by an editor",
	})
	if code != http.StatusOK {
		t.Fatalf("a write-capability caller projecting = %d, want 200 (%+v)", code, resp.Error)
	}

	// Revoked, they cannot — WITHOUT waiting for any cache to expire, which is
	// the property that makes this endpoint different from the WS path.
	h.req(t, http.StatusNoContent, http.MethodDelete,
		"/api/v1/drive/file/"+doc.ID+"/shares/user/"+reader.id, admin, nil)
	code, resp = h.project(t, reader.token, doc.ID, map[string]any{
		"seq": 1, "schema_version": 1, "body_text": "written after revocation",
	})
	if code == http.StatusOK {
		t.Fatalf("a revoked editor projected successfully (%+v)", resp.Data)
	}
}

// RULE 4: bounded, and every bound is a refusal rather than a truncation.
//
// Storing half of what a client sent would make the index disagree with the
// document in a way nothing downstream could detect — the body would simply be
// missing its tail, and no error would ever be raised about it.
func TestProjectionBoundsAreRefusalsNotTruncations(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)

	doc := h.newDocument(t, admin, ws, fmt.Sprintf("bounds-%d", time.Now().UnixNano()))
	h.appendUpdate(t, *doc.CollabDocumentID, me, []byte{1})

	refs := make([]map[string]any, 0, 1001)
	for i := 0; i < 1001; i++ {
		refs = append(refs, map[string]any{
			"ref_type": "file", "ref_id": "00000000-0000-0000-0000-000000000001",
			"block_id": fmt.Sprintf("b%d", i),
		})
	}
	outline := make([]map[string]any, 0, 501)
	for i := 0; i < 501; i++ {
		outline = append(outline, map[string]any{"block_id": "b", "level": 1, "text": "h"})
	}

	tests := []struct {
		name string
		body map[string]any
	}{
		{"body over 1 MiB", map[string]any{
			// REAL CONTENT, not `make([]byte, N)`. That filler is a megabyte of
			// NUL bytes, which httputil.DecodeJSON now strips — so the body
			// arrived empty and was accepted, and the case stopped testing the
			// size bound at all. A printable byte is what a caller would send.
			"seq": 1, "schema_version": 1, "body_text": strings.Repeat("x", (1<<20)+1),
		}},
		{"over 1000 refs", map[string]any{
			"seq": 1, "schema_version": 1, "body_text": "x", "refs": refs,
		}},
		{"over 500 outline entries", map[string]any{
			"seq": 1, "schema_version": 1, "body_text": "x", "outline": outline,
		}},
		{"a block id over 64 bytes", map[string]any{
			"seq": 1, "schema_version": 1, "body_text": "x",
			"refs": []map[string]any{{
				"ref_type": "file", "ref_id": "00000000-0000-0000-0000-000000000001",
				"block_id": strings.Repeat("b", 65), // printable, for the same reason
			}},
		}},
		{"a ref_type authz would refuse", map[string]any{
			// Underscored: legal for files.file_type, illegal for an authz
			// object type, because an object path is spliced into LIKE.
			"seq": 1, "schema_version": 1, "body_text": "x",
			"refs": []map[string]any{{
				"ref_type": "drive_file", "ref_id": "00000000-0000-0000-0000-000000000001",
			}},
		}},
		{"a ref_id that is not a uuid", map[string]any{
			"seq": 1, "schema_version": 1, "body_text": "x",
			"refs": []map[string]any{{"ref_type": "file", "ref_id": "../../etc/passwd"}},
		}},
		{"schema_version below 1", map[string]any{
			"seq": 1, "schema_version": 0, "body_text": "x",
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, resp := h.project(t, admin, doc.ID, tt.body)
			if code != http.StatusBadRequest && code != http.StatusRequestEntityTooLarge {
				t.Fatalf("= %d, want 400 (%+v)", code, resp.Error)
			}
		})
	}

	// Nothing was stored by any of them. A partial write here is the failure
	// this whole test exists to catch.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var n int
	if err := h.app.DB.QueryRow(ctx,
		`SELECT count(*) FROM file_projections WHERE file_id = $1`, doc.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("a refused projection still wrote a row")
	}
}

// A projection from a client OLDER than the one that last wrote is refused.
//
// An old extractor silently drops nodes its compiled schema does not know, so
// accepting it would write a lossy body and a wrong ref set into the search
// index — and the user would see a document that looks complete on screen and
// is half-indexed. 409, so the client knows to reload rather than to retry.
func TestProjectionFromAnOlderSchemaIsRefused(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)

	doc := h.newDocument(t, admin, ws, fmt.Sprintf("schema-%d", time.Now().UnixNano()))
	for i := 0; i < 3; i++ {
		h.appendUpdate(t, *doc.CollabDocumentID, me, []byte{byte(i)})
	}

	if code, resp := h.project(t, admin, doc.ID, map[string]any{
		"seq": 1, "schema_version": 4, "body_text": "written by a current client",
	}); code != http.StatusOK {
		t.Fatalf("project at schema 4 = %d (%+v)", code, resp.Error)
	}

	code, resp := h.project(t, admin, doc.ID, map[string]any{
		"seq": 3, "schema_version": 2, "body_text": "written by a stale tab",
	})
	if code != http.StatusConflict {
		t.Fatalf("a projection from an older schema = %d, want 409 (%+v)", code, resp.Error)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var stored string
	if err := h.app.DB.QueryRow(ctx,
		`SELECT body_text FROM file_projections WHERE file_id = $1`, doc.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "written by a current client" {
		t.Fatalf("the stale tab's body was stored: %q", stored)
	}
}

// A blob file has no log, so a projection posted to one is a 409 rather than a
// silent success that would put a caller-supplied body in the index under a
// file whose real bytes say something else.
func TestProjectionOnABlobFileIsRefused(t *testing.T) {
	h := getHarness(t)
	h.storage(t) // skips when no object storage is configured
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	root := h.driveRoot(t, admin, ws)

	code, resp := h.uploadToDrive(t, admin, ws, root.ID, "notes.txt", []byte("real bytes"))
	if code != http.StatusCreated {
		t.Fatalf("upload = %d (%+v)", code, resp.Error)
	}
	var file driveDescriptor
	decodeInto(t, resp.Data, &file)

	code, resp = h.project(t, admin, file.ID, map[string]any{
		"seq": 1, "schema_version": 1, "body_text": "whatever the caller says it contains",
	})
	if code != http.StatusConflict {
		t.Fatalf("projecting a blob file = %d, want 409 (%+v)", code, resp.Error)
	}
}

// The descriptor carries the gap, and a blob file's is null.
//
// The client compares head_seq to projection_seq to decide whether to
// re-project on open. That is the ONLY backstop for a document edited and
// closed before its debounce fired: the server cannot produce content, and a
// zeroed status on a blob would read as "projected, and empty".
func TestDescriptorCarriesTheProjectionGap(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)

	doc := h.newDocument(t, admin, ws, fmt.Sprintf("gap-%d", time.Now().UnixNano()))
	for i := 0; i < 4; i++ {
		h.appendUpdate(t, *doc.CollabDocumentID, me, []byte{byte(i)})
	}

	var opened struct {
		Projection *projectionStatus `json:"projection"`
	}
	decodeInto(t, h.req(t, http.StatusOK, http.MethodGet,
		"/api/v1/drive/files/"+doc.ID, admin, nil).Data, &opened)
	if opened.Projection == nil {
		t.Fatal("a collab document's descriptor carries no projection status; the client " +
			"cannot tell whether the body it is about to render is stale")
	}
	if opened.Projection.HeadSeq != 4 || opened.Projection.ProjectionSeq != 0 {
		t.Fatalf("gap = %d/%d, want head 4 and projection 0",
			opened.Projection.ProjectionSeq, opened.Projection.HeadSeq)
	}

	if code, resp := h.project(t, admin, doc.ID, map[string]any{
		"seq": 4, "schema_version": 1, "body_text": "caught up",
	}); code != http.StatusOK {
		t.Fatalf("project = %d (%+v)", code, resp.Error)
	}

	var reopened struct {
		Projection *projectionStatus `json:"projection"`
	}
	decodeInto(t, h.req(t, http.StatusOK, http.MethodGet,
		"/api/v1/drive/files/"+doc.ID, admin, nil).Data, &reopened)
	if reopened.Projection == nil || reopened.Projection.ProjectionSeq != 4 {
		t.Fatalf("after projecting, the gap did not close: %+v", reopened.Projection)
	}
	if reopened.Projection.ProjectedAt == nil {
		t.Error("projected_at is null after a successful projection")
	}
}

// The whole reason the projection exists: a document is findable by its TEXT,
// not only by its title. The body reaches the index from the database — the
// event carries no body, because a 1 MiB payload on every rename is not a trade
// worth making — so this proves the BodySource seam end to end.
func TestADocumentIsFindableByItsBody(t *testing.T) {
	h := getHarness(t)
	h.requireSearch(t)

	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)

	// The title deliberately shares no word with the body, so a hit can only
	// come from the projection.
	doc := h.newDocument(t, admin, ws, fmt.Sprintf("untitled-%d", time.Now().UnixNano()))
	h.appendUpdate(t, *doc.CollabDocumentID, me, []byte{1})

	phrase := fmt.Sprintf("perihelion%d", time.Now().UnixNano())
	if code, resp := h.project(t, admin, doc.ID, map[string]any{
		"seq": 1, "schema_version": 1,
		"body_text": "The meeting agreed on " + phrase + " as the launch window.",
	}); code != http.StatusOK {
		t.Fatalf("project = %d (%+v)", code, resp.Error)
	}

	h.indexDriveFile(t, ws, doc.ID)

	if !contains(h.searchHits(t, admin, ws, phrase), doc.ID) {
		t.Fatal("a document is not findable by a word that appears only in its body; " +
			"the projection never reached the index")
	}

	// And another tenant still finds nothing — the body is indexed under the
	// document's real access keys, not widened by having content.
	other := h.newTenant(t, "body-outsider")
	if contains(h.searchHits(t, other.token, other.workspaceID, phrase), doc.ID) {
		t.Fatal("another tenant found the document by its body text")
	}
}

// RULE 6: A DOCUMENT NOBODY OPENS IS STILL REPAIRABLE.
//
// The three healthy projection paths — the editor's debounce, its flush on
// unmount, and its catch-up on open — all need somebody to be in the document.
// A document edited and then closed by a browser that was killed runs none of
// them, and is then stale in search with nothing able to notice. The repair
// sweep is what notices.
//
// THE LEFT JOIN IS WHAT THIS TEST IS FOR. A document that has never been
// projected has no row in file_projections at all; an inner join would find
// every mildly-stale document and miss precisely the ones with no searchable
// text whatsoever.
func TestStaleProjectionSweepFindsTheNeverProjected(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	wsID := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	ctx := context.Background()

	// Never projected: 300 updates, no projection row.
	never := h.newDocument(t, admin, wsID, fmt.Sprintf("never-%d", time.Now().UnixNano()))
	if never.CollabDocumentID == nil {
		t.Fatal("the document has no collaborative document")
	}
	h.setHeadSeq(t, *never.CollabDocumentID, 300)

	// Projected, but far behind.
	behind := h.newDocument(t, admin, wsID, fmt.Sprintf("behind-%d", time.Now().UnixNano()))
	if behind.CollabDocumentID == nil {
		t.Fatal("the document has no collaborative document")
	}
	h.appendUpdate(t, *behind.CollabDocumentID, me, []byte{1})
	if code, _ := h.project(t, admin, behind.ID, map[string]any{
		"seq": 1, "schema_version": 1, "body_text": "hello",
	}); code != http.StatusOK {
		t.Fatalf("seed projection: %d", code)
	}
	h.setHeadSeq(t, *behind.CollabDocumentID, 500)

	// Caught up. Must NOT be swept.
	fresh := h.newDocument(t, admin, wsID, fmt.Sprintf("fresh-%d", time.Now().UnixNano()))
	if fresh.CollabDocumentID == nil {
		t.Fatal("the document has no collaborative document")
	}
	h.appendUpdate(t, *fresh.CollabDocumentID, me, []byte{1})
	if code, _ := h.project(t, admin, fresh.ID, map[string]any{
		"seq": 1, "schema_version": 1, "body_text": "current",
	}); code != http.StatusOK {
		t.Fatalf("seed fresh projection: %d", code)
	}
	h.setHeadSeq(t, *fresh.CollabDocumentID, 1)

	// Mid-edit: a big gap, but touched a moment ago. Must NOT be swept — the
	// debounce two seconds away was about to do this work anyway.
	live := h.newDocument(t, admin, wsID, fmt.Sprintf("live-%d", time.Now().UnixNano()))
	if live.CollabDocumentID == nil {
		t.Fatal("the document has no collaborative document")
	}
	if _, err := h.app.DB.Exec(ctx,
		`UPDATE collab_documents SET head_seq = 900, updated_at = NOW() WHERE id = $1`,
		*live.CollabDocumentID); err != nil {
		t.Fatalf("age live document: %v", err)
	}

	// A HUGE LIMIT, because the queue is shared. Other tests — including the
	// starvation test below — leave permanently-stale documents behind, and
	// this one is about which documents the query FINDS, not about where they
	// sort. Asking for 100 made it depend on how many zombies happened to be
	// ahead of it, which is how it came to fail on a fresh database and pass on
	// a dirty one.
	stale, err := collab.FindStaleProjections(ctx, h.app.DB, 200, 30*time.Minute, 100000)
	if err != nil {
		t.Fatalf("find stale projections: %v", err)
	}
	found := map[string]int64{}
	for _, s := range stale {
		found[s.DocumentID] = s.Gap()
	}

	if gap, ok := found[*never.CollabDocumentID]; !ok {
		t.Error("a document that was never projected was not swept — the join lost it")
	} else if gap != 300 {
		t.Errorf("never-projected gap = %d, want 300", gap)
	}
	if gap, ok := found[*behind.CollabDocumentID]; !ok {
		t.Error("a document 499 behind its log was not swept")
	} else if gap != 499 {
		t.Errorf("behind gap = %d, want 499", gap)
	}
	if _, ok := found[*fresh.CollabDocumentID]; ok {
		t.Error("a caught-up document was swept; the sweep would ask rooms for work already done")
	}
	if _, ok := found[*live.CollabDocumentID]; ok {
		t.Error("a document being edited right now was swept; the age filter did not hold")
	}
}

// setHeadSeq advances the log head and ages the row past the sweep's minimum,
// which is what a document edited and then abandoned looks like.
func (h *harness) setHeadSeq(t *testing.T, documentID string, head int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := h.app.DB.Exec(ctx,
		`UPDATE collab_documents
		    SET head_seq = $2, updated_at = NOW() - INTERVAL '2 hours'
		  WHERE id = $1`, documentID, head); err != nil {
		t.Fatalf("set head seq: %v", err)
	}
}

// AN UNANSWERABLE DOCUMENT MUST NOT STARVE THE QUEUE.
//
// The sweep ordered by updated_at and took the first N with no state at all, so
// the batch was a pure function of the table and the oldest entries never left
// it. Only a client holding the CRDT can produce a projection, so a document
// whose room is permanently empty can NEVER be repaired — and it sat at the
// front of that queue forever while everything behind it starved. At the job's
// batch of 100, a tenant with 100 abandoned documents stopped repairing
// anything else, and the WARN line kept reporting the same count every ten
// minutes as though the sweep were working.
//
// Found by an audit that wrote this scenario as a failing test against my code.
func TestTheRepairSweepDoesNotAskTheSameDocumentForever(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	ctx := context.Background()

	// Two stale documents. The zombie went stale first, so a queue ordered by
	// age alone would return it every single time.
	zombie := h.newDocument(t, admin, ws, fmt.Sprintf("zombie-%d", time.Now().UnixNano()))
	newer := h.newDocument(t, admin, ws, fmt.Sprintf("newer-%d", time.Now().UnixNano()))
	if zombie.CollabDocumentID == nil || newer.CollabDocumentID == nil {
		t.Fatal("a document has no collaborative document")
	}
	// Both far behind and far older than every other row in the table, so this
	// test does not depend on what previous tests left lying around.
	for id, age := range map[string]string{
		*zombie.CollabDocumentID: "20 years",
		*newer.CollabDocumentID:  "19 years",
	} {
		if _, err := h.app.DB.Exec(ctx, `
			UPDATE collab_documents
			   SET head_seq = 5000, updated_at = NOW() - $2::interval,
			       repair_requested_at = NULL
			 WHERE id = $1`, id, age); err != nil {
			t.Fatal(err)
		}
	}

	// EVERY OTHER STALE DOCUMENT IS PUT BEHIND THESE TWO, and put back
	// afterwards, because the queue is shared and this test is about ORDER.
	//
	// Leaving them ahead is what made this test fail on correct code: a run
	// interrupted midway, or another test's leftovers, sit at the front with a
	// NULL stamp and the sweep correctly asks for them instead. The first
	// version's comment claimed the 19/20-year ages made it independent of
	// residue — false, because repair_requested_at is the PRIMARY sort key.
	if _, err := h.app.DB.Exec(ctx, `
		UPDATE collab_documents SET repair_requested_at = NOW()
		 WHERE id <> ALL($1::uuid[])`,
		[]string{*zombie.CollabDocumentID, *newer.CollabDocumentID}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Put this test's own fixtures out of everybody else's way, so the next
		// run — and the sibling test above — sees the queue it expects.
		cctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = h.app.DB.Exec(cctx, `
			UPDATE collab_documents SET head_seq = 0, repair_requested_at = NOW()
			 WHERE id = ANY($1::uuid[])`,
			[]string{*zombie.CollabDocumentID, *newer.CollabDocumentID})
	})

	// A batch of one is the same mechanism as the job's hundred, in miniature.
	seen := map[string]int{}
	for sweep := 0; sweep < 2; sweep++ {
		got, err := collab.FindStaleProjections(ctx, h.app.DB, 200, 30*time.Minute, 1)
		if err != nil {
			t.Fatalf("sweep %d: %v", sweep, err)
		}
		if len(got) != 1 {
			t.Fatalf("sweep %d returned %d rows, want 1", sweep, len(got))
		}
		seen[got[0].DocumentID]++
	}

	if seen[*zombie.CollabDocumentID] != 1 || seen[*newer.CollabDocumentID] != 1 {
		t.Fatalf("two sweeps asked %v — an unanswerable document is holding the "+
			"front of the queue and everything behind it starves", seen)
	}
}
