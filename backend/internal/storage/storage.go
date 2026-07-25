// Package storage is object storage as a deployment-dependent capability
// (docs/ROADMAP.md §3c, docs/plans/02-drive.md §6).
//
// It was internal/file/storage.go, a MinIO client with five methods, reachable
// only through the chat-attachment handler. Drive needs the same bytes from
// three more places and needs two things that file did not: a presigned URL, so
// that a 200MB download does not transit the Go process, and a boot-time proof
// that the configured bucket is actually writable.
//
// The three §3c rules, applied literally:
//
//  1. SAFE DEFAULT. `minio`, pointing at the compose service, exactly as before.
//     An operator who configures nothing gets what they had.
//  2. FAIL AT BOOT. Selecting a backend whose credentials are wrong, or whose
//     bucket cannot be written, is a startup error — not a 500 on somebody's
//     first upload three days later. Validate does a real round trip.
//  3. OPERATOR VERIFICATION. Validate is also reachable on demand through
//     POST /api/v1/admin/storage/test, which returns the transport's own error.
//
// NO NEW DEPENDENCY. minio-go is already vendored and already speaks S3, so
// `minio` and `s3` are one implementation with different endpoint, region and
// path-style defaults. GCS is reachable today through its S3 interoperability
// endpoint with HMAC keys; a NATIVE GCS backend needs cloud.google.com/go/storage
// and that is a deliberate later decision with a named cost, not an oversight.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// Backend is everything the product needs from object storage.
//
// It is small on purpose: every method here is one an S3-compatible service
// answers directly, so a second implementation is a configuration difference
// rather than a porting exercise.
type Backend interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	// Get returns the object's bytes and its stored content type. The caller
	// closes the reader.
	Get(ctx context.Context, key string) (io.ReadCloser, string, error)
	Head(ctx context.Context, key string) (ObjectInfo, error)
	// Delete removes one object and MUST be idempotent: deleting a key that is
	// not there is success, not an error. file.Collect depends on that — it
	// deletes an object and then its row, and a retry after a partial failure
	// re-deletes objects that are already gone.
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string, limit int) ([]string, error)

	// PresignGet returns a URL that serves the object directly, or "" when this
	// backend cannot presign — the caller then falls back to proxying rather
	// than failing. See PresignOptions for why the response overrides are not
	// optional.
	PresignGet(ctx context.Context, key string, ttl time.Duration, opt PresignOptions) (string, error)
	// PresignPut returns a URL the client may upload to directly, or "".
	PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error)

	// Validate proves the bucket is readable, writable AND deletable by round
	// tripping a marker object. It is part of the interface rather than an
	// implementation detail because ROADMAP §3c makes operator verification
	// part of the contract: a capability an operator cannot test is one they
	// discover is broken from a user's bug report.
	//
	// Open calls it, so a deployment either starts working or does not start;
	// POST /api/v1/admin/storage/test calls the same method, so a 200 there
	// means the process would boot.
	Validate(ctx context.Context) error

	// Name is the transport an operator sees in the admin UI and in an audit
	// entry: "minio", "s3".
	Name() string
}

// ObjectInfo is what Head answers. It is the trustworthy size — the one the
// server reads back after a direct-to-storage upload, rather than the one the
// client claimed.
type ObjectInfo struct {
	Key          string
	Size         int64
	ContentType  string
	ETag         string
	LastModified time.Time
}

// PresignOptions forces the response headers a presigned GET will carry.
//
// This exists because presigning is where the download hardening could quietly
// be lost. Every downloaded byte used to transit the Go process, and the proxy
// path is where the security headers live: X-Content-Type-Options: nosniff, a
// restrictive CSP, and the inline-type allowlist that keeps anything scriptable
// from rendering on our origin. A presigned URL cannot carry a CSP header.
//
// So the split is: ORIGINALS are presigned as attachments with an opaque
// content type, and are therefore never rendered by the browser, which leaves
// the CSP nothing to protect. INLINE rendering uses the thumbnail, which the
// SERVER generated and whose type comes from a closed set (see drive's
// thumbMediaTypes). Small inline originals keep the proxied path with its
// existing headers.
//
// Both fields are required rather than optional for that reason: a zero
// PresignOptions would be a presigned URL that renders in the browser on our
// bucket's origin, and the type that reaches for it is exactly the type nobody
// remembers to fill in.
type PresignOptions struct {
	// ContentType is sent as response-content-type. Use
	// "application/octet-stream" for anything the browser must not render.
	ContentType string
	// Disposition is sent as response-content-disposition, already formatted —
	// use httputil/file's builder so the filename is escaped once.
	Disposition string
}

