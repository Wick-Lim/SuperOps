package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// s3Backend serves both BackendMinIO and BackendS3. They are the same protocol;
// what differs is the endpoint, whether the bucket is addressed by path or by
// hostname, and whether this process is allowed to create it.
type s3Backend struct {
	client *minio.Client
	bucket string
	name   string
	logger *slog.Logger
}

// Open constructs a backend and proves it works.
//
// "Proves it works" is the part that is new. The old constructor called
// BucketExists, which passes for a bucket the credentials can see and cannot
// write — the read-only-policy misconfiguration that fails on the first upload,
// hours later, as a 500 with no operator-facing explanation. Open now round
// trips a marker object, so the deployment either starts working or does not
// start.
func Open(ctx context.Context, cfg Config, logger *slog.Logger) (Backend, error) {
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}

	opts := &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       cfg.UseSSL,
		Region:       cfg.Region,
		BucketLookup: minio.BucketLookupDNS,
	}
	if deref(cfg.PathStyle) {
		opts.BucketLookup = minio.BucketLookupPath
	}

	client, err := minio.New(cfg.Endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("storage: create %s client: %w", cfg.Backend, err)
	}

	b := &s3Backend{client: client, bucket: cfg.Bucket, name: cfg.Backend, logger: logger}

	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("storage: reach bucket %q on %s: %w", cfg.Bucket, cfg.Endpoint, err)
	}
	if !exists {
		if !deref(cfg.CreateBucket) {
			return nil, fmt.Errorf("storage: bucket %q does not exist on %s and "+
				"STORAGE_CREATE_BUCKET is off; create it out of band (bucket creation is an "+
				"account-level action with billing and policy consequences)", cfg.Bucket, cfg.Endpoint)
		}
		if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{Region: cfg.Region}); err != nil {
			return nil, fmt.Errorf("storage: create bucket %q: %w", cfg.Bucket, err)
		}
		logger.Info("created storage bucket", "backend", cfg.Backend, "bucket", cfg.Bucket)
	}

	if err := b.Validate(ctx); err != nil {
		return nil, err
	}

	logger.Info("storage ready",
		"backend", cfg.Backend, "endpoint", cfg.Endpoint, "bucket", cfg.Bucket,
		"path_style", deref(cfg.PathStyle))
	return b, nil
}

// validationKey is where the boot check writes. Under a reserved prefix so it
// can never collide with a real object key, which always begins with a
// workspace uuid.
const validationKey = ".superops/storage-check"

// Validate round trips a marker object: put, get, delete.
//
// All three, because they fail independently. A bucket policy that allows
// s3:PutObject and not s3:GetObject passes a write-only check and then serves
// nobody's downloads; one that allows both and not s3:DeleteObject passes and
// then makes the object GC a no-op that logs success forever.
//
// It is called at boot and again by POST /admin/storage/test, so the operator
// verifying their settings runs exactly the check that gates startup.
func (b *s3Backend) Validate(ctx context.Context) error {
	body := []byte("superops storage check")

	if err := b.Put(ctx, validationKey, bytes.NewReader(body), int64(len(body)), "text/plain"); err != nil {
		return fmt.Errorf("storage: bucket %q is not writable: %w", b.bucket, err)
	}
	r, _, err := b.Get(ctx, validationKey)
	if err != nil {
		return fmt.Errorf("storage: bucket %q is writable but not readable: %w", b.bucket, err)
	}
	got, readErr := io.ReadAll(r)
	_ = r.Close()
	if readErr != nil {
		return fmt.Errorf("storage: read back from bucket %q: %w", b.bucket, readErr)
	}
	if !bytes.Equal(got, body) {
		return fmt.Errorf("storage: bucket %q returned %d bytes for a %d byte object; "+
			"something between here and the bucket is rewriting objects",
			b.bucket, len(got), len(body))
	}
	if err := b.Delete(ctx, validationKey); err != nil {
		return fmt.Errorf("storage: bucket %q does not allow deletes, so the object "+
			"garbage collector would silently leak every object it tried to remove: %w", b.bucket, err)
	}
	return nil
}

func (b *s3Backend) Name() string { return b.name }

