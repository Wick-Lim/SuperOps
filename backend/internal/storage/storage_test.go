package storage

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Normalize is where a deployment-dependent capability either fails at boot or
// starts working wrongly (ROADMAP §3c rule 2). Every case below is one an
// operator can actually produce from a .env file.
func TestNormalizeFailsAtBootRatherThanLater(t *testing.T) {
	base := func() Config {
		return Config{
			Endpoint:  "localhost:9000",
			AccessKey: "k",
			SecretKey: "s",
			Bucket:    "superops",
		}
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string // substring; empty = must succeed
		because string
	}{
		{
			name:    "the default is what a deployment already had",
			mutate:  func(*Config) {},
			because: "an operator who configures nothing must keep working",
		},
		{
			name:    "an unknown backend",
			mutate:  func(c *Config) { c.Backend = "s4" },
			wantErr: "unknown STORAGE_BACKEND",
			because: "falling through to a default would silently point a production deployment at localhost",
		},
		{
			name:    "a typo with whitespace is not a backend either",
			mutate:  func(c *Config) { c.Backend = "s3 " },
			wantErr: "unknown STORAGE_BACKEND",
			because: "the set is closed; trailing whitespace is a typo, not an alias",
		},
		{
			name:    "s3 with no region and no endpoint",
			mutate:  func(c *Config) { c.Backend = BackendS3; c.Endpoint = ""; c.UseSSL = true },
			wantErr: "STORAGE_REGION",
			because: "a bucket reached in the wrong region fails with a redirect that reads as a credentials error",
		},
		{
			name:    "s3 without TLS",
			mutate:  func(c *Config) { c.Backend = BackendS3; c.Region = "eu-west-1"; c.Endpoint = "" },
			wantErr: "STORAGE_USE_SSL",
			because: "credentials go on every request and this endpoint is on the public internet",
		},
		{
			name:    "no bucket",
			mutate:  func(c *Config) { c.Bucket = "" },
			wantErr: "STORAGE_BUCKET",
			because: "there is no sensible default for where a company's files live",
		},
		{
			name:    "no credentials",
			mutate:  func(c *Config) { c.SecretKey = "" },
			wantErr: "STORAGE_SECRET_KEY",
			because: "an anonymous client fails on the first write, hours later",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base()
			tt.mutate(&cfg)
			err := cfg.Normalize()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Normalize() = %v, want success — %s", err, tt.because)
			case tt.wantErr == "":
				return
			case err == nil:
				t.Fatalf("Normalize() succeeded, want an error naming %q — %s", tt.wantErr, tt.because)
			case !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("Normalize() = %v, want it to name %q so the operator knows what to fix", err, tt.wantErr)
			}
		})
	}
}

// The path-style and bucket-creation defaults differ per backend, and both
// three-valued settings have to survive an explicit false.
func TestNormalizeDefaultsAreBackendSpecific(t *testing.T) {
	minio := Config{Endpoint: "localhost:9000", AccessKey: "k", SecretKey: "s", Bucket: "b"}
	if err := minio.Normalize(); err != nil {
		t.Fatal(err)
	}
	if !deref(minio.PathStyle) {
		t.Error("minio defaulted to virtual-host addressing, which it does not serve")
	}
	if !deref(minio.CreateBucket) {
		t.Error("minio defaulted to not creating its bucket; nothing else in compose would")
	}

	s3 := Config{Backend: BackendS3, Region: "eu-west-1", UseSSL: true,
		AccessKey: "k", SecretKey: "s", Bucket: "b"}
	if err := s3.Normalize(); err != nil {
		t.Fatal(err)
	}
	if deref(s3.PathStyle) {
		t.Error("s3 defaulted to path style")
	}
	if deref(s3.CreateBucket) {
		t.Error("s3 defaulted to creating buckets; that is an account-level action with billing consequences")
	}
	if s3.Endpoint != "s3.eu-west-1.amazonaws.com" {
		t.Errorf("s3 endpoint = %q, want it derived from the region", s3.Endpoint)
	}

	// An operator who says "no" must not be overridden by the default.
	off := false
	explicit := Config{Endpoint: "localhost:9000", AccessKey: "k", SecretKey: "s", Bucket: "b",
		CreateBucket: &off}
	if err := explicit.Normalize(); err != nil {
		t.Fatal(err)
	}
	if deref(explicit.CreateBucket) {
		t.Error("STORAGE_CREATE_BUCKET=false was overwritten by the default; " +
			"unset and false have to stay distinguishable")
	}
}

// PresignGet without response overrides is the one way the download hardening
// could be lost silently, so it is refused rather than defaulted.
//
// Today every downloaded byte transits the Go process, where nosniff, the CSP
// and the inline-type allowlist live. A presigned URL carries none of those. If
// PresignOptions were optional, the first caller to omit it would serve
// arbitrary user-uploaded content inline on the bucket's origin — and it would
// work, which is why nobody would notice.
func TestPresignRefusesToDropTheDownloadHardening(t *testing.T) {
	b := &s3Backend{bucket: "b", name: "minio"}
	ctx := context.Background()

	for _, opt := range []PresignOptions{
		{},
		{ContentType: "application/octet-stream"},
		{Disposition: `attachment; filename="x"`},
	} {
		url, err := b.PresignGet(ctx, "some/key", time.Minute, opt)
		if err == nil {
			t.Fatalf("PresignGet(%+v) returned %q; a presigned URL cannot carry a CSP header, "+
				"so the response overrides are the only thing keeping user content from "+
				"rendering on the bucket's origin", opt, url)
		}
	}
}

// The boot check's marker must never reach the garbage collector as an
// unreferenced object: it has no files row by construction, so the collector
// would delete it every sweep and the next Validate would recreate it.
func TestValidationKeyIsReserved(t *testing.T) {
	if !strings.HasPrefix(validationKey, ".") {
		t.Errorf("validationKey %q does not start with '.', so it could collide with a real key",
			validationKey)
	}
	// Real keys begin with a workspace uuid, so a leading dot is unreachable
	// from the upload path — and the hex shards the sweep walks never match it.
	for _, shard := range []string{"0", "9", "a", "f"} {
		if strings.HasPrefix(validationKey, shard) {
			t.Errorf("validationKey %q falls under sweep shard %q", validationKey, shard)
		}
	}
}
