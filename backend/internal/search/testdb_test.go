package search

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The handler tests in this package run against a real Postgres and a real
// Meilisearch, on purpose.
//
// What is being tested is an authorization boundary that is enforced in two
// places at once: a SQL predicate (authz.Checker.KeysFor, via AccessKeys)
// and a Meilisearch filter expression. A mock of either one would assert that
// this package's Go is unchanged while leaving the part that actually decides
// whether one tenant can read another's private channels completely unchecked —
// and cross-tenant search was a real vulnerability here, not a hypothetical.
//
// Reachability is decided from the standard DB_* / MEILI_* environment. When
// the infrastructure is not there the tests skip, unless SUPEROPS_REQUIRE_INFRA=1
// forces them to fail (the same switch test/integration and internal/authz use).

// testDBName is per-package and per-process: `go test ./...` runs package test
// binaries concurrently, and two of them migrating the same database would
// race.
var testDBName = fmt.Sprintf("superops_search_test_%d", os.Getpid())

var (
	dbOnce sync.Once
	dbPool *pgxpool.Pool
	dbErr  error

	searchOnce sync.Once
	searchSvc  *Service
	searchErr  error
)

// env honours a set-but-empty variable as an explicit empty value: DB_PASSWORD
// is legitimately empty against a trust-auth server, and MEILI_HOST is
// legitimately empty when search is switched off.
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

// testDB returns a pool onto a freshly migrated throwaway database, shared by
// every test in the package.
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
		t.Skipf("postgres unavailable, skipping search database tests: %v", dbErr)
	}
	return dbPool
}

// testService returns a Service against the configured Meilisearch.
//
// The index name is a package constant, so this shares one index with every
// other process pointed at the same server. That is deliberate and safe: every
// fixture below lives in a workspace with a freshly generated uuid, so nothing
// another run wrote can satisfy a filter this one builds.
func testService(t *testing.T) *Service {
	t.Helper()
	searchOnce.Do(func() {
		host := env("MEILI_HOST", "http://127.0.0.1:7700")
		if host == "" {
			searchErr = fmt.Errorf("MEILI_HOST is empty")
			return
		}
		searchSvc, searchErr = NewService(host, env("MEILI_MASTER_KEY", "changeme_meili_master_key"),
			slog.New(slog.NewTextHandler(io.Discard, nil)))
	})
	if searchErr != nil {
		if requireInfra() {
			t.Fatalf("SUPEROPS_REQUIRE_INFRA=1 but Meilisearch is unusable: %v", searchErr)
		}
		t.Skipf("meilisearch unavailable, skipping search tests: %v", searchErr)
	}
	return searchSvc
}

func createTestDB(ctx context.Context) (*pgxpool.Pool, error) {
	maint, err := pgx.Connect(ctx, dsn(env("DB_MAINTENANCE_NAME", "postgres")))
	if err != nil {
		return nil, fmt.Errorf("connect to maintenance database: %w", err)
	}
	defer maint.Close(ctx)

	// The identifier is built from the process id, not from user input.
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
	if err := applyMigrations(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// applyMigrations replays migrations/*.up.sql in order. pgx uses the simple
// protocol whenever a query has no arguments, so a multi-statement file goes
// through in one call.
func applyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	dir := filepath.Join("..", "..", "migrations")
	names, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	if len(names) == 0 {
		return fmt.Errorf("no migrations found under %s", dir)
	}
	sort.Strings(names)
	for _, name := range names {
		body, err := os.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", filepath.Base(name), err)
		}
	}
	return nil
}

// TestMain drops the throwaway database again. Leaving it behind would slowly
// fill the server with one database per test run.
func TestMain(m *testing.M) {
	code := m.Run()
	if dbPool != nil {
		dbPool.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if maint, err := pgx.Connect(ctx, dsn(env("DB_MAINTENANCE_NAME", "postgres"))); err == nil {
			// Best effort: a failure here costs a leftover database, not a
			// wrong test result.
			_, _ = maint.Exec(ctx, `DROP DATABASE IF EXISTS "`+testDBName+`" WITH (FORCE)`)
			_ = maint.Close(ctx)
		}
	}
	os.Exit(code)
}
