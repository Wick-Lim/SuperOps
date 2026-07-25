package app

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMetricsObserveBuckets(t *testing.T) {
	tests := []struct {
		name    string
		latency time.Duration
		wantIdx int
	}{
		{"faster than the first bound", 1 * time.Millisecond, 0},
		{"exactly on a bound", 5 * time.Millisecond, 0},
		{"just past a bound", 6 * time.Millisecond, 1},
		{"mid range", 300 * time.Millisecond, 6},
		{"on the last bound", 10 * time.Second, len(latencyBucketsSeconds) - 1},
		{"overflow", 2 * time.Hour, len(latencyBucketsSeconds)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &metrics{}
			m.observe(tt.latency, http.StatusOK)

			for i := range m.buckets {
				want := int64(0)
				if i == tt.wantIdx {
					want = 1
				}
				if got := m.buckets[i].Load(); got != want {
					t.Errorf("bucket[%d] = %d, want %d", i, got, want)
				}
			}
		})
	}
}

func TestMetricsObserveStatusClasses(t *testing.T) {
	m := &metrics{}
	for _, status := range []int{200, 204, 301, 404, 422, 500, 503} {
		m.observe(time.Millisecond, status)
	}

	if got := m.requests2xx.Load(); got != 2 {
		t.Errorf("2xx = %d, want 2", got)
	}
	if got := m.requests3xx.Load(); got != 1 {
		t.Errorf("3xx = %d, want 1", got)
	}
	if got := m.requests4xx.Load(); got != 2 {
		t.Errorf("4xx = %d, want 2", got)
	}
	if got := m.requests5xx.Load(); got != 2 {
		t.Errorf("5xx = %d, want 2", got)
	}
}

// A panicking handler must still produce a 5xx sample; RecoveryMiddleware sits
// outside this middleware, so without a defer the panic used to unwind past
// every counter and 5xx alerting could not fire.
func TestMetricsMiddlewareRecordsPanicsAs5xx(t *testing.T) {
	before := appMetrics.requests5xx.Load()

	h := MetricsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic must keep propagating to RecoveryMiddleware")
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/boom", nil))
	}()

	if got := appMetrics.requests5xx.Load(); got != before+1 {
		t.Errorf("5xx counter = %d, want %d", got, before+1)
	}
}

// hijackableRecorder is an httptest.ResponseRecorder that can be hijacked, so
// the WebSocket path can be exercised without a real socket.
type hijackableRecorder struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (h *hijackableRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return h.conn, bufio.NewReadWriter(bufio.NewReader(h.conn), bufio.NewWriter(h.conn)), nil
}

func TestMetricsMiddlewareSkipsHijackedConnections(t *testing.T) {
	beforeUpgrades := appMetrics.wsUpgrades.Load()
	beforeNanos := appMetrics.requestNanos.Load()
	before2xx := appMetrics.requests2xx.Load()

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	h := MetricsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("the metrics wrapper must stay hijackable or the WS upgrade breaks")
		}
		if _, _, err := hj.Hijack(); err != nil {
			t.Fatalf("hijack: %v", err)
		}
		// Stand in for a long-lived WebSocket session.
		time.Sleep(20 * time.Millisecond)
	}))

	rec := &hijackableRecorder{ResponseRecorder: httptest.NewRecorder(), conn: server}
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/ws", nil))

	if got := appMetrics.wsUpgrades.Load(); got != beforeUpgrades+1 {
		t.Errorf("ws upgrades = %d, want %d", got, beforeUpgrades+1)
	}
	if got := appMetrics.requestNanos.Load(); got != beforeNanos {
		t.Error("a hijacked connection's lifetime must not pollute request latency")
	}
	if got := appMetrics.requests2xx.Load(); got != before2xx {
		t.Error("a hijacked connection must not be counted as an HTTP response")
	}
}

func TestWriteLatencyHistogramIsWellFormed(t *testing.T) {
	w := httptest.NewRecorder()
	writeLatencyHistogram(w)
	body := w.Body.String()

	for _, want := range []string{
		"# TYPE superops_http_request_duration_seconds histogram",
		`superops_http_request_duration_seconds_bucket{le="0.005"}`,
		`superops_http_request_duration_seconds_bucket{le="+Inf"}`,
		"superops_http_request_duration_seconds_sum",
		"superops_http_request_duration_seconds_count",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition is missing %q:\n%s", want, body)
		}
	}

	// Buckets must be monotonically non-decreasing (they are cumulative) —
	// getting that wrong makes histogram_quantile return nonsense.
	var prev int64 = -1
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "superops_http_request_duration_seconds_bucket") {
			continue
		}
		fields := strings.Fields(line)
		v, err := strconv.ParseInt(fields[len(fields)-1], 10, 64)
		if err != nil {
			t.Fatalf("unparseable bucket line %q", line)
		}
		if v < prev {
			t.Errorf("bucket counts are not cumulative: %d after %d in %q", v, prev, line)
		}
		prev = v
	}
}
