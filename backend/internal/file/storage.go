package file

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type StorageConfig struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	UseSSL    bool
}

type Storage struct {
	client *minio.Client
	bucket string
	logger *slog.Logger
}

func NewStorage(cfg StorageConfig, logger *slog.Logger) (*Storage, error) {
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("create minio client: %w", err)
	}

	// Ensure bucket exists
	ctx := context.Background()
	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("check bucket: %w", err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{}); err != nil {
			return nil, fmt.Errorf("create bucket: %w", err)
		}
		logger.Info("created MinIO bucket", "bucket", cfg.Bucket)
	}

	logger.Info("connected to MinIO", "endpoint", cfg.Endpoint, "bucket", cfg.Bucket)

	return &Storage{client: client, bucket: cfg.Bucket, logger: logger}, nil
}

func (s *Storage) Upload(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, reader, size, minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		return fmt.Errorf("upload object: %w", err)
	}
	return nil
}

func (s *Storage) Download(ctx context.Context, key string) (io.ReadCloser, string, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("get object: %w", err)
	}

	stat, err := obj.Stat()
	if err != nil {
		obj.Close()
		return nil, "", fmt.Errorf("stat object: %w", err)
	}

	return obj, stat.ContentType, nil
}

func (s *Storage) Delete(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("remove object: %w", err)
	}
	return nil
}

// ListKeys walks the bucket so the garbage collector can find objects whose
// files row no longer exists (workspace deletion cascades the rows away and
// leaves every object behind). limit bounds one sweep; pass a prefix to shard.
func (s *Storage) ListKeys(ctx context.Context, prefix string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 1000
	}
	keys := make([]string, 0, limit)
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("list objects: %w", obj.Err)
		}
		keys = append(keys, obj.Key)
		if len(keys) >= limit {
			break
		}
	}
	return keys, nil
}
