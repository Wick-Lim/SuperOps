//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Wick-Lim/SuperOps/backend/internal/search"
)

// THE PIPELINE, DRIVEN BY REAL EVENTS.
//
// Every other search test in this package hands the event to the indexer by
// hand (see indexDriveFile). That covers the indexer and covers nothing in
// front of it — which is why a defect in the PUBLISHER survived every one of
// them: the projection route's message id was (action, file, files.updated_at),
// and PutProjection does not touch the files row, so every projection landing
// inside the stream's two-minute duplicate window collapsed onto the first
// one's id and was dropped before any consumer existed to see it.
//
// A user typed for forty seconds and closed the tab. The database projection
// was complete and current; search returned the document by its first sentence
// and by nothing written afterwards. No repair path could notice, because the
// staleness lived between the database and the index — the one gap nothing
// looks at.
//
// So this test subscribes to the real stream, drives the real HTTP routes, and
// asserts on what actually arrives.

// fileEvents collects file events off the real JetStream stream for one
// workspace. It is an ephemeral consumer starting at "new", so it sees only
// what this test causes.
type fileEvents struct {
	msgs chan string // file id per event
	stop func()
}

func watchFileEvents(t *testing.T, workspaceID string) *fileEvents {
	t.Helper()
	if getHarness(t).app.NATS == nil {
		t.Fatal("the integration NATS connection is not wired")
	}
	js, err := jetstream.New(getHarness(t).app.NATS.Conn)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)

	cons, err := js.CreateOrUpdateConsumer(ctx, "SUPEROPS", jetstream.ConsumerConfig{
		FilterSubject: "superops." + workspaceID + ".file.*",
		DeliverPolicy: jetstream.DeliverNewPolicy,
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		cancel()
		t.Fatalf("create consumer: %v", err)
	}

	fe := &fileEvents{msgs: make(chan string, 64)}
	cc, err := cons.Consume(func(m jetstream.Msg) {
		_ = m.Ack()
		var env struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if json.Unmarshal(m.Data(), &env) == nil {
			select {
			case fe.msgs <- env.Data.ID:
			default:
			}
		}
	})
	if err != nil {
		cancel()
		t.Fatalf("consume: %v", err)
	}
	fe.stop = func() { cc.Stop(); cancel() }
	t.Cleanup(fe.stop)
	return fe
}

// countFor drains for d and returns how many events named fileID.
func (fe *fileEvents) countFor(fileID string, d time.Duration) int {
	n := 0
	deadline := time.After(d)
	for {
		select {
		case id := <-fe.msgs:
			if id == fileID {
				n++
			}
		case <-deadline:
			return n
		}
	}
}

// EVERY PROJECTION MUST REACH THE STREAM, not just the first one inside the
// duplicate window.
func TestASecondProjectionIsNotSwallowedByTheDuplicateWindow(t *testing.T) {
	h := getHarness(t)
	h.requireSearch(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)

	doc := h.newDocument(t, admin, ws, fmt.Sprintf("dupwindow-%d", time.Now().UnixNano()))
	if doc.CollabDocumentID == nil {
		t.Fatal("the document has no collaborative document")
	}
	watch := watchFileEvents(t, ws)

	h.appendUpdate(t, *doc.CollabDocumentID, me, []byte{1})
	if code, _ := h.project(t, admin, doc.ID, map[string]any{
		"seq": 1, "schema_version": 1, "body_text": "aardvark first save",
	}); code != http.StatusOK {
		t.Fatalf("first projection: %d", code)
	}
	if got := watch.countFor(doc.ID, 5*time.Second); got != 1 {
		t.Fatalf("the first projection produced %d events, want 1", got)
	}

	// Well inside the two-minute window, and with a higher seq so the write is
	// genuinely applied rather than discarded as stale.
	h.appendUpdate(t, *doc.CollabDocumentID, me, []byte{2})
	if code, _ := h.project(t, admin, doc.ID, map[string]any{
		"seq": 2, "schema_version": 1, "body_text": "basilisk second save",
	}); code != http.StatusOK {
		t.Fatalf("second projection: %d", code)
	}
	if got := watch.countFor(doc.ID, 5*time.Second); got != 1 {
		t.Fatalf("the second projection produced %d events, want 1 — it was swallowed by "+
			"the stream's duplicate window, so the document stays searchable only by "+
			"the text of its first save", got)
	}

	// A genuine RETRY of the same logical projection must still collapse; the
	// point of the id is to distinguish events, not to disable dedupe.
	if code, _ := h.project(t, admin, doc.ID, map[string]any{
		"seq": 2, "schema_version": 1, "body_text": "basilisk second save",
	}); code != http.StatusOK {
		t.Fatalf("replayed projection: %d", code)
	}
	if got := watch.countFor(doc.ID, 3*time.Second); got != 0 {
		t.Fatalf("a replayed projection produced %d events, want 0 — dedupe is gone", got)
	}
}

