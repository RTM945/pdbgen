package dbpool

import (
	"context"
	"errors"
	"fmt"
	"pdbgen/ptable"
	"pdbgen/readxml"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	dbpool *pgxpool.Pool

	statementTimeoutMs                int
	idleInTransactionSessionTimeoutMs int
	lockTimeoutMs                     int
)

var ErrAdvisoryLockNotAcquired = errors.New("advisory lock not acquired")

func Init(ctx context.Context, pdb *readxml.Schema) error {
	cfg, err := pgxpool.ParseConfig(pdb.URL)
	if err != nil {
		return err
	}
	cfg.MaxConns = pdb.PoolMaxConns
	cfg.MinConns = pdb.PoolMinConns
	cfg.MaxConnLifetime = time.Duration(pdb.PoolMaxConnLifetime) * time.Second
	cfg.MaxConnIdleTime = time.Duration(pdb.PoolMaxConnIdleTime) * time.Second
	cfg.HealthCheckPeriod = time.Duration(pdb.PoolHealthCheckPeriod) * time.Second
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	if pdb.AppName != "" {
		cfg.ConnConfig.RuntimeParams["application_name"] = pdb.AppName
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	dbpool = pool
	statementTimeoutMs = pdb.StatementTimeoutMs
	idleInTransactionSessionTimeoutMs = pdb.IdleInTransactionSessionTimeoutMs
	lockTimeoutMs = pdb.LockTimeoutMs
	return nil
}

func setLocalTimeout(ctx context.Context, tx pgx.Tx) error {
	if statementTimeoutMs > 0 {
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = "+strconv.Itoa(statementTimeoutMs)); err != nil {
			return fmt.Errorf("set statement_timeout: %w", err)
		}
	}

	if idleInTransactionSessionTimeoutMs > 0 {
		if _, err := tx.Exec(ctx, "SET LOCAL idle_in_transaction_session_timeout = "+strconv.Itoa(idleInTransactionSessionTimeoutMs)); err != nil {
			return fmt.Errorf("set idle_in_transaction_session_timeout: %w", err)
		}
	}

	if lockTimeoutMs > 0 {
		if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = "+strconv.Itoa(lockTimeoutMs)); err != nil {
			return fmt.Errorf("set lock_timeout: %w", err)
		}
	}

	return nil
}

func withAdvisoryLock(ctx context.Context, f func(ctx context.Context) error) error {
	tx, err := dbpool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if r := recover(); r != nil {
			_ = tx.Rollback(context.Background())
			panic(r)
		}

		if err != nil {
			_ = tx.Rollback(context.Background())
			return
		}

		if commitErr := tx.Commit(ctx); commitErr != nil {
			err = fmt.Errorf("commit transaction: %w", commitErr)
		}
	}()
	if err := setLocalTimeout(ctx, tx); err != nil {
		_ = tx.Rollback(context.Background())
		return err
	}

	if tryLock {
		var locked bool

		err := tx.QueryRow(
			ctx,
			"SELECT pg_try_advisory_xact_lock($1)",
			key,
		).Scan(&locked)
		if err != nil {
			panic(err)
		}

		if !locked {
			return ErrAdvisoryLockNotAcquired
		}
	} else {
		_, err := tx.Exec(
			ctx,
			"SELECT pg_advisory_xact_lock($1)",
			key,
		)
		if err != nil {
			panic(err)
		}
	}

	ctx = ptable.WithTx(ctx, tx)

	return fn(ctx)
}
