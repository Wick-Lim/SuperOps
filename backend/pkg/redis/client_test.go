package redis

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests use the live Redis from the deployment's docker-compose /
// development environment rather than a fake: the only behaviour worth pinning
// here is what go-redis actually does on connect — the timeouts that keep a
// hung Redis from holding request goroutines past the server write timeout, and
// the fact that a bad password is reported instead of surfacing later on the
// first rate-limit call.
//
// Reachability comes from REDIS_ADDR / REDIS_PASSWORD. Unreachable means skip,
// unless SUPEROPS_REQUIRE_INFRA=1 forces a failure.

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

func requireInfra() bool {
	b, err := strconv.ParseBool(os.Getenv("SUPEROPS_REQUIRE_INFRA"))
	return err == nil && b
}

func testConfig() Config {
	return Config{
		Addr:     env("REDIS_ADDR", "127.0.0.1:6379"),
		Password: env("REDIS_PASSWORD", ""),
		DB:       0,
	}
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

var (
	probeOnce sync.Once
	probeErr  error
)

// requireRedis skips (or fails) when the configured Redis is unusable, so a
// broken password does not read as "all green".
func requireRedis(t *testing.T) {
	t.Helper()
	probeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, err := NewClient(ctx, testConfig(), discard())
		probeErr = err
		if err == nil {
			_ = c.Close()
		}
	})
	if probeErr != nil {
		if requireInfra() {
			t.Fatalf("SUPEROPS_REQUIRE_INFRA=1 but Redis is unusable: %v", probeErr)
		}
		t.Skipf("redis unavailable, skipping: %v", probeErr)
	}
}

func TestNewClientAppliesDefaults(t *testing.T) {
	requireRedis(t)

	client, err := NewClient(t.Context(), testConfig(), discard())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	opts := client.Options()
	// Zero values in Config must not reach go-redis: its own defaults let a
	// hung Redis hold a request goroutine longer than the HTTP write timeout.
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"dial timeout", opts.DialTimeout, DefaultDialTimeout},
		{"read timeout", opts.ReadTimeout, DefaultReadTimeout},
		{"write timeout", opts.WriteTimeout, DefaultWriteTimeout},
		{"pool size", opts.PoolSize, DefaultPoolSize},
		{"pool timeout", opts.PoolTimeout, DefaultPoolTimeout},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
		}
	}
}

func TestNewClientHonoursExplicitSettings(t *testing.T) {
	requireRedis(t)

	cfg := testConfig()
	cfg.DialTimeout = 7 * time.Second
	cfg.ReadTimeout = 3 * time.Second
	cfg.WriteTimeout = 4 * time.Second
	cfg.PoolSize = 5
	cfg.PoolTimeout = 6 * time.Second

	client, err := NewClient(t.Context(), cfg, discard())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	opts := client.Options()
	if opts.DialTimeout != cfg.DialTimeout || opts.ReadTimeout != cfg.ReadTimeout ||
		opts.WriteTimeout != cfg.WriteTimeout || opts.PoolSize != cfg.PoolSize ||
		opts.PoolTimeout != cfg.PoolTimeout {
		t.Errorf("explicit settings were overridden: %+v", opts)
	}
}

// TestNewClientPingsBeforeReturning is the reason NewClient does a round trip:
// a client that only looks connected turns a misconfiguration into a failure on
// the first rate-limit check, long after startup.
func TestNewClientPingsBeforeReturning(t *testing.T) {
	requireRedis(t)

	t.Run("a working client answers commands", func(t *testing.T) {
		client, err := NewClient(t.Context(), testConfig(), discard())
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		t.Cleanup(func() { _ = client.Close() })

		key := "superops-test-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		if err := client.Set(t.Context(), key, "v", time.Minute).Err(); err != nil {
			t.Fatalf("SET: %v", err)
		}
		t.Cleanup(func() { _ = client.Del(context.Background(), key).Err() })
		got, err := client.Get(t.Context(), key).Result()
		if err != nil || got != "v" {
			t.Errorf("GET = %q, %v; want \"v\", nil", got, err)
		}
	})

	// A WRONG PASSWORD IS ONLY WRONG IF THE SERVER WANTS ONE.
	//
	// This asserted a refusal unconditionally, and it is RED against a Redis
	// with no requirepass — which is how CI runs Redis. It never went red
	// because CI never ran this package at all: the workflow claimed these
	// tests were "covered by the integration job", and that job runs
	// ./test/integration/... alone.
	//
	// Both configurations are now stated, and neither is skipped. Which arm
	// runs is decided by asking the server, not by trusting the environment.
	t.Run("a wrong password is reported when the server requires one", func(t *testing.T) {
		cfg := testConfig()
		requiresAuth := serverRequiresAuth(t, cfg)

		cfg.Password = "definitely-not-the-password"
		client, err := NewClient(t.Context(), cfg, discard())

		if requiresAuth {
			if err == nil {
				_ = client.Close()
				t.Fatal("NewClient accepted a wrong password against a server that requires one")
			}
			if client != nil {
				t.Error("NewClient returned a client alongside an error")
			}
			return
		}

		// No requirepass: the password is meaningless and the connection
		// succeeds. The contract here is that it is not SILENT — NewClient warns
		// that the connection is unauthenticated, because a deployment that set
		// REDIS_PASSWORD believes otherwise.
		var buf bytes.Buffer
		client, err = NewClient(t.Context(), cfg,
			slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
		if err != nil {
			t.Fatalf("NewClient failed against a server with no requirepass: %v", err)
		}
		_ = client.Close()
		if !strings.Contains(buf.String(), "requires no authentication") {
			t.Errorf("connecting unauthenticated was silent; log was %q", buf.String())
		}
	})

	t.Run("an unreachable address is reported at construction", func(t *testing.T) {
		cfg := testConfig()
		// Port 1 is reserved and never listening.
		cfg.Addr = "127.0.0.1:1"
		cfg.DialTimeout = 500 * time.Millisecond
		client, err := NewClient(t.Context(), cfg, discard())
		if err == nil {
			_ = client.Close()
			t.Fatal("NewClient accepted an unreachable address")
		}
		if client != nil {
			t.Error("NewClient returned a client alongside an error")
		}
	})

	t.Run("a cancelled context does not hang", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		client, err := NewClient(ctx, testConfig(), discard())
		if err == nil {
			_ = client.Close()
			t.Fatal("NewClient ignored a cancelled context")
		}
	})
}

func TestOrDuration(t *testing.T) {
	const fallback = 5 * time.Second
	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"zero falls back", 0, fallback},
		{"negative falls back", -time.Second, fallback},
		{"explicit value wins", time.Second, time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := orDuration(tt.in, fallback); got != tt.want {
				t.Errorf("orDuration(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestOrInt(t *testing.T) {
	const fallback = 20
	tests := []struct {
		name string
		in   int
		want int
	}{
		{"zero falls back", 0, fallback},
		{"negative falls back", -1, fallback},
		{"explicit value wins", 3, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := orInt(tt.in, fallback); got != tt.want {
				t.Errorf("orInt(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// serverRequiresAuth asks the server rather than the environment, so this test
// states the same contract on a developer's password-protected Redis and on
// CI's password-less one.
func serverRequiresAuth(t *testing.T, cfg Config) bool {
	t.Helper()
	bare := cfg
	bare.Password = ""
	client, err := NewClient(t.Context(), bare, discard())
	if err != nil {
		// Refused without a password: it wants one.
		return true
	}
	_ = client.Close()
	return false
}
