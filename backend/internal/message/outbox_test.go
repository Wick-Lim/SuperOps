package message

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var testDBName = fmt.Sprintf("superops_msg_test_%d", os.Getpid())

var (
	dbOnce sync.Once
	dbPool *pgxpool.Pool
	dbErr  error
)

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

func dsn(database string) string {
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s",
		url.QueryEscape(env("DB_USER", "superops")),
		url.QueryEscape(env("DB_PASSWORD", "")),
		env("DB_HOST", "127.0.0.1"),
		env("DB_PORT", "5432"),
		database,
		env("DB_SSLMODE", "disable"),
	)
}

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		dbPool, dbErr = createTestDB(ctx)
	})
	if dbErr != nil {
		if requireInfra() {
			t.Fatalf("SUPEROPS_REQUIRE_INFRA=1 but Postgres is unusable: %v", dbErr)
		}
		t.Skipf("postgres unavailable, skipping message outbox tests: %v", dbErr)
	}
	return dbPool
}

func createTestDB(ctx context.Context) (*pgxpool.Pool, error) {
	maint, err := pgx.Connect(ctx, dsn(env("DB_MAINTENANCE_NAME", "postgres")))
	if err != nil {
		return nil, fmt.Errorf("connect to maintenance database: %w", err)
	}
	defer maint.Close(ctx)

	if _, err := maint.Exec(ctx, `DROP DATABASE IF EXISTS "`+testDBName+`"`); err != nil {
		return nil, fmt.Errorf("drop stale test database: %w", err)
	}
	if _, err := maint.Exec(ctx, `CREATE DATABASE "`+testDBName+`"`); err != nil {
		return nil, fmt.Errorf("create test database: %w", err)
	}

	pool, err := pgxpool.New(ctx, dsn(testDBName))
	if err != nil {
		return nil, fmt.Errorf("open test database: %w", err)
	}
	dir := filepath.Join("..", "..", "migrations")
	names, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	sort.Strings(names)
	for _, name := range names {
		body, err := os.ReadFile(name)
		if err != nil {
			pool.Close()
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			pool.Close()
			return nil, fmt.Errorf("apply %s: %w", filepath.Base(name), err)
		}
	}
	return pool, nil
}

func TestMain(m *testing.M) {
	code := m.Run()
	if dbPool != nil {
		dbPool.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if maint, err := pgx.Connect(ctx, dsn(env("DB_MAINTENANCE_NAME", "postgres"))); err == nil {
			_, _ = maint.Exec(ctx, `DROP DATABASE IF EXISTS "`+testDBName+`" WITH (FORCE)`)
			_ = maint.Close(ctx)
		}
	}
	os.Exit(code)
}

func TestDomainEventOutboxPersistsAndSweeps(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	repo := NewRepository(pool)
	h := NewHandler(repo, nil, nil, nil)

	wsID := uuid.NewString()
	msgID := uuid.NewString()
	dedupe := eventKey(evtMessageNew, msgID)

	// 1. Handler.publish records the event in domain_event_outbox even when NATS is nil/unavailable.
	h.publish(ctx, wsID, evtMessageNew, dedupe, map[string]any{
		"id":      msgID,
		"content": "durable hello",
	})

	// 2. First sweep fails with a transient broker error -> row stays unpublished with attempts = 1.
	published, err := repo.SweepOutbox(ctx, 10, func(ctx context.Context, ev OutboxEvent) error {
		return errors.New("nats connection closed")
	})
	if err != nil {
		t.Fatalf("SweepOutbox (failing): %v", err)
	}
	if published != 0 {
		t.Fatalf("published = %d, want 0", published)
	}

	var attempts int
	var lastErr string
	if err := pool.QueryRow(ctx,
		`SELECT attempts, COALESCE(last_error, '') FROM domain_event_outbox WHERE dedupe_id = $1`,
		dedupe).Scan(&attempts, &lastErr); err != nil {
		t.Fatalf("query outbox row: %v", err)
	}
	if attempts != 1 || lastErr == "" {
		t.Fatalf("attempts=%d lastErr=%q, want attempts=1 and non-empty lastErr", attempts, lastErr)
	}

	// Fast-forward next_attempt_at so the retry is due immediately.
	if _, err := pool.Exec(ctx,
		`UPDATE domain_event_outbox SET next_attempt_at = NOW() - INTERVAL '1 second' WHERE dedupe_id = $1`,
		dedupe); err != nil {
		t.Fatalf("fast-forward next_attempt_at: %v", err)
	}

	// 3. Second sweep succeeds -> event is delivered and marked published_at.
	var delivered []OutboxEvent
	published, err = repo.SweepOutbox(ctx, 10, func(ctx context.Context, ev OutboxEvent) error {
		delivered = append(delivered, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("SweepOutbox (succeeding): %v", err)
	}
	if published != 1 || len(delivered) != 1 {
		t.Fatalf("published=%d delivered=%d, want 1", published, len(delivered))
	}
	if delivered[0].Subject != "superops."+wsID+".message.created" {
		t.Fatalf("subject = %q, want superops.%s.message.created", delivered[0].Subject, wsID)
	}
	if delivered[0].DedupeID != dedupe {
		t.Fatalf("dedupe_id = %q, want %q", delivered[0].DedupeID, dedupe)
	}

	// 4. Subsequent sweep finds nothing left to publish.
	publishedAgain, err := repo.SweepOutbox(ctx, 10, func(ctx context.Context, ev OutboxEvent) error {
		t.Fatal("already-published event was re-swept")
		return nil
	})
	if err != nil || publishedAgain != 0 {
		t.Fatalf("subsequent SweepOutbox = (%d, %v), want (0, nil)", publishedAgain, err)
	}
}
