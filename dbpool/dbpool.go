package dbpool

import (
	"context"
	"errors"
	"fmt"
	"log"
	"pdbgen/dbctx"
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
	sqlLogger                         logger
	showSQL                           bool
)

type logger interface {
	Printf(format string, args ...any)
}

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
	showSQL = pdb.ShowSQL
	sqlLogger = log.Default()
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

func Query[T any](ctx context.Context, fn func(context.Context) (T, error)) (T, error) {
	// 不允许在事务中调用 Query。
	// 否则容易出现事务里已经修改数据，但 Query 却从
	// pool 的另一条 PostgreSQL connection 读取的问题。
	if dbctx.HasTx(ctx) {
		var zero T
		return zero, errors.New("dbpool.Query cannot be called inside transaction")
	}

	ctx = dbctx.WithQuerier(ctx, wrapLogger(dbpool))

	return fn(ctx)
}

func WithTx(ctx context.Context, fn func(context.Context) error) error {
	return withTx(ctx, nil, fn)
}

func withTx(ctx context.Context, prepare func(context.Context, pgx.Tx) error, fn func(context.Context) error) (err error) {
	tx, err := dbpool.Begin(ctx)
	if err != nil {
		return err
	}

	uow := NewUnitOfWork()
	db := wrapLogger(tx)
	ctx = dbctx.WithTx(ctx, db)
	ctx = dbctx.WithUnitOfWork(ctx, uow)

	defer func() {
		if r := recover(); r != nil {
			_ = tx.Rollback(context.Background())
			panic(r)
		}

		if err != nil {
			_ = tx.Rollback(context.Background())
			return
		}

		// 所有业务执行完成后，按注册顺序 flush dirty object。
		if err = uow.Flush(ctx); err != nil {
			_ = tx.Rollback(context.Background())
			return
		}

		// Commit 失败时事务状态可能已经不确定，
		// 不再依赖 Rollback。
		if err = tx.Commit(ctx); err != nil {
			return
		}
	}()

	if err = setLocalTimeout(ctx, tx); err != nil {
		return err
	}

	if prepare != nil {
		if err = prepare(ctx, tx); err != nil {
			return err
		}
	}

	return fn(ctx)
}

// WithTryAdvisoryLock 拿不到锁时会直接返回
func WithTryAdvisoryLock(ctx context.Context, lockKey string, lockValue int64, fn func(context.Context) error) error {
	return withAdvisoryLock(ctx, lockKey, lockValue, true, fn)
}

// WithAdvisoryLock 拿不到锁时会阻塞
func WithAdvisoryLock(ctx context.Context, lockKey string, lockValue int64, fn func(context.Context) error) error {
	return withAdvisoryLock(ctx, lockKey, lockValue, false, fn)
}

func withAdvisoryLock(ctx context.Context, lockKey string, lockValue int64, tryLock bool, fn func(context.Context) error) (err error) {
	var prepare func(ctx context.Context, tx pgx.Tx) error

	if tryLock {
		prepare = func(ctx context.Context, tx pgx.Tx) error {
			var locked bool

			err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock(hashtext($1), $2", lockKey, lockValue).Scan(&locked)
			if err != nil {
				panic(err)
			}

			if !locked {
				return ErrAdvisoryLockNotAcquired
			}
			return nil
		}
	} else {
		prepare = func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1), $2", lockKey, lockValue)
			if err != nil {
				panic(err)
			}
			return nil
		}
	}

	return withTx(ctx, prepare, fn)
}
