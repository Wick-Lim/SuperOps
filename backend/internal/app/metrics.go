package app

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/Wick-Lim/SuperOps/backend/internal/ws"
)

// Lightweight, dependency-free Prometheus metrics. Exposes Go runtime stats
// plus HTTP request counters/latency in the text exposition format scraped by
// deploy/docker/prometheus.yml.
type metrics struct {
	started      time.Time
	requests2xx  atomic.Int64
	requests3xx  atomic.Int64
	requests4xx  atomic.Int64
	requests5xx  atomic.Int64
	requestNanos atomic.Int64 // cumulative latency
}

var appMetrics = &metrics{started: time.Now()}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Hijack delegates to the underlying ResponseWriter so the WebSocket upgrade
// works through the metrics middleware.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := r.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("underlying ResponseWriter does not support hijacking")
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// MetricsMiddleware records request counts by status class and latency.
func MetricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		appMetrics.requestNanos.Add(int64(time.Since(start)))
		switch {
		case rec.status >= 500:
			appMetrics.requests5xx.Add(1)
		case rec.status >= 400:
			appMetrics.requests4xx.Add(1)
		case rec.status >= 300:
			appMetrics.requests3xx.Add(1)
		default:
			appMetrics.requests2xx.Add(1)
		}
	})
}

func metricsHandler(hub *ws.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		wsActive := len(hub.GetOnlineUserIDs())

		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "# HELP superops_uptime_seconds Process uptime in seconds.\n")
		fmt.Fprintf(w, "# TYPE superops_uptime_seconds gauge\n")
		fmt.Fprintf(w, "superops_uptime_seconds %f\n", time.Since(appMetrics.started).Seconds())

		fmt.Fprintf(w, "# HELP superops_http_requests_total HTTP requests by status class.\n")
		fmt.Fprintf(w, "# TYPE superops_http_requests_total counter\n")
		fmt.Fprintf(w, "superops_http_requests_total{status=\"2xx\"} %d\n", appMetrics.requests2xx.Load())
		fmt.Fprintf(w, "superops_http_requests_total{status=\"3xx\"} %d\n", appMetrics.requests3xx.Load())
		fmt.Fprintf(w, "superops_http_requests_total{status=\"4xx\"} %d\n", appMetrics.requests4xx.Load())
		fmt.Fprintf(w, "superops_http_requests_total{status=\"5xx\"} %d\n", appMetrics.requests5xx.Load())

		fmt.Fprintf(w, "# HELP superops_http_request_duration_seconds_total Cumulative request latency.\n")
		fmt.Fprintf(w, "# TYPE superops_http_request_duration_seconds_total counter\n")
		fmt.Fprintf(w, "superops_http_request_duration_seconds_total %f\n", float64(appMetrics.requestNanos.Load())/1e9)

		fmt.Fprintf(w, "# HELP superops_ws_connections_active Active WebSocket connections.\n")
		fmt.Fprintf(w, "# TYPE superops_ws_connections_active gauge\n")
		fmt.Fprintf(w, "superops_ws_connections_active %d\n", wsActive)

		fmt.Fprintf(w, "# HELP go_goroutines Number of goroutines.\n")
		fmt.Fprintf(w, "# TYPE go_goroutines gauge\n")
		fmt.Fprintf(w, "go_goroutines %d\n", runtime.NumGoroutine())

		fmt.Fprintf(w, "# HELP go_memstats_alloc_bytes Allocated heap bytes.\n")
		fmt.Fprintf(w, "# TYPE go_memstats_alloc_bytes gauge\n")
		fmt.Fprintf(w, "go_memstats_alloc_bytes %d\n", m.Alloc)

		fmt.Fprintf(w, "# HELP go_memstats_sys_bytes Bytes obtained from the OS.\n")
		fmt.Fprintf(w, "# TYPE go_memstats_sys_bytes gauge\n")
		fmt.Fprintf(w, "go_memstats_sys_bytes %d\n", m.Sys)
	}
}