// AND THE BODY ACTUALLY REACHES THE INDEX, through the real indexer, so the
// second save is findable.
func TestTheSecondSaveIsSearchable(t *testing.T) {
	h := getHarness(t)
	h.requireSearch(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)

	token := fmt.Sprintf("basilisk%d", time.Now().UnixNano())
	doc := h.newDocument(t, admin, ws, fmt.Sprintf("secondsave-%d", time.Now().UnixNano()))
	if doc.CollabDocumentID == nil {
		t.Fatal("the document has no collaborative document")
	}
	watch := watchFileEvents(t, ws)

	h.appendUpdate(t, *doc.CollabDocumentID, me, []byte{1})
	h.project(t, admin, doc.ID, map[string]any{
		"seq": 1, "schema_version": 1, "body_text": "aardvark only",
	})
	watch.countFor(doc.ID, 5*time.Second)

	h.appendUpdate(t, *doc.CollabDocumentID, me, []byte{2})
	h.project(t, admin, doc.ID, map[string]any{
		"seq": 2, "schema_version": 1, "body_text": "aardvark and " + token,
	})
	if got := watch.countFor(doc.ID, 5*time.Second); got != 1 {
		t.Fatalf("the second projection never reached the stream (%d events)", got)
	}

	// Run the real indexer over the real event, then search over HTTP.
	h.indexDriveFile(t, ws, doc.ID)
	hits := h.searchHits(t, admin, ws, token)
	found := false
	for _, id := range hits {
		if id == doc.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the second save's text is not searchable: %q returned %v", token, hits)
	}
}

var _ = search.TypeFile
var _ = slog.Default
var _ = nats.Msg{}

// THE TRASH LIFECYCLE MUST REACH THE INDEX.
//
// Only the single-file trash route ever published. So:
//   - trashing a FOLDER removed it from the Drive listing and left every
//     document inside it fully searchable, title and body;
//   - RESTORING published nothing, so a restored document was usable and
//     permanently unsearchable;
//   - PURGING published nothing and destroyed the rows, which is worse than
//     the other two combined — cmd/reindex only upserts from live rows and has
//     no prune pass, so the orphan answers searches forever and NO operation
//     can remove it.
//
// The audit demonstrated all three by running them. This asserts on the events,
// because they are what the indexer acts on.
func TestTheTrashLifecycleReachesTheIndex(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	root := h.driveRoot(t, admin, ws)
	folder := h.createFolder(t, admin, ws, root.ID, uniqueSlug("trashlife"))

	// A document inside the folder, so the subtree path is what finds it.
	doc := h.newDocumentIn(t, admin, ws, folder.ID, fmt.Sprintf("inside-%d", time.Now().UnixNano()))
	watch := watchFileEvents(t, ws)

	// 1. Trashing the FOLDER must unindex what is inside it.
	h.req(t, http.StatusNoContent, http.MethodDelete,
		"/api/v1/drive/folders/"+folder.ID, admin, nil)
	if got := watch.countFor(doc.ID, 6*time.Second); got != 1 {
		t.Fatalf("trashing the folder produced %d events for the document inside it, want 1 — "+
			"the folder leaves the listing and its contents stay searchable", got)
	}

	// 2. Restoring must put it back.
	h.req(t, http.StatusOK, http.MethodPost,
		"/api/v1/drive/folder/"+folder.ID+"/restore", admin, nil)
	if got := watch.countFor(doc.ID, 6*time.Second); got != 1 {
		t.Fatalf("restoring produced %d events, want 1 — a restored document is usable "+
			"and permanently unsearchable", got)
	}

	// 3. Purging must unindex, and this is the only moment the id exists.
	h.req(t, http.StatusNoContent, http.MethodDelete,
		"/api/v1/drive/folders/"+folder.ID, admin, nil)
	watch.countFor(doc.ID, 6*time.Second)
	h.req(t, http.StatusOK, http.MethodDelete,
		"/api/v1/workspaces/"+ws+"/drive/trash", admin, nil)
	if got := watch.countFor(doc.ID, 8*time.Second); got < 1 {
		t.Fatalf("emptying the trash produced %d events, want at least 1 — the purged "+
			"document stays in the index forever with nothing able to remove it", got)
	}

	var alive int
	if err := h.app.DB.QueryRow(context.Background(),
		`SELECT count(*) FROM files WHERE id = $1`, doc.ID).Scan(&alive); err != nil {
		t.Fatal(err)
	}
	if alive != 0 {
		t.Fatalf("the purge left %d rows; this test is not exercising a purge", alive)
	}
}

