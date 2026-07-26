package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func WithTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) (err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	// A panic inside fn used to unwind straight past the rollback, leaving the
	// transaction open and its connection checked out of the pool until the
	// context died — a handful of panicking requests could exhaust the pool.
	// Roll back on the way out, then re-panic so RecoveryMiddleware still sees
	// it. Rollback on an already-committed tx returns ErrTxClosed, which is
	// why this is not a blanket defer.
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
	}()

	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(ctx); rbErr != nil {
			return fmt.Errorf("rollback failed: %v (original: %w)", rbErr, err)
		}
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

// IsLockTimeout reports whether err is Postgres refusing to keep waiting for a
// row lock (SQLSTATE 55P03, lock_not_available).
//
// It exists so a route can tell "another transaction holds this row" apart from
// its own failures. That distinction has a user-visible consequence: the
// workflow PATCH route reported EVERY save failure as `400 INVALID_WORKFLOW`
// with the raw error text, so two admins editing one workflow gave the second
// "your workflow is invalid" plus a SQLSTATE on the wire — for a condition that
// is transient, nobody's mistake, and fixed by trying again.
//
// 55P03 only ever appears because lock_timeout is set (pkg/database sets it
// from DB_LOCK_TIMEOUT); without it the statement waits forever instead.
func IsLockTimeout(err error) bool {
	var pgErr *pgconn.PgError
	// The literal rather than pgerrcode.LockNotAvailable: that package is not a
	// dependency here and one constant does not justify making it one.
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
}

// IsCallerData reports whether err is Postgres refusing a VALUE rather than
// failing at its job: a violated CHECK constraint (23514), or text the encoding
// cannot represent (22021, 22P05 — in practice U+0000, which is legal JSON and
// which no Postgres text or jsonb column can store).
//
// It exists for routes whose every written value comes from the request body.
// There, these codes mean the caller sent something the schema does not allow,
// and reporting them as 500 both misleads the user and pages whoever owns the
// error-rate alert. Do NOT use it where the process supplies part of the row —
// a violated constraint on OUR value is a bug, and 400 would bury it.
func IsCallerData(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "23514", // check_violation
		"22021", // character_not_in_repertoire
		"22P05": // untranslatable_character
		return true
	}
	return false
}
