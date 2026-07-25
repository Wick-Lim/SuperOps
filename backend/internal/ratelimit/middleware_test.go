package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func requestWithXFF(remoteAddr, xff string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	r.RemoteAddr = remoteAddr
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return r
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		trustProxy bool
		hops       int
		want       string
	}{
		{
			name:       "no proxy trust ignores forged header",
			remoteAddr: "10.0.0.9:5555",
			xff:        "1.2.3.4",
			trustProxy: false,
			hops:       0,
			want:       "10.0.0.9",
		},
		{
			// nginx $proxy_add_x_forwarded_for appends the peer it saw, so the
			// spoofed value the client sent stays on the LEFT and the real
			// client address is the rightmost entry.
			name:       "one hop takes the rightmost entry, not the spoofed left one",
			remoteAddr: "10.0.0.9:5555",
			xff:        "1.2.3.4, 203.0.113.7",
			trustProxy: true,
			hops:       1,
			want:       "203.0.113.7",
		},
		{
			name:       "one hop is immune to however many entries the client prepends",
			remoteAddr: "10.0.0.9:5555",
			xff:        "1.1.1.1, 2.2.2.2, 3.3.3.3, 203.0.113.7",
			trustProxy: true,
			hops:       1,
			want:       "203.0.113.7",
		},
		{
			name:       "two hops (CDN then nginx) skips the CDN address",
			remoteAddr: "10.0.0.9:5555",
			xff:        "1.2.3.4, 203.0.113.7, 198.51.100.20",
			trustProxy: true,
			hops:       2,
			want:       "203.0.113.7",
		},
		{
			name:       "trust proxy with unset hops defaults to one hop",
			remoteAddr: "10.0.0.9:5555",
			xff:        "1.2.3.4, 203.0.113.7",
			trustProxy: true,
			hops:       0,
			want:       "203.0.113.7",
		},
		{
			name:       "fewer entries than configured hops falls back to the peer",
			remoteAddr: "10.0.0.9:5555",
			xff:        "203.0.113.7",
			trustProxy: true,
			hops:       3,
			want:       "10.0.0.9",
		},
		{
			name:       "missing header falls back to the peer",
			remoteAddr: "10.0.0.9:5555",
			xff:        "",
			trustProxy: true,
			hops:       1,
			want:       "10.0.0.9",
		},
		{
			name:       "non-IP at the trusted position falls back to the peer",
			remoteAddr: "10.0.0.9:5555",
			xff:        "1.2.3.4, unknown",
			trustProxy: true,
			hops:       1,
			want:       "10.0.0.9",
		},
		{
			name:       "IPv6 entry is normalized",
			remoteAddr: "10.0.0.9:5555",
			xff:        "1.2.3.4, 2001:0db8:0000:0000:0000:0000:0000:0001",
			trustProxy: true,
			hops:       1,
			want:       "2001:db8::1",
		},
		{
			name:       "entry carrying a port is accepted",
			remoteAddr: "10.0.0.9:5555",
			xff:        "1.2.3.4, 203.0.113.7:41234",
			trustProxy: true,
			hops:       1,
			want:       "203.0.113.7",
		},
		{
			name:       "surrounding whitespace is trimmed",
			remoteAddr: "10.0.0.9:5555",
			xff:        "1.2.3.4,   203.0.113.7   ",
			trustProxy: true,
			hops:       1,
			want:       "203.0.113.7",
		},
		{
			name:       "remote addr without a port is used verbatim",
			remoteAddr: "10.0.0.9",
			xff:        "",
			trustProxy: false,
			want:       "10.0.0.9",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clientIP(requestWithXFF(tt.remoteAddr, tt.xff), tt.trustProxy, tt.hops)
			if got != tt.want {
				t.Errorf("clientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

// A spoofed header must not be able to move a caller off its own bucket —
// that is the whole login brute-force limit.
func TestClientIPSpoofedHeaderCannotChangeBucket(t *testing.T) {
	const peer = "10.0.0.9:5555"
	base := clientIP(requestWithXFF(peer, "203.0.113.7"), true, 1)

	for _, forged := range []string{
		"1.2.3.4, 203.0.113.7",
		"9.9.9.9, 8.8.8.8, 203.0.113.7",
		", 203.0.113.7",
	} {
		if got := clientIP(requestWithXFF(peer, forged), true, 1); got != base {
			t.Errorf("XFF %q produced bucket %q, want %q", forged, got, base)
		}
	}
}

func TestWindowCutoff(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	got := windowCutoff(now, time.Minute)

	// Exclusive bound: an entry landing exactly on the window edge is still
	// inside the window and must survive the trim.
	if !strings.HasPrefix(got, "(") {
		t.Fatalf("cutoff %q must be an exclusive Redis score bound", got)
	}
	score, err := strconv.ParseInt(strings.TrimPrefix(got, "("), 10, 64)
	if err != nil {
		t.Fatalf("cutoff %q is not a parseable score: %v", got, err)
	}
	if want := now.Add(-time.Minute).UnixNano(); score != want {
		t.Errorf("cutoff score = %d, want %d", score, want)
	}
}

// Sorted-set members are deduplicated, so two requests arriving in the same
// nanosecond must still produce distinct members or the window undercounts.
func TestWindowMemberIsUnique(t *testing.T) {
	now := time.Unix(1_700_000_000, 12345)

	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		m := windowMember(now)
		if seen[m] {
			t.Fatalf("duplicate member %q after %d iterations", m, i)
		}
		seen[m] = true

		ts, _, ok := strings.Cut(m, ":")
		if !ok {
			t.Fatalf("member %q is missing its timestamp prefix", m)
		}
		if ts != strconv.FormatInt(now.UnixNano(), 10) {
			t.Fatalf("member %q does not score to the observation time", m)
		}
	}
}

func TestSubjectKeyNamespacesUsersAndIPs(t *testing.T) {
	cfg := Config{TrustProxy: false}
	r := requestWithXFF("203.0.113.7:1234", "")

	anon := subjectKey(r, cfg, nil)
	if anon != "ratelimit:api:ip:203.0.113.7" {
		t.Errorf("anonymous key = %q", anon)
	}

	// An identifier that looks exactly like an address must not land in the
	// address namespace.
	impostor := subjectKey(r, cfg, func(*http.Request) string { return "203.0.113.7" })
	if impostor == anon {
		t.Errorf("user key %q collides with the IP key", impostor)
	}

	empty := subjectKey(r, cfg, func(*http.Request) string { return "" })
	if empty != anon {
		t.Errorf("empty identifier should fall back to the IP key, got %q", empty)
	}
}

func TestConfigWindowDefaultsToOneMinute(t *testing.T) {
	if got := (Config{}).window(); got != time.Minute {
		t.Errorf("zero window = %v, want 1m", got)
	}
	if got := (Config{Window: 5 * time.Second}).window(); got != 5*time.Second {
		t.Errorf("explicit window = %v, want 5s", got)
	}
}
