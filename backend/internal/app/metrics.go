package app

import (
	"bufio"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/ws"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
)

// Lightweight, dependency-free Prometheus metrics. Exposes Go runtime stats
// plus HTTP request counters/latency in the text exposition format scraped by
// deploy/docker/prometheus.yml.

// latencyBucketsSeconds are the histogram's upper bounds. A cumulative
// seconds counter (what this used to export) only ever yields a global mean;
// buckets are what make histogram_quantile — and therefore a p99 SLO — possible.
var latencyBucketsSeconds = [11]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type metrics struct {
	started     time.Time
	requests2xx atomic.Int64
	requests3xx atomic.Int64
	requests4xx atomic.Int64
	requests5xx atomic.Int64
	wsUpgrades  atomic.Int64

	// Histogram state. buckets[i] counts observations in
	// (latencyBucketsSeconds[i-1], latencyBucketsSeconds[i]]; the extra trailing
	// slot is the +Inf overflow.
	buckets      [len(latencyBucketsSeconds) + 1]atomic.Int64
	requestNanos atomic.Int64
}

var appMetrics = &metrics{started: time.Now()}

func (m *metrics) observe(d time.Duration, status int) {
	m.requestNanos.Add(int64(d))

	secs := d.Seconds()
	idx := len(latencyBucketsSeconds)
	for i, upper := range latencyBucketsSeconds {
		if secs <= upper {
			idx = i
			break
		}
	}
	m.buckets[idx].Add(1)

	switch {
	case status >= 500:
		m.requests5xx.Add(1)
	case status >= 400:
		m.requests4xx.Add(1)
	case status >= 300:
		m.requests3xx.Add(1)
	default:
		m.requests2xx.Add(1)
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status   int
	hijacked bool
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Hijack delegates to the underlying ResponseWriter so the WebSocket upgrade
// works through the metrics middleware, and records that this request left the
// HTTP request/response model.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("underlying ResponseWriter does not support hijacking")
	}
	conn, rw, err := hj.Hijack()
	if err == nil {
		r.hijacked = true
	}
	return conn, rw, err
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// MetricsMiddleware records request counts by status class and a latency
// histogram.
//
// Recording happens in a defer: previously it ran after next.ServeHTTP
// returned, and RecoveryMiddleware sits further out, so a panic unwound past
// this middleware entirely — no 5xx increment, no latency sample. 5xx alerting
// could not fire on the one failure mode that most needs it.
//
// Hijacked connections are excluded. A WebSocket's "duration" is its whole
// multi-hour session; folding that into request latency destroyed every
// percentile in the histogram. They are counted separately as upgrades.
func MetricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		defer func() {
			if rec.hijacked {
				appMetrics.wsUpgrades.Add(1)
				return
			}
			status := rec.status
			if p := recover(); p != nil {
				// The handler never wrote a status; RecoveryMiddleware further
				// out will send the 500. Record it, then let the panic continue.
				appMetrics.observe(time.Since(start), http.StatusInternalServerError)
				panic(p)
			}
			appMetrics.observe(time.Since(start), status)
		}()

		next.ServeHTTP(rec, r)
	})
}

// connectionCounter is the optional interface a hub implements once it tracks
// more than one connection per user. Until then only the distinct-user gauge
// is exported.
type connectionCounter interface {
	ConnectionCount() int
}

