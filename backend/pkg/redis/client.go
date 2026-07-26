package redis

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
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

	// A CONFIGURED PASSWORD AGAINST A SERVER THAT WANTS NONE IS NOT AN ERROR,
	// AND IT IS NOT NOTHING EITHER.
	//
	// go-redis tolerates it: the server answers AUTH with "called without any
	// password configured", the driver treats that as a no-op, and Ping
	// succeeds. So a deployment that believes it is authenticating connects
	// completely unauthenticated and nothing says so — and the connection is
	// then only as private as the network, which is exactly the assumption an
	// operator setting REDIS_PASSWORD is trying not to make.
	//
	// Warned rather than refused: the cache still works, and turning a
	// misconfiguration into a boot failure would take an otherwise healthy
	// deployment down. One extra round trip at startup buys the operator a line
	// they can act on.
	if cfg.Password != "" {
		if err := client.Do(ctx, "AUTH", cfg.Password).Err(); err != nil &&
			strings.Contains(err.Error(), "without any password configured") {
			logger.Warn("REDIS_PASSWORD is set but this Redis requires no authentication; "+
				"the connection is unauthenticated and is only as private as the network",
				"addr", cfg.Addr)
		}
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
