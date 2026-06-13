package ratelimit

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

type Config struct {
	RequestsPerMinute int
	Window            time.Duration
	// TrustProxy controls whether X-Forwarded-For is honored. Enable only when
	// running behind a trusted reverse proxy (nginx/ingress); when directly
	// exposed it must be false, otherwise clients can spoof their IP to evade
	// per-IP limits.
	TrustProxy bool
}

func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i >= 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func enforce(w http.ResponseWriter, r *http.Request, rdb *redis.Client, key string, cfg Config, next http.Handler) {
	allowed, err := checkRate(r.Context(), rdb, key, cfg.RequestsPerMinute, cfg.Window)
	if err != nil {
		// If Redis is unavailable, fail open so the platform stays usable.
		next.ServeHTTP(w, r)
		return
	}
	if !allowed {
		w.Header().Set("Retry-After", "60")
		httputil.JSONError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests")
		return
	}
	next.ServeHTTP(w, r)
}

// Middleware rate-limits by authenticated user (falling back to client IP).
func Middleware(rdb *redis.Client, cfg Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := authctx.UserID(r.Context())
			if key == "" {
				key = clientIP(r, cfg.TrustProxy)
			}
			enforce(w, r, rdb, fmt.Sprintf("ratelimit:%s", key), cfg, next)
		})
	}
}

// MiddlewareByIP rate-limits strictly by client IP — used to protect
// unauthenticated, brute-forceable endpoints (login, refresh, accept-invite).
func MiddlewareByIP(rdb *redis.Client, cfg Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			enforce(w, r, rdb, fmt.Sprintf("ratelimit:ip:%s:%s", r.URL.Path, clientIP(r, cfg.TrustProxy)), cfg, next)
		})
	}
}

// APIMiddleware applies a general per-user/IP limit, but only to /api/v1 paths
// so health/readiness probes and other infra routes are never throttled.
func APIMiddleware(rdb *redis.Client, cfg Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, "/api/v1/") {
				next.ServeHTTP(w, r)
				return
			}
			key := authctx.UserID(r.Context())
			if key == "" {
				key = clientIP(r, cfg.TrustProxy)
			}
			enforce(w, r, rdb, fmt.Sprintf("ratelimit:api:%s", key), cfg, next)
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
