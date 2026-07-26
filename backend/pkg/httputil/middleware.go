package httputil

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Wick-Lim/SuperOps/backend/pkg/logger"
)

type requestIDKey struct{}

// RequestIDFromContext returns the correlation id minted by
// RequestIDMiddleware, or "" when the middleware did not run.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", requestID)

		ctx := r.Context()
		// Store the raw id as well as the enriched logger: LoggingMiddleware
		// reads the id directly, and handlers that want structured logging read
		// the logger. Previously only the logger was stored and nothing ever
		// read it, so request_id reached no log line at all.
		ctx = context.WithValue(ctx, requestIDKey{}, requestID)
		ctx = logger.WithContext(ctx, logger.FromContext(ctx).With("request_id", requestID))

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// LoggingMiddleware emits one access-log line per request.
//
// The log is written from a defer so a panicking handler still produces a line
// (RecoveryMiddleware turns it into a 500 further out, but the access log is
// what makes the request findable at all).
//
// request_id is read from the context when RequestIDMiddleware ran further
// out, and otherwise from the response header — which RequestIDMiddleware sets
// even when it runs further *in*. That makes the field present regardless of
// the order the two are composed in.
func LoggingMiddleware(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

			defer func() {
				requestID := RequestIDFromContext(r.Context())
				if requestID == "" {
					requestID = sw.Header().Get("X-Request-ID")
				}

				attrs := []any{
					"method", r.Method,
					"path", redactPath(r.URL.Path),
					"status", sw.status,
					"duration_ms", time.Since(start).Milliseconds(),
					"remote_addr", r.RemoteAddr,
					"request_id", requestID,
				}
				if sw.hijacked {
					// A hijacked connection's "duration" is the whole WebSocket
					// session, not request latency — flag it so it is not read
					// as one.
					attrs = append(attrs, "hijacked", true)
				}
				log.Info("request", attrs...)
			}()

			next.ServeHTTP(sw, r)
		})
	}
}

func RecoveryMiddleware(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if err := recover(); err != nil {
					log.Error("panic recovered",
						"error", err,
						"path", r.URL.Path,
						"request_id", RequestIDFromContext(r.Context()),
					)
					JSONError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status   int
	hijacked bool
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Hijack delegates to the underlying ResponseWriter so WebSocket upgrades work
// through this middleware (a wrapper that hides Hijacker breaks the upgrade).
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("underlying ResponseWriter does not support hijacking")
	}
	conn, rw, err := hj.Hijack()
	if err == nil {
		w.hijacked = true
		w.status = http.StatusSwitchingProtocols
	}
	return conn, rw, err
}

// Flush delegates to the underlying ResponseWriter when it supports flushing.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// secretPathPrefixes are routes whose PATH carries a credential.
//
// A share-link token is a bearer credential for content: whoever holds it reads
// the object. LoggingMiddleware writes r.URL.Path at INFO on every request, so
// without this every resolve attempt would put a working token into the log —
// where it outlives the link, gets shipped to whatever aggregator the operator
// runs, and is readable by everyone with access to it.
//
// A prefix rather than a regex, and a list rather than a guess: a route is added
// here deliberately, by the person who put a secret in a path.
var secretPathPrefixes = []string{
	"/api/v1/drive/links/",
}

// redactPath replaces the secret-bearing segment of a path with a placeholder,
// keeping the shape so the log is still useful for finding the route.
func redactPath(path string) string {
	for _, prefix := range secretPathPrefixes {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(path, prefix)
		// The secret is the first segment; anything after it is route structure
		// and is kept.
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			return prefix + "<redacted>" + rest[i:]
		}
		return prefix + "<redacted>"
	}
	return path
}

// RejectNULInURL refuses a request whose URL carries U+0000.
//
// The body guard in DecodeJSON cannot see this: a query parameter never passes
// through it, and `net/url` decodes `%00` into a real NUL byte that a handler
// then hands to Postgres. Demonstrated on `GET /api/v1/users/search`, one byte
// apart, any authenticated user, no body at all:
//
//	?q=a%00b -> 500 internal server error
//	?q=ab    -> 200
//
// It is a MIDDLEWARE rather than a helper because twelve handlers read
// r.URL.Query() directly instead of going through QueryParam, so a guard in the
// accessor would cover neither them nor a route written next year.
//
// Matching on the encoded form is exact, not a heuristic: `%00` is the only
// spelling of the NUL byte in a URL — its hex digits have no case variants, and
// a caller who wants the three literal characters sends `%2500`. The raw-byte
// check beside it covers a client that puts the byte in the request line
// unencoded.
func RejectNULInURL(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hasEncodedNULInURL(r.URL.RawQuery) || hasEncodedNULInURL(r.URL.EscapedPath()) {
			// BAD_REQUEST, not INVALID_BODY: there is no body involved, and
			// BAD_REQUEST is what 138 other refusals in this codebase use.
			// Inventing a code for one middleware would give clients something
			// new to handle for no gain.
			HandleError(w, &AppError{
				Status:  http.StatusBadRequest,
				Code:    "BAD_REQUEST",
				Message: "the URL contains a NUL character (U+0000), which cannot be stored",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func hasEncodedNULInURL(s string) bool {
	return strings.Contains(s, "%00") || strings.ContainsRune(s, 0)
}
