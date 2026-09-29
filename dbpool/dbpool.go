package dbpool

import (
	"context"
	"errors"
	"fmt"
	"log"
	"pdbgen/dbctx"
	"pdbgen/readxml"
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
	Println(v ...any)
	Printf(format string, v ...any)
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

func Query(ctx context.Context, fn func(context.Context) error) error {
	// 不允许在事务中调用 Query。
	// 否则容易出现事务里已经修改数据，但 Query 却从
	// pool 的另一条 PostgreSQL connection 读取的问题。
	if dbctx.HasTx(ctx) {
		return errors.New("dbpool.Query cannot be called inside transaction")
	}

	ctx = dbctx.WithQuerier(ctx, wrapLogger(dbpool))

	return fn(ctx)
}

// 只要开事务必加咨询锁
//func WithTx(ctx context.Context, fn func(context.Context) error) error {
//	return withTx(ctx, nil, fn)
//}

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

	if prepare != nil {
		if err = prepare(ctx, tx); err != nil {
			return err
		}
	}

	return fn(ctx)
}

// WithTryAdvisoryLock 使用单 bigint 形式的咨询锁，拿不到锁直接返回。
// key 的含义由调用方决定，框架不关心。
func WithTryAdvisoryLock(ctx context.Context, key int64, fn func(context.Context) error) error {
	return withTx(
		ctx,
		func(ctx context.Context, tx pgx.Tx) error {
			return prepare(ctx, tx, true, lockKey(key))
		},
		fn,
	)
}

// WithTryAdvisoryLock2 使用双 int32 形式的咨询锁，拿不到锁直接返回。
// ns/id 的含义由调用方决定，框架不关心。
func WithTryAdvisoryLock2(ctx context.Context, ns, id int32, fn func(context.Context) error) error {
	return withTx(
		ctx,
		func(ctx context.Context, tx pgx.Tx) error {
			return prepare(ctx, tx, true, lockKey2{ns: ns, id: id})
		},
		fn,
	)
}

// WithAdvisoryLock 拿不到锁时会阻塞
func WithAdvisoryLock(ctx context.Context, key int64, fn func(context.Context) error) error {
	return withTx(
		ctx,
		func(ctx context.Context, tx pgx.Tx) error {
			return prepare(ctx, tx, false, lockKey(key))
		},
		fn,
	)
}

func WithAdvisoryLock2(ctx context.Context, ns, id int32, fn func(context.Context) error) error {
	return withTx(
		ctx,
		func(ctx context.Context, tx pgx.Tx) error {
			return prepare(ctx, tx, false, lockKey2{ns: ns, id: id})
		},
		fn,
	)
}

type lockArgs interface {
	sql(fn string) (string, []any)
}

type lockKey int64 // 单 bigint 形式

func (k lockKey) sql(fn string) (string, []any) {
	return fmt.Sprintf("SELECT %s($1)", fn), []any{int64(k)}
}

type lockKey2 struct{ ns, id int32 } // 双 int32 形式

func (k lockKey2) sql(fn string) (string, []any) {
	return fmt.Sprintf("SELECT %s($1, $2)", fn), []any{k.ns, k.id}
}

func prepare(ctx context.Context, tx pgx.Tx, tryLock bool, lock lockArgs) error {
	batch := &pgx.Batch{}

	if statementTimeoutMs > 0 {
		batch.Queue(fmt.Sprintf("SET LOCAL statement_timeout = %d", statementTimeoutMs))
	}

	if idleInTransactionSessionTimeoutMs > 0 {
		batch.Queue(fmt.Sprintf("SET LOCAL idle_in_transaction_session_timeout = %d", idleInTransactionSessionTimeoutMs))
	}

	if lockTimeoutMs > 0 {
		batch.Queue(fmt.Sprintf("SET LOCAL lock_timeout = %d", lockTimeoutMs))
	}

	fn := "pg_advisory_xact_lock"
	if tryLock {
		fn = "pg_try_advisory_xact_lock"
	}
	query, args := lock.sql(fn)
	batch.Queue(query, args...)

	if batch.Len() == 0 {
		return nil
	}

	start := time.Now()

	br := tx.SendBatch(ctx, batch)

	var err error
	for i := 0; i < batch.Len()-1; i++ {
		if _, err = br.Exec(); err != nil {
			break
		}
	}
	if err == nil {
		if tryLock {
			var locked bool

			err = br.QueryRow().Scan(&locked)

			if err == nil && !locked {
				err = ErrAdvisoryLockNotAcquired
			}
		} else {
			var rows pgx.Rows

			rows, err = br.Query()
			if err == nil {
				err = rows.Err()
				rows.Close()
			}
		}
	}
	closeErr := br.Close()
	if err == nil {
		err = closeErr
	}

	logBatch(start, batch, err)

	return err
}
