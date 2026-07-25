package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The conformance suite: everything the product assumes about a Backend,
// asserted against a real one.
//
// docs/plans/02-drive.md §11 asks for this to be table-driven over every
// configured backend. MinIO is the one CI runs; S3 is skipped unless
// credentialed, because a suite that silently tests nothing is worse than one
// that says it did not run.
//
// Two of these are load-bearing rather than routine:
//
//   - DELETE OF A MISSING KEY MUST SUCCEED. file.Collect deletes an object and
//     then its row, and retries after a partial failure re-delete objects that
//     are already gone. A backend that errored there would wedge the collector
//     on its first retry and leak every object behind it forever.
//   - GET OF A MISSING KEY MUST BE ErrNotFound. A handler that cannot tell
//     "gone" from "broken" answers 500 to a user who deleted their own file.

func backendUnderTest(t *testing.T) Backend {
	t.Helper()
	ctx := context.Background()

	cfg := Config{
		Backend:   env("STORAGE_BACKEND", BackendMinIO),
		Endpoint:  env("MINIO_ENDPOINT", "127.0.0.1:19000"),
		AccessKey: env("MINIO_ACCESS_KEY", "minioadmin"),
		SecretKey: env("MINIO_SECRET_KEY", "minioadmin"),
		// A bucket of its own: the conformance suite deletes what it writes, and
		// pointing it at the deployment's bucket would make a bug here a data
		// loss incident there.
		Bucket: env("STORAGE_TEST_BUCKET", "superops-conformance-test"),
		UseSSL: envBool("MINIO_USE_SSL", false),
		Region: os.Getenv("STORAGE_REGION"),
	}

	b, err := Open(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		if requireInfra() {
			t.Fatalf("SUPEROPS_REQUIRE_INFRA=1 but object storage is unusable: %v", err)
		}
		t.Skipf("object storage unavailable, skipping conformance suite: %v", err)
	}
	return b
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func requireInfra() bool {
	b, err := strconv.ParseBool(os.Getenv("SUPEROPS_REQUIRE_INFRA"))
	return err == nil && b
}

// testKey is under the sweep-visible keyspace on purpose: these objects should
// look exactly like product objects to anything else that walks the bucket.
func testKey(name string) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d/2020/01/01/%s", os.Getpid()%1000000000000, name)
}