// ErrNotFound is returned by Get and Head for a key that is not there. Delete
// does NOT return it; see Backend.
var ErrNotFound = errors.New("object not found")

// Backends is the closed set of names Open accepts.
//
// Closed on purpose. An unknown STORAGE_BACKEND must fail at boot with the list
// of what is valid, not fall through to a default — an operator who typed "S3 "
// with a trailing space should not silently get MinIO on localhost.
const (
	BackendMinIO = "minio"
	BackendS3    = "s3"
)

// Config is the operator's whole storage decision.
type Config struct {
	// Backend is one of the constants above. Empty means BackendMinIO, which is
	// what a deployment that configures nothing has always had.
	Backend string

	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	UseSSL    bool

	// Region is required by S3 and ignored by MinIO. AWS's SDKs default it
	// silently; this does not, because a bucket in the wrong region fails with
	// a redirect that reads as a credentials problem.
	Region string

	// PathStyle puts the bucket in the path rather than the hostname. MinIO
	// needs it, S3 accepts it, and a custom S3-compatible endpoint usually
	// needs it — so it defaults to true for minio and false for s3.
	PathStyle *bool

	// CreateBucket makes Open create the bucket when it does not exist. True is
	// right for MinIO in compose, where nothing else would; false is right for
	// S3, where bucket creation is an account-level action with billing and
	// policy consequences that a service account should not be taking on its
	// own.
	CreateBucket *bool
}

// Normalize fills the defaults that depend on the chosen backend, and reports
// what an operator has to fix before boot can continue.
//
// It is separate from Open so that a config error is distinguishable from an
// unreachable service: the first is a typo the operator fixes, the second is an
// outage they wait out, and collapsing them into one startup message costs an
// afternoon.
func (c *Config) Normalize() error {
	if c.Backend == "" {
		c.Backend = BackendMinIO
	}
	switch c.Backend {
	case BackendMinIO:
		if c.PathStyle == nil {
			c.PathStyle = ptr(true)
		}
		if c.CreateBucket == nil {
			c.CreateBucket = ptr(true)
		}
	case BackendS3:
		if c.PathStyle == nil {
			c.PathStyle = ptr(false)
		}
		if c.CreateBucket == nil {
			c.CreateBucket = ptr(false)
		}
		if c.Endpoint == "" {
			if c.Region == "" {
				return fmt.Errorf("storage: STORAGE_BACKEND=s3 needs STORAGE_REGION " +
					"(or an explicit STORAGE_ENDPOINT); a bucket reached in the wrong region " +
					"fails with a redirect that reads as a credentials error")
			}
			c.Endpoint = "s3." + c.Region + ".amazonaws.com"
		}
		if !c.UseSSL {
			// Not a style preference. S3 credentials are sent on every request
			// and this is the one backend where the endpoint is on the public
			// internet by default.
			return fmt.Errorf("storage: STORAGE_BACKEND=s3 requires STORAGE_USE_SSL=true")
		}
	default:
		return fmt.Errorf("storage: unknown STORAGE_BACKEND %q, want one of %q or %q",
			c.Backend, BackendMinIO, BackendS3)
	}

	if c.Endpoint == "" {
		return fmt.Errorf("storage: STORAGE_ENDPOINT is required for backend %q", c.Backend)
	}
	if c.Bucket == "" {
		return fmt.Errorf("storage: STORAGE_BUCKET is required")
	}
	if c.AccessKey == "" || c.SecretKey == "" {
		return fmt.Errorf("storage: STORAGE_ACCESS_KEY and STORAGE_SECRET_KEY are required for backend %q",
			c.Backend)
	}
	return nil
}

func ptr[T any](v T) *T { return &v }

func deref(p *bool) bool { return p != nil && *p }