func (b *s3Backend) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if _, err := b.client.PutObject(ctx, b.bucket, key, r, size, minio.PutObjectOptions{
		ContentType: contentType,
	}); err != nil {
		return fmt.Errorf("put object: %w", err)
	}
	return nil
}

func (b *s3Backend) Get(ctx context.Context, key string) (io.ReadCloser, string, error) {
	obj, err := b.client.GetObject(ctx, b.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, "", wrapNotFound("get object", err)
	}
	// GetObject is lazy: it does not talk to the server until the first read or
	// Stat, so a missing key surfaces here rather than above.
	stat, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		return nil, "", wrapNotFound("stat object", err)
	}
	return obj, stat.ContentType, nil
}

func (b *s3Backend) Head(ctx context.Context, key string) (ObjectInfo, error) {
	stat, err := b.client.StatObject(ctx, b.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return ObjectInfo{}, wrapNotFound("head object", err)
	}
	return ObjectInfo{
		Key:          stat.Key,
		Size:         stat.Size,
		ContentType:  stat.ContentType,
		ETag:         stat.ETag,
		LastModified: stat.LastModified,
	}, nil
}

// Delete is idempotent: S3 and MinIO both answer 204 for a key that is not
// there, and file.Collect depends on that.
func (b *s3Backend) Delete(ctx context.Context, key string) error {
	if err := b.client.RemoveObject(ctx, b.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("remove object: %w", err)
	}
	return nil
}

// List walks the bucket so the collector can find objects whose row is gone.
// limit bounds one sweep; pass a prefix to shard, because there is no
// continuation token in this signature and an unprefixed listing returns the
// same first N keys forever.
func (b *s3Backend) List(ctx context.Context, prefix string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 1000
	}
	// ListObjects leaks a goroutine unless its context is cancelled or the
	// channel is drained, and this function breaks out early by design.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	keys := make([]string, 0, limit)
	for obj := range b.client.ListObjects(ctx, b.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("list objects: %w", obj.Err)
		}
		// The boot check's marker is not a product object and must never be
		// offered to the collector as an unreferenced one.
		if obj.Key == validationKey {
			continue
		}
		keys = append(keys, obj.Key)
		if len(keys) >= limit {
			break
		}
	}
	return keys, nil
}

func (b *s3Backend) PresignGet(ctx context.Context, key string, ttl time.Duration, opt PresignOptions) (string, error) {
	if opt.ContentType == "" || opt.Disposition == "" {
		// Refused rather than defaulted. A presigned URL with no response
		// overrides serves the object with its stored type, inline, on the
		// bucket's origin — which is the download hardening the proxy path
		// exists to provide, silently removed. Defaulting would make the
		// mistake invisible; erroring makes it a compile-time-shaped bug found
		// on the first call.
		return "", fmt.Errorf("presign get %q: PresignOptions.ContentType and .Disposition are required; "+
			"a presigned URL cannot carry a CSP header, so the response overrides are the only thing "+
			"stopping the browser from rendering the object on the bucket's origin", key)
	}
	params := url.Values{}
	params.Set("response-content-type", opt.ContentType)
	params.Set("response-content-disposition", opt.Disposition)

	u, err := b.client.PresignedGetObject(ctx, b.bucket, key, ttl, params)
	if err != nil {
		return "", fmt.Errorf("presign get %q: %w", key, err)
	}
	return u.String(), nil
}

func (b *s3Backend) PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error) {
	u, err := b.client.PresignedPutObject(ctx, b.bucket, key, ttl)
	if err != nil {
		return "", fmt.Errorf("presign put %q: %w", key, err)
	}
	return u.String(), nil
}

// wrapNotFound turns the backend's own 404 into ErrNotFound so a handler can
// answer 404 instead of 500. Everything else passes through unchanged: a
// permissions failure that read as "not found" would hide a misconfiguration
// behind a plausible answer.
func wrapNotFound(op string, err error) error {
	var resp minio.ErrorResponse
	if errors.As(err, &resp) && (resp.StatusCode == 404 ||
		resp.Code == "NoSuchKey" || resp.Code == "NoSuchBucket") {
		return fmt.Errorf("%s: %w", op, ErrNotFound)
	}
	return fmt.Errorf("%s: %w", op, err)
}