// A CHAT ATTACHMENT IS SEARCHABLE, AND SEARCHABLE IN ITS CHANNEL.
//
// POST /api/v1/files/upload — the route the client actually calls for a chat
// attachment — published nothing at all. A file posted into a conversation was
// findable only after an operator ran cmd/reindex by hand, and a deleted one
// became an index orphan no operation could remove.
//
// `?channel=` was the same defect seen from the other side. channel_id reaches
// the index from files.message_id -> messages.channel_id, which only this path
// knows, so a live-indexed attachment carried an empty channel while a rebuilt
// one carried the real value. The filter matched nothing, ever.
func TestAChatAttachmentIsIndexedAndScopedToItsChannel(t *testing.T) {
	h := getHarness(t)
	h.requireSearch(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)

	watch := watchFileEvents(t, ws)
	name := fmt.Sprintf("ledger-%d.txt", time.Now().UnixNano())
	fileID := h.upload(t, admin, ws, name)
	if fileID == "" {
		t.Fatal("file storage is not wired; this test cannot verify anything")
	}

	// 1. Uploading publishes at all — it did not before.
	if got := watch.countFor(fileID, 6*time.Second); got != 1 {
		t.Fatalf("uploading a chat attachment produced %d events, want 1 — the file is "+
			"unsearchable until somebody runs cmd/reindex", got)
	}

	// 2. Posting it re-indexes, because attaching relocates the file: its
	//    readers become the channel's and it finally has a channel_id.
	channel := h.createTypedChannel(t, admin, ws, uniqueSlug("attach"), "public")
	h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/channels/"+channel+"/messages", admin,
		map[string]any{"content": "here it is", "file_ids": []string{fileID}})
	if got := watch.countFor(fileID, 6*time.Second); got != 1 {
		t.Fatalf("posting the attachment produced %d events, want 1 — the file stays "+
			"searchable by its uploader alone and never matches ?channel=", got)
	}

	// 3. And the index agrees, through the real indexer.
	h.indexDriveFile(t, ws, fileID)
	inChannel := h.searchHitsInChannel(t, admin, ws, name, channel)
	found := false
	for _, id := range inChannel {
		if id == fileID {
			found = true
		}
	}
	if !found {
		t.Fatalf("?channel= did not return the attachment posted into that channel: %v", inChannel)
	}
}

// searchHitsInChannel narrows a search to one channel, which is the filter that
// returned nothing for files.
func (h *harness) searchHitsInChannel(t *testing.T, token, workspaceID, query, channelID string) []string {
	t.Helper()
	r := h.req(t, http.StatusOK, "GET",
		"/api/v1/workspaces/"+workspaceID+"/search?q="+query+"&channel="+channelID, token, nil)
	var res struct {
		Hits []struct {
			ID string `json:"id"`
		} `json:"hits"`
	}
	decodeInto(t, r.Data, &res)
	out := make([]string, 0, len(res.Hits))
	for _, hit := range res.Hits {
		out = append(out, hit.ID)
	}
	return out
}
