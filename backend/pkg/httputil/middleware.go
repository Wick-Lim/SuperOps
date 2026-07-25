package httputil

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
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
					"path", r.URL.Path,
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
