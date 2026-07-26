package database

import (
	"context"
	"log/slog"
	"testing"
	"time"
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

// And an operator's explicit connect_timeout wins, because that is what every
// other DSN parameter in this file does.
func TestAnExplicitConnectTimeoutWins(t *testing.T) {
	cfg := Config{
		DSN:            "postgres://u:p@192.0.2.1:5432/db?sslmode=disable&connect_timeout=1",
		MaxConns:       2,
		ConnectTimeout: time.Hour, // ignored: the DSN said 1 second
	}
	start := time.Now()
	pool, err := NewPool(context.Background(), cfg, slog.New(slog.DiscardHandler))
	elapsed := time.Since(start)
	if err == nil {
		pool.Close()
		t.Fatal("NewPool succeeded against an unroutable address")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("the DSN's connect_timeout was overridden: took %s", elapsed)
	}
}
