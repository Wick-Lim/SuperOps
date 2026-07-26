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

	// ConnectTimeout bounds ONE dial. Zero means DefaultConnectTimeout; a value
	// already in the DSN wins over both, because that is the operator being
	// explicit.
	ConnectTimeout time.Duration
}

// DefaultConnectTimeout bounds the first connection so an unreachable database
// is a fast, loud failure instead of a process that never says anything.
//
// Ten seconds: long enough to survive a slow DNS answer or a database still
// coming up beside us in compose, short enough that a container which will
// never become ready says so before an orchestrator's own patience runs out.
const DefaultConnectTimeout = 10 * time.Second

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

	// THE FIRST CONNECTION IS BOUNDED. Without this the process HANGS with no
	// output at all when the database is unreachable — verified at 35 seconds
	// and zero bytes against a black-holed address.
	//
	// That is the worst shape a boot failure can take. A crash is a page and a
	// log line; a silent hang is a container that never becomes ready, an
	// orchestrator that keeps waiting, and an operator with nothing to read.
	// Two things were missing: pgx's default dial has no deadline of its own,
	// and Ping was handed a context with none either.
	//
	// connect_timeout is set on the CONFIG rather than the DSN string so an
	// operator's explicit DATABASE_URL value still wins — ParseConfig has
	// already applied theirs by this point, and a zero here means they said
	// nothing.
	if poolCfg.ConnConfig.ConnectTimeout == 0 {
		poolCfg.ConnConfig.ConnectTimeout = orDuration(cfg.ConnectTimeout, DefaultConnectTimeout)
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	// And the ping gets its own deadline, because ConnectTimeout bounds one
	// dial while the pool may retry: a name that resolves to several dead
	// addresses is several timeouts in a row, which is a hang again with extra
	// steps.
	pingCtx, cancel := context.WithTimeout(ctx, orDuration(cfg.ConnectTimeout, DefaultConnectTimeout)*2)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
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
