package sso

import (
	"context"
	"fmt"
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

// The SSO tests run against a real Postgres and a real (in-process) identity
// provider, on purpose.
//
// What this package gets wrong is not Go: it is a SQL predicate that links the
// wrong account, a single-use token that turns out to be reusable, or a
// signature check that passes something it should not. A mocked pool and a
// mocked provider would assert that the code around those is unchanged while
// leaving all three unexamined. So: a throwaway database with the real
// migrations, and an httptest OIDC provider whose keys the test controls, which
// is what makes it possible to sign a *genuinely* invalid token rather than a
// string a mock agreed to reject.
//
// Reachability is decided from the standard DB_* environment. When Postgres is
// not there the suite skips, unless SUPEROPS_REQUIRE_INFRA=1 forces a failure.

var testDBName = fmt.Sprintf("superops_sso_test_%d", os.Getpid())

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
		t.Skipf("postgres unavailable, skipping sso database tests: %v", dbErr)
	}
	return dbPool
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
