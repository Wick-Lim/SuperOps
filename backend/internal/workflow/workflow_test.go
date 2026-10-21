package workflow

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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var testDBName = fmt.Sprintf("superops_workflow_test_%d", os.Getpid())

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
		t.Skipf("postgres unavailable, skipping workflow database tests: %v", dbErr)
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

type fakeAction struct {
	kind  string
	calls int
	fn    func(ctx context.Context, run *Run, config map[string]any) (any, error)
}

func (a *fakeAction) Kind() string { return a.kind }

func (a *fakeAction) Perform(ctx context.Context, run *Run, config map[string]any) (any, error) {
	a.calls++
	if a.fn != nil {
		return a.fn(ctx, run, config)
	}
	return map[string]any{"ok": true}, nil
}

// TestEffectStateMachineRetriesInterruptedClaimAndDeduplicatesSucceeded verifies
// that a worker crash after ClaimEffect (before RecordEffectResult) leaves the
// effect in 'claimed' so a released stale run actually executes the step on
// retry, whereas a step that reached 'succeeded' is skipped on retry.
func TestEffectStateMachineRetriesInterruptedClaimAndDeduplicatesSucceeded(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	userID := uuid.NewString()
	wsID := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, username, full_name, password_hash, is_active)
		 VALUES ($1, $2, $3, 'WF Owner', 'h', true)`,
		userID, "wf-"+userID+"@example.com", "wf_"+strings.ReplaceAll(userID[:8], "-", ""),
	); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO workspaces (id, name, slug, owner_id) VALUES ($1, 'WF WS', $2, $3)`,
		wsID, "wf-"+wsID[:8], userID,
	); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`,
		wsID, userID,
	); err != nil {
		t.Fatalf("insert workspace member: %v", err)
	}

	repo := NewRepository(pool)
	if _, err := repo.Save(ctx, wsID, "", "Test Flow", "", []Step{
		{Kind: StepPostMessage, Config: map[string]any{"body": "step 0"}},
		{Kind: StepNotify, Config: map[string]any{"title": "step 1"}},
	}, []Trigger{{EventType: "message.created"}}, true, userID); err != nil {
		t.Fatalf("Save workflow: %v", err)
	}

	matches, err := repo.MatchingWorkflows(ctx, wsID, "message.created")
	if err != nil || len(matches) != 1 {
		t.Fatalf("MatchingWorkflows = %v, %v", matches, err)
	}
	runID, err := repo.EnqueueRun(ctx, matches[0], "message.created", "trig-1", []byte(`{"id":"m1"}`))
	if err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}

	run, steps, err := repo.ClaimRun(ctx)
	if err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	if run.ID != runID {
		t.Fatalf("claimed run %s, want %s", run.ID, runID)
	}

	// Simulate Worker 1:
	// - Step 0 claims effect AND completes (RecordEffectResult -> 'succeeded').
	// - Step 1 claims effect ('claimed') and Worker 1 crashes before Perform finishes!
	fresh0, err := repo.ClaimEffect(ctx, run.ID, 0, StepPostMessage)
	if err != nil || !fresh0 {
		t.Fatalf("ClaimEffect step 0 = (%v, %v), want (true, nil)", fresh0, err)
	}
	repo.RecordEffectResult(ctx, run.ID, 0, map[string]any{"message_id": "m-posted"})

	fresh1, err := repo.ClaimEffect(ctx, run.ID, 1, StepNotify)
	if err != nil || !fresh1 {
		t.Fatalf("ClaimEffect step 1 = (%v, %v), want (true, nil)", fresh1, err)
	}

	// Simulate Reaper releasing the stale run, and Worker 2 claiming and executing it.
	if _, err := pool.Exec(ctx,
		`UPDATE workflow_runs SET started_at = NOW() - INTERVAL '10 minutes' WHERE id = $1`, run.ID); err != nil {
		t.Fatalf("age run: %v", err)
	}
	released, err := repo.ReleaseStaleRuns(ctx, 5*time.Minute)
	if err != nil || released != 1 {
		t.Fatalf("ReleaseStaleRuns = (%d, %v), want (1, nil)", released, err)
	}

	run2, _, err := repo.ClaimRun(ctx)
	if err != nil {
		t.Fatalf("ClaimRun 2: %v", err)
	}

	postAct := &fakeAction{kind: StepPostMessage}
	notifyAct := &fakeAction{kind: StepNotify}
	exec := NewExecutor(repo, slog.New(slog.NewTextHandler(io.Discard, nil)), postAct, notifyAct)

	if err := exec.Run(ctx, run2, steps); err != nil {
		t.Fatalf("Executor.Run: %v", err)
	}

	// Step 0 ('succeeded') must NOT have been re-executed; Step 1 ('claimed' when
	// Worker 1 crashed) MUST have been executed once by Worker 2!
	if postAct.calls != 0 {
		t.Fatalf("step 0 (succeeded) was re-executed %d times, want 0", postAct.calls)
	}
	if notifyAct.calls != 1 {
		t.Fatalf("step 1 (interrupted 'claimed') was executed %d times, want 1", notifyAct.calls)
	}

	// And a subsequent ClaimEffect on step 1 now returns false because it succeeded.
	fresh1After, err := repo.ClaimEffect(ctx, run.ID, 1, StepNotify)
	if err != nil || fresh1After {
		t.Fatalf("ClaimEffect step 1 after completion = (%v, %v), want (false, nil)", fresh1After, err)
	}
}