func metricsHandler(hub *ws.Hub, pool *pgxpool.Pool, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Optional bearer-token guard so /metrics isn't world-readable when the
		// endpoint is reachable outside a trusted scrape network.
		if token != "" {
			provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if provided == "" {
				provided = r.URL.Query().Get("token")
			}
			if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}

		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		dbStats := database.PoolStats(pool)

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

		writeLatencyHistogram(w)

		fmt.Fprintf(w, "# HELP superops_ws_upgrades_total WebSocket upgrades accepted (hijacked connections, excluded from HTTP latency).\n")
		fmt.Fprintf(w, "# TYPE superops_ws_upgrades_total counter\n")
		fmt.Fprintf(w, "superops_ws_upgrades_total %d\n", appMetrics.wsUpgrades.Load())

		// This gauge used to be exported as superops_ws_connections_active,
		// which it never was: the hub keys clients by user id, so the value is
		// distinct users on this replica.
		fmt.Fprintf(w, "# HELP superops_ws_users_online Distinct users with at least one WebSocket connection on this replica.\n")
		fmt.Fprintf(w, "# TYPE superops_ws_users_online gauge\n")
		fmt.Fprintf(w, "superops_ws_users_online %d\n", len(hub.GetOnlineUserIDs()))

		if cc, ok := any(hub).(connectionCounter); ok {
			fmt.Fprintf(w, "# HELP superops_ws_connections_active Open WebSocket connections on this replica.\n")
			fmt.Fprintf(w, "# TYPE superops_ws_connections_active gauge\n")
			fmt.Fprintf(w, "superops_ws_connections_active %d\n", cc.ConnectionCount())
		}

		fmt.Fprintf(w, "# HELP superops_db_pool_conns Postgres pool connections by state.\n")
		fmt.Fprintf(w, "# TYPE superops_db_pool_conns gauge\n")
		fmt.Fprintf(w, "superops_db_pool_conns{state=\"acquired\"} %d\n", dbStats.AcquiredConns)
		fmt.Fprintf(w, "superops_db_pool_conns{state=\"idle\"} %d\n", dbStats.IdleConns)
		fmt.Fprintf(w, "superops_db_pool_conns{state=\"total\"} %d\n", dbStats.TotalConns)

		fmt.Fprintf(w, "# HELP superops_db_pool_max_conns Configured pool ceiling.\n")
		fmt.Fprintf(w, "# TYPE superops_db_pool_max_conns gauge\n")
		fmt.Fprintf(w, "superops_db_pool_max_conns %d\n", dbStats.MaxConns)

		fmt.Fprintf(w, "# HELP superops_db_pool_acquires_total Connection acquisitions, total and those that had to wait for a free slot.\n")
		fmt.Fprintf(w, "# TYPE superops_db_pool_acquires_total counter\n")
		fmt.Fprintf(w, "superops_db_pool_acquires_total{result=\"ok\"} %d\n", dbStats.AcquireCount)
		fmt.Fprintf(w, "superops_db_pool_acquires_total{result=\"empty\"} %d\n", dbStats.EmptyAcquireCount)
		fmt.Fprintf(w, "superops_db_pool_acquires_total{result=\"canceled\"} %d\n", dbStats.CanceledAcquireCount)

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

// writeLatencyHistogram emits the classic Prometheus histogram triple. Buckets
// are cumulative by definition (le = "less than or equal"), so the per-bucket
// counts are summed as they are written.
func writeLatencyHistogram(w http.ResponseWriter) {
	fmt.Fprintf(w, "# HELP superops_http_request_duration_seconds HTTP request latency, excluding hijacked (WebSocket) connections.\n")
	fmt.Fprintf(w, "# TYPE superops_http_request_duration_seconds histogram\n")

	var cumulative int64
	for i, upper := range latencyBucketsSeconds {
		cumulative += appMetrics.buckets[i].Load()
		fmt.Fprintf(w, "superops_http_request_duration_seconds_bucket{le=\"%s\"} %d\n",
			strconv.FormatFloat(upper, 'g', -1, 64), cumulative)
	}
	cumulative += appMetrics.buckets[len(latencyBucketsSeconds)].Load()

	fmt.Fprintf(w, "superops_http_request_duration_seconds_bucket{le=\"+Inf\"} %d\n", cumulative)
	fmt.Fprintf(w, "superops_http_request_duration_seconds_sum %f\n", float64(appMetrics.requestNanos.Load())/1e9)
	fmt.Fprintf(w, "superops_http_request_duration_seconds_count %d\n", cumulative)
}
