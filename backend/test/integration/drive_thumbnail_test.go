//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/Wick-Lim/SuperOps/backend/internal/thumb"
)

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// runThumbnailer delivers one thumbnail request to the real consumer. The suite
// runs no worker, so the durable binding is proven by the worker booting and
// everything else is proven here.
func (h *harness) runThumbnailer(t *testing.T, workspaceID, fileID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var storageKey, contentType string
	if err := h.app.DB.QueryRow(ctx,
		`SELECT storage_key, content_type FROM files WHERE id = $1`, fileID).
		Scan(&storageKey, &contentType); err != nil {
		t.Fatalf("read file: %v", err)
	}

	data, err := json.Marshal(map[string]any{
		"type": "thumbnail.requested",
		"data": map[string]string{
			"file_id": fileID, "storage_key": storageKey, "content_type": contentType,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	consumer := thumb.NewConsumer(h.app.DB, h.storage(t), slog.Default())
	if err := consumer.Handle(ctx, &nats.Msg{
		Subject: "superops." + workspaceID + ".thumbnail.requested",
		Data:    data,
	}); err != nil {
		t.Fatalf("thumbnailer: %v", err)
	}
}

func TestDriveThumbnailIsGeneratedAndServedInline(t *testing.T) {
	h := getHarness(t)
	tenant := h.newTenant(t, "thumbs")
	root := h.driveRoot(t, tenant.token, tenant.workspaceID)

	code, resp := h.uploadToDrive(t, tenant.token, tenant.workspaceID, root.ID,
		"photo.png", testPNG(t, 1200, 600))
	if code != http.StatusCreated {
		t.Fatalf("upload = %d (%+v)", code, resp.Error)
	}
	var file driveDescriptor
	decodeInto(t, resp.Data, &file)

	// Before the thumbnailer runs there is no preview, and the descriptor says
	// so rather than offering a URL to nothing.
	if file.ThumbnailURL != nil {
		t.Error("a freshly uploaded file already claims a thumbnail")
	}

	h.runThumbnailer(t, tenant.workspaceID, file.ID)

	reopened := h.req(t, http.StatusOK, http.MethodGet, "/api/v1/drive/files/"+file.ID, tenant.token, nil)
	var withThumb driveDescriptor
	decodeInto(t, reopened.Data, &withThumb)
	if withThumb.ThumbnailURL == nil || *withThumb.ThumbnailURL == "" {
		t.Fatal("no thumbnail_url after the thumbnailer ran")
	}

	// THE ONE OBJECT DRIVE SERVES INLINE from a presigned URL, where no CSP
	// header can travel. Fetch it from the bucket and check what the BUCKET
	// says it is — the server's intent is not the thing that reaches a browser.
	object, err := httpClient.Get(*withThumb.ThumbnailURL) //nolint:gosec // a URL the server just minted
	if err != nil {
		t.Fatalf("fetch thumbnail: %v", err)
	}
	defer object.Body.Close()
	if object.StatusCode != http.StatusOK {
		t.Fatalf("thumbnail GET = %d", object.StatusCode)
	}
	if ct := object.Header.Get("Content-Type"); ct != thumb.MediaType {
		t.Errorf("the bucket served the thumbnail as %q, want %q — the served type is the "+
			"security property, and the tree claimed image/webp while x/image has no WebP encoder",
			ct, thumb.MediaType)
	}
	if cd := object.Header.Get("Content-Disposition"); cd != "" && cd != "inline" {
		t.Errorf("thumbnail disposition = %q, want inline", cd)
	}
}

// The thumbnailer is the FIRST writer of files.thumbnail_key, so this is also
// the first time internal/file's Orphan.Keys() thumbnail handling and
// StorageKeysPresent's thumbnail arm do any work. Both were written for this
// moment and neither had ever been exercised — a thumbnail that the sweep
// treated as unreferenced would be deleted on the next run, silently, and the
// listing would fill with broken images.
func TestThumbnailSurvivesTheObjectCollector(t *testing.T) {
	h := getHarness(t)
	tenant := h.newTenant(t, "thumbgc")
	root := h.driveRoot(t, tenant.token, tenant.workspaceID)

	code, resp := h.uploadToDrive(t, tenant.token, tenant.workspaceID, root.ID,
		"kept.png", testPNG(t, 300, 300))
	if code != http.StatusCreated {
		t.Fatalf("upload = %d (%+v)", code, resp.Error)
	}
	var file driveDescriptor
	decodeInto(t, resp.Data, &file)
	h.runThumbnailer(t, tenant.workspaceID, file.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var thumbKey string
	if err := h.app.DB.QueryRow(ctx,
		`SELECT COALESCE(thumbnail_key, '') FROM files WHERE id = $1`, file.ID).Scan(&thumbKey); err != nil {
		t.Fatal(err)
	}
	if thumbKey == "" {
		t.Fatal("no thumbnail_key was written")
	}

	// Ask the collector, through its real predicate, whether the thumbnail is
	// referenced. Sweeping every hex prefix is what the worker does over sixteen
	// runs; one call to the predicate is what decides the answer.
	present, err := h.fileRepo(t).StorageKeysPresent(ctx, []string{thumbKey})
	if err != nil {
		t.Fatal(err)
	}
	if !present[thumbKey] {
		t.Fatal("the object collector considers a live thumbnail unreferenced; the next " +
			"bucket sweep would delete it and every listing would show a broken image")
	}
}
