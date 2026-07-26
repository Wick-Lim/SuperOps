package database

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AN UNREACHABLE DATABASE MUST FAIL FAST AND LOUDLY.
//
// It used to hang: pgx's dial had no deadline of its own and Ping was handed a
// context with none either, so the process sat for 35 seconds and counting with
// ZERO bytes of output. That is the worst shape a boot failure can take — a
// crash is a page and a log line, a silent hang is a container that never
// becomes ready and an operator with nothing to read.
//
// 192.0.2.1 is TEST-NET-1 (RFC 5737): routable-looking, never routed, so the
// dial hangs rather than being refused. A refused connection would prove
// nothing — that path always returned promptly.
func TestNewPoolBoundsAnUnreachableDatabase(t *testing.T) {
	cfg := Config{
		DSN:            "postgres://u:p@192.0.2.1:5432/db?sslmode=disable",
		MaxConns:       2,
		MinConns:       0,
		ConnectTimeout: 500 * time.Millisecond,
	}

	// Deliberately NOT a context with a deadline: the point is that NewPool
	// bounds itself. A deadline here would prove the test's patience, not the
	// code's.
	start := time.Now()
	pool, err := NewPool(context.Background(), cfg, slog.New(slog.DiscardHandler))
	elapsed := time.Since(start)

	if err == nil {
		pool.Close()
		t.Fatal("NewPool succeeded against an unroutable address")
	}
	// Two dial windows plus scheduling slack. Generous, because the assertion
	// that matters is "bounded at all", not the exact number.
	if elapsed > 5*time.Second {
		t.Fatalf("NewPool took %s against an unreachable database; it is unbounded again", elapsed)
	}
	if pool != nil {
		t.Error("NewPool returned a pool alongside an error")
	}
}

// A DSN connect_timeout is respected for the DIAL — but the ping deadline is
// derived from the Go field, so it caps the total.
//
// This test used to claim "the DSN wins" and passed with the ConnectTimeout
// guard deleted, because it only checked an upper bound loose enough for either
// behaviour. An audit measured the real contract: DSN=60s alone gives 20s (the
// ping's 2 x default), and only setting BOTH gives 60. Stating it exactly is
// the point — a comment promising an override the code does not honour is
// worse than no comment.
func TestADSNConnectTimeoutBoundsTheDialButThePingCapsTheTotal(t *testing.T) {
	cfg := Config{
		DSN:      "postgres://u:p@192.0.2.1:5432/db?sslmode=disable&connect_timeout=30",
		MaxConns: 2,
		// Deliberately short: the ping deadline is 2 x this, so it is what the
		// caller actually waits, NOT the DSN's 30 seconds.
		ConnectTimeout: 400 * time.Millisecond,
	}
	start := time.Now()
	pool, err := NewPool(context.Background(), cfg, slog.New(slog.DiscardHandler))
	elapsed := time.Since(start)
	if err == nil {
		pool.Close()
		t.Fatal("NewPool succeeded against an unroutable address")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("took %s: the ping deadline is not bounding the total, so a DSN "+
			"connect_timeout can hold the process far longer than the config says", elapsed)
	}
}

// THE DIAL DEADLINE IS FOR THE CONNECTIONS THE PING NEVER SEES.
//
// It cannot be isolated behaviourally through NewPool: the ping's deadline is
// derived from the same field and is always twice it, so the ping ends every
// construction first. That is exactly why deleting the dial timeout left the
// boot tests green — they measure construction, and construction is the case
// the ping covers.
//
// What the dial timeout is actually for is the pool's LAZY connections, opened
// long after NewPool returned, to a host that has since become unreachable.
// Nothing pings those. Without ConnConfig.ConnectTimeout pgx floors them at two
// minutes per fallback address, which is a request hanging for two minutes on a
// database that is simply gone.
//
// So this asserts the configuration rather than a duration. A white-box test is
// the honest shape here: the alternative is a test that appears to measure the
// dial and is really measuring the ping.
func TestTheDialDeadlineIsSetOnTheConnectionConfig(t *testing.T) {
	// A reachable-looking DSN is not required; ParseConfig does not connect.
	cfg := Config{
		DSN:            "postgres://u:p@127.0.0.1:1/db?sslmode=disable",
		MaxConns:       2,
		ConnectTimeout: 7 * time.Second,
	}
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		t.Fatal(err)
	}
	applyConnectTimeout(poolCfg, cfg)
	if got := poolCfg.ConnConfig.ConnectTimeout; got != 7*time.Second {
		t.Errorf("ConnConfig.ConnectTimeout = %s, want 7s — a pooled connection "+
			"opened after startup would fall back to pgx's two-minute floor", got)
	}

	// And an explicit DSN value still wins, because that is the operator being
	// deliberate.
	dsnCfg, err := pgxpool.ParseConfig(
		"postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=3")
	if err != nil {
		t.Fatal(err)
	}
	applyConnectTimeout(dsnCfg, cfg)
	if got := dsnCfg.ConnConfig.ConnectTimeout; got != 3*time.Second {
		t.Errorf("a DSN connect_timeout of 3s became %s", got)
	}
}
