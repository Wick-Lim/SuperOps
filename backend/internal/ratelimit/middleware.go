// Package ratelimit implements a Redis-backed sliding-window request limiter.
//
// Sliding window, not fixed window: the previous implementation ran
// INCR + EXPIRE on every request, so the TTL was pushed forward each time the
// counter was touched. A client that once crossed the limit and kept sending
// at least one request per window never let the key expire, and the counter
// only ever climbed — a permanent lockout that no amount of backing off could
// clear. A sorted set scored by arrival time has no such state to get stuck
// in: entries fall out of the window on their own.
package ratelimit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

type Config struct {
	RequestsPerMinute int
	Window            time.Duration

	// TrustProxy controls whether X-Forwarded-For is honored at all. It must be
	// false whenever the process is directly reachable, otherwise any client can
	// pick its own rate-limit bucket by sending the header.
	TrustProxy bool

	// TrustedProxyHops is the number of trusted reverse proxies in front of this
	// process. See clientIP: the caller's address is the Nth entry counting from
	// the RIGHT of X-Forwarded-For, because each proxy appends the peer it saw.
	// Zero with TrustProxy set is treated as one hop (the single-nginx case).
	TrustedProxyHops int
}

// Identifier extracts the rate-limit subject from a request — normally the
// authenticated user id — returning "" for an anonymous caller.
//
// It exists because the API limiter runs outside the mux while authentication
// runs inside it, so the limiter cannot read authctx: at that point in the
// chain the context is always empty and every user behind one NAT egress
// shared a single bucket. The composition root passes a closure that parses
// the bearer token without enforcing it.
type Identifier func(*http.Request) string

// clientIP resolves the address to bucket a request under.
//
// Trusted-hop model. Each reverse proxy *appends* the peer address it saw to
// X-Forwarded-For (this is what nginx's $proxy_add_x_forwarded_for does), so
// with N trusted proxies in front of us the real client is the Nth entry from
// the right. Everything to the left of that was supplied by the client and is
// pure attacker input — reading the leftmost entry, as this used to, let anyone
// defeat the login brute-force limit with a single header. Counting from the
// right is immune to however many entries the client prepends.
func clientIP(r *http.Request, trustProxy bool, hops int) string {
	remote := remoteIP(r)
	if !trustProxy {
		return remote
	}
	if hops <= 0 {
		hops = 1
	}

	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return remote
	}

	parts := strings.Split(xff, ",")
	idx := len(parts) - hops
	if idx < 0 || idx >= len(parts) {
		// Fewer entries than trusted hops: the chain is not what we were
		// configured for, so nothing in the header can be trusted.
		return remote
	}

	if ip := normalizeIP(parts[idx]); ip != "" {
		return ip
	}
	return remote
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// normalizeIP validates a single X-Forwarded-For element, tolerating the
// "ip:port" and "[v6]:port" forms some proxies emit. Returns "" if it is not
// an IP address.
func normalizeIP(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if ip := net.ParseIP(s); ip != nil {
		return ip.String()
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip.String()
		}
	}
	return ""
}

func enforce(w http.ResponseWriter, r *http.Request, rdb *redis.Client, key string, cfg Config, next http.Handler) {
	allowed, err := checkRate(r.Context(), rdb, key, cfg.RequestsPerMinute, cfg.window())
	if err != nil {
		// If Redis is unavailable, fail open so the platform stays usable.
		next.ServeHTTP(w, r)
		return
	}
	if !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(cfg.window().Seconds())))
		httputil.JSONError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests")
		return
	}
	next.ServeHTTP(w, r)
}

func (c Config) window() time.Duration {
	if c.Window > 0 {
		return c.Window
	}
	return time.Minute
}

// MiddlewareByIP rate-limits strictly by client IP — used to protect
// unauthenticated, brute-forceable endpoints (login, refresh, accept-invite).
//
// The bucket includes the request PATH, which is right for every route whose
// path is fixed and WRONG for one whose path contains the thing being guessed.
// See MiddlewareByIPBucket.
func MiddlewareByIP(rdb *redis.Client, cfg Config) func(http.Handler) http.Handler {
	return MiddlewareByIPBucket(rdb, cfg, "")
}

