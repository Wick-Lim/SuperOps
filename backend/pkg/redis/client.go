package redis

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// Defaults are deliberately tight: Redis sits on the hot path of every
// rate-limit and presence call, and go-redis' own defaults let a hung Redis
// hold request goroutines longer than the server write timeout allows.
const (
	DefaultDialTimeout  = 2 * time.Second
	DefaultReadTimeout  = 1 * time.Second
	DefaultWriteTimeout = 1 * time.Second
	DefaultPoolSize     = 20
	DefaultPoolTimeout  = 2 * time.Second
)

type Config struct {
	Addr     string
	Password string
	DB       int

	// Zero values fall back to the Default* constants above.
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	PoolSize     int
	PoolTimeout  time.Duration
}

func NewClient(ctx context.Context, cfg Config, logger *slog.Logger) (*redis.Client, error) {
	opts := &redis.Options{
		Addr:         cfg.Addr,
		Password:     cfg.Password,
		DB:           cfg.DB,
		DialTimeout:  orDuration(cfg.DialTimeout, DefaultDialTimeout),
		ReadTimeout:  orDuration(cfg.ReadTimeout, DefaultReadTimeout),
		WriteTimeout: orDuration(cfg.WriteTimeout, DefaultWriteTimeout),
		PoolSize:     orInt(cfg.PoolSize, DefaultPoolSize),
		PoolTimeout:  orDuration(cfg.PoolTimeout, DefaultPoolTimeout),
	}

	client := redis.NewClient(opts)

	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}

	logger.Info("connected to Redis",
		"addr", cfg.Addr,
		"pool_size", opts.PoolSize,
		"read_timeout", opts.ReadTimeout,
	)

	return client, nil
}

func orDuration(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

func orInt(v, fallback int) int {
	if v > 0 {
		return v
	}
	return fallback
}
