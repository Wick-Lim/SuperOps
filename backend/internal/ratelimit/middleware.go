package ratelimit

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

type Config struct {
	RequestsPerMinute int
	Window            time.Duration
}

func Middleware(rdb *redis.Client, cfg Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Identify client by user ID or IP
			key := authctx.UserID(r.Context())
			if key == "" {
				key = r.RemoteAddr
			}
			key = fmt.Sprintf("ratelimit:%s", key)

			ctx := r.Context()
			allowed, err := checkRate(ctx, rdb, key, cfg.RequestsPerMinute, cfg.Window)
			if err != nil {
				// If Redis is down, allow the request
				next.ServeHTTP(w, r)
				return
			}

			if !allowed {
				w.Header().Set("Retry-After", "60")
				httputil.JSONError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func checkRate(ctx context.Context, rdb *redis.Client, key string, limit int, window time.Duration) (bool, error) {
	pipe := rdb.Pipeline()

	incr := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, window)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return false, err
	}

	count := incr.Val()
	return count <= int64(limit), nil
}