// MiddlewareByIPBucket rate-limits by client IP under a FIXED bucket name.
//
// It exists because MiddlewareByIP keys on r.URL.Path, and a route whose path
// carries a secret — POST /drive/links/{token}/resolve — would give every guess
// its own bucket. The endpoint would be advertised as rate limited, would appear
// rate limited in the config, and would not be: an attacker walking the token
// space would never hit a limit, because no two attempts share a key.
//
// An empty bucket falls back to the path, which is the correct default for every
// route whose path is fixed.
func MiddlewareByIPBucket(rdb *redis.Client, cfg Config, bucket string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scope := bucket
			if scope == "" {
				scope = r.URL.Path
			}
			ip := clientIP(r, cfg.TrustProxy, cfg.TrustedProxyHops)
			enforce(w, r, rdb, fmt.Sprintf("ratelimit:ip:%s:%s", scope, ip), cfg, next)
		})
	}
}

// APIMiddleware applies a general per-user limit, but only to /api/v1 paths so
// health/readiness probes and other infra routes are never throttled.
//
// identify may be nil, in which case every caller is bucketed by IP.
func APIMiddleware(rdb *redis.Client, cfg Config, identify Identifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, "/api/v1/") {
				next.ServeHTTP(w, r)
				return
			}
			enforce(w, r, rdb, subjectKey(r, cfg, identify), cfg, next)
		})
	}
}

// subjectKey namespaces user buckets separately from IP buckets so an
// attacker cannot collide with a victim's bucket by forging an identifier that
// looks like an address.
func subjectKey(r *http.Request, cfg Config, identify Identifier) string {
	if identify != nil {
		if userID := identify(r); userID != "" {
			return "ratelimit:api:user:" + userID
		}
	}
	return "ratelimit:api:ip:" + clientIP(r, cfg.TrustProxy, cfg.TrustedProxyHops)
}

// checkRate admits a request if fewer than limit requests were recorded for
// key in the trailing window.
func checkRate(ctx context.Context, rdb *redis.Client, key string, limit int, window time.Duration) (bool, error) {
	return checkRateAt(ctx, rdb, key, limit, window, time.Now())
}

func checkRateAt(ctx context.Context, rdb *redis.Client, key string, limit int, window time.Duration, now time.Time) (bool, error) {
	member := windowMember(now)

	pipe := rdb.Pipeline()
	pipe.ZRemRangeByScore(ctx, key, "0", windowCutoff(now, window))
	pipe.ZAdd(ctx, key, redis.Z{Score: float64(now.UnixNano()), Member: member})
	card := pipe.ZCard(ctx, key)
	// The TTL is a janitor for idle keys only; correctness comes from
	// ZREMRANGEBYSCORE, so re-arming it on every request is harmless here.
	pipe.Expire(ctx, key, window+time.Second)

	if _, err := pipe.Exec(ctx); err != nil {
		return false, err
	}

	if card.Val() <= int64(limit) {
		return true, nil
	}

	// Rejected requests must not extend the lockout, so withdraw the entry we
	// just added. Failing to remove it only costs this caller one slot in the
	// current window, so the error is not worth failing the request over.
	rdb.ZRem(ctx, key, member)
	return false, nil
}

// windowCutoff is the exclusive upper bound (as a ZREMRANGEBYSCORE score) for
// entries that have aged out of the window.
func windowCutoff(now time.Time, window time.Duration) string {
	return "(" + strconv.FormatInt(now.Add(-window).UnixNano(), 10)
}

var (
	// procID disambiguates members minted by different replicas within the same
	// nanosecond; seq does the same within one replica.
	procID = newProcID()
	seq    atomic.Uint64
)

func newProcID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}

// windowMember builds a set member that is unique across concurrent requests.
// Sorted-set members are deduplicated, so a plain timestamp would silently
// merge simultaneous requests and undercount the window.
func windowMember(now time.Time) string {
	return strconv.FormatInt(now.UnixNano(), 10) + ":" + procID + ":" + strconv.FormatUint(seq.Add(1), 36)
}