func TestBackendConformance(t *testing.T) {
	b := backendUnderTest(t)
	ctx := context.Background()
	body := []byte("the quick brown fox")

	t.Run("put then get returns the same bytes and type", func(t *testing.T) {
		key := testKey("roundtrip.txt")
		t.Cleanup(func() { _ = b.Delete(ctx, key) })

		if err := b.Put(ctx, key, bytes.NewReader(body), int64(len(body)), "text/plain"); err != nil {
			t.Fatalf("put: %v", err)
		}
		r, contentType, err := b.Get(ctx, key)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		got, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !bytes.Equal(got, body) {
			t.Errorf("get returned %q, want %q", got, body)
		}
		if contentType != "text/plain" {
			t.Errorf("content type = %q, want text/plain", contentType)
		}
	})

	t.Run("head reports the true size", func(t *testing.T) {
		key := testKey("head.txt")
		t.Cleanup(func() { _ = b.Delete(ctx, key) })

		if err := b.Put(ctx, key, bytes.NewReader(body), int64(len(body)), "text/plain"); err != nil {
			t.Fatalf("put: %v", err)
		}
		info, err := b.Head(ctx, key)
		if err != nil {
			t.Fatalf("head: %v", err)
		}
		if info.Size != int64(len(body)) {
			t.Errorf("head size = %d, want %d — this is the number the presigned-upload "+
				"completion path trusts INSTEAD of the client's claim", info.Size, len(body))
		}
	})

	t.Run("delete of a missing key succeeds", func(t *testing.T) {
		// The property file.Collect depends on. See the package note above.
		if err := b.Delete(ctx, testKey("never-existed.bin")); err != nil {
			t.Fatalf("delete of a missing key = %v, want nil; the object collector deletes "+
				"an object and then its row, so every retry after a partial failure "+
				"re-deletes objects that are already gone — an error here wedges it forever", err)
		}
	})

	t.Run("delete is idempotent", func(t *testing.T) {
		key := testKey("twice.bin")
		if err := b.Put(ctx, key, bytes.NewReader(body), int64(len(body)), "application/octet-stream"); err != nil {
			t.Fatalf("put: %v", err)
		}
		if err := b.Delete(ctx, key); err != nil {
			t.Fatalf("first delete: %v", err)
		}
		if err := b.Delete(ctx, key); err != nil {
			t.Fatalf("second delete: %v", err)
		}
	})

	t.Run("get of a missing key is ErrNotFound", func(t *testing.T) {
		_, _, err := b.Get(ctx, testKey("absent.bin"))
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("get of a missing key = %v, want ErrNotFound; a handler that cannot "+
				"tell 'gone' from 'broken' answers 500 to a user who deleted their own file", err)
		}
	})

	t.Run("head of a missing key is ErrNotFound", func(t *testing.T) {
		_, err := b.Head(ctx, testKey("absent.bin"))
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("head of a missing key = %v, want ErrNotFound", err)
		}
	})

	t.Run("list finds what was put and respects the limit", func(t *testing.T) {
		keys := []string{testKey("l1.bin"), testKey("l2.bin"), testKey("l3.bin")}
		for _, k := range keys {
			if err := b.Put(ctx, k, bytes.NewReader(body), int64(len(body)), "application/octet-stream"); err != nil {
				t.Fatalf("put %s: %v", k, err)
			}
			t.Cleanup(func() { _ = b.Delete(ctx, k) })
		}
		prefix := keys[0][:strings.Index(keys[0], "/")+1]

		got, err := b.List(ctx, prefix, 100)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		found := 0
		for _, k := range got {
			for _, want := range keys {
				if k == want {
					found++
				}
			}
		}
		if found != len(keys) {
			t.Errorf("list found %d of %d keys under %q", found, len(keys), prefix)
		}

		// The limit is what bounds one sweep. A backend that ignored it would
		// hand the collector the whole bucket in memory.
		bounded, err := b.List(ctx, prefix, 2)
		if err != nil {
			t.Fatalf("bounded list: %v", err)
		}
		if len(bounded) > 2 {
			t.Errorf("list with limit 2 returned %d keys", len(bounded))
		}
	})

	t.Run("list hides the validation marker", func(t *testing.T) {
		// Validate leaves nothing behind, but a crash between its put and its
		// delete would. The marker has no files row, so the collector would
		// treat it as unreferenced and delete it on every sweep forever.
		if err := b.Validate(ctx); err != nil {
			t.Fatalf("validate: %v", err)
		}
		keys, err := b.List(ctx, "", 1000)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, k := range keys {
			if k == validationKey {
				t.Errorf("List returned %q; the collector would sweep it every run", k)
			}
		}
	})

	t.Run("presigned get serves the object as an attachment", func(t *testing.T) {
		key := testKey("presigned.bin")
		t.Cleanup(func() { _ = b.Delete(ctx, key) })

		// Deliberately a type a browser WOULD render, to prove the override is
		// what reaches the client rather than the stored type.
		if err := b.Put(ctx, key, bytes.NewReader(body), int64(len(body)), "text/html"); err != nil {
			t.Fatalf("put: %v", err)
		}

		url, err := b.PresignGet(ctx, key, time.Minute, PresignOptions{
			ContentType: "application/octet-stream",
			Disposition: `attachment; filename="presigned.bin"`,
		})
		if err != nil {
			t.Fatalf("presign: %v", err)
		}

		resp, err := http.Get(url) //nolint:gosec // the URL is one this test just minted
		if err != nil {
			t.Fatalf("fetch presigned url: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("presigned GET = %d, want 200", resp.StatusCode)
		}
		got, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read presigned body: %v", err)
		}
		if !bytes.Equal(got, body) {
			t.Errorf("presigned GET returned %q, want %q", got, body)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
			t.Errorf("presigned Content-Type = %q, want application/octet-stream — the stored "+
				"text/html reached the browser, and a presigned URL has no CSP to stop it "+
				"rendering on the bucket's origin", ct)
		}
		if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
			t.Errorf("presigned Content-Disposition = %q, want attachment", cd)
		}
	})

	t.Run("presign of a key that does not exist still mints a url", func(t *testing.T) {
		// Presigning is a signature over a request, not a lookup. A backend that
		// round-tripped to check would make every download two calls.
		url, err := b.PresignGet(ctx, testKey("absent.bin"), time.Minute, PresignOptions{
			ContentType: "application/octet-stream",
			Disposition: `attachment; filename="absent.bin"`,
		})
		if err != nil {
			t.Fatalf("presign of a missing key: %v", err)
		}
		if url == "" {
			t.Error("presign returned an empty url with no error")
		}
	})
}

// Validate is what gates boot and what POST /admin/storage/test runs. It has to
// be safe to run repeatedly against a live bucket.
func TestValidateIsRepeatableAndLeavesNothingBehind(t *testing.T) {
	b := backendUnderTest(t)
	ctx := context.Background()

	for i := range 3 {
		if err := b.Validate(ctx); err != nil {
			t.Fatalf("validate run %d: %v", i+1, err)
		}
	}
	if _, err := b.Head(ctx, validationKey); !errors.Is(err, ErrNotFound) {
		t.Errorf("the validation marker survived: head = %v, want ErrNotFound", err)
	}
}
