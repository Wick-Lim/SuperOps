package database

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	DSN      string
	MaxConns int32
	MinConns int32

	// Server-side guardrails, applied as connection RuntimeParams so they hold
	// for every query on every pooled connection. Without them a single
	// pathological query (a seq scan over messages, a lock wait behind a long
	// transaction) pins a pool slot until the client goes away and the pool
	// starves for everyone else. Zero means "leave the server default".
	StatementTimeout         time.Duration
	LockTimeout              time.Duration
	IdleInTransactionTimeout time.Duration

	MaxConnLifetime   time.Duration
	MaxConnIdleTime   time.Duration
	HealthCheckPeriod time.Duration
}

// Stats is a snapshot of pgxpool counters, exported so the metrics handler can
// publish pool saturation without importing pgxpool itself.
type Stats struct {
	AcquiredConns        int32
	IdleConns            int32
	TotalConns           int32
	MaxConns             int32
	AcquireCount         int64
	EmptyAcquireCount    int64
	CanceledAcquireCount int64
}

// PoolStats snapshots a pool. A nil pool yields the zero value so callers do
// not have to branch on optional infrastructure.
func PoolStats(pool *pgxpool.Pool) Stats {
	if pool == nil {
		return Stats{}
	}
	s := pool.Stat()
	return Stats{
		AcquiredConns:        s.AcquiredConns(),
		IdleConns:            s.IdleConns(),
		TotalConns:           s.TotalConns(),
		MaxConns:             s.MaxConns(),
		AcquireCount:         s.AcquireCount(),
		EmptyAcquireCount:    s.EmptyAcquireCount(),
		CanceledAcquireCount: s.CanceledAcquireCount(),
	}
}

func NewPool(ctx context.Context, cfg Config, logger *slog.Logger) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse database config: %w", err)
	}

	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = orDuration(cfg.MaxConnLifetime, 30*time.Minute)
	poolCfg.MaxConnIdleTime = orDuration(cfg.MaxConnIdleTime, 5*time.Minute)
	poolCfg.HealthCheckPeriod = orDuration(cfg.HealthCheckPeriod, 30*time.Second)

	if poolCfg.ConnConfig.RuntimeParams == nil {
		poolCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	// A value already present in the DSN wins — that is the operator being
	// explicit, and it is how a DATABASE_URL override is expected to behave.
	setTimeoutParam(poolCfg.ConnConfig.RuntimeParams, "statement_timeout", cfg.StatementTimeout)
	setTimeoutParam(poolCfg.ConnConfig.RuntimeParams, "lock_timeout", cfg.LockTimeout)
	setTimeoutParam(poolCfg.ConnConfig.RuntimeParams, "idle_in_transaction_session_timeout", cfg.IdleInTransactionTimeout)

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	logger.Info("connected to PostgreSQL",
		"host", poolCfg.ConnConfig.Host,
		"database", poolCfg.ConnConfig.Database,
		"max_conns", poolCfg.MaxConns,
		"statement_timeout", poolCfg.ConnConfig.RuntimeParams["statement_timeout"],
		"lock_timeout", poolCfg.ConnConfig.RuntimeParams["lock_timeout"],
	)

	return pool, nil
}

// setTimeoutParam writes a Postgres millisecond timeout parameter, leaving an
// existing (DSN-supplied) value untouched.
func setTimeoutParam(params map[string]string, name string, d time.Duration) {
	if d <= 0 {
		return
	}
	if _, ok := params[name]; ok {
		return
	}
	params[name] = strconv.FormatInt(d.Milliseconds(), 10)
}

func orDuration(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}
