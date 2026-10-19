package dbpool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	transactionTimeoutMs              int
	showSQL                           bool
)

// SetLogger 换掉 SQL 日志的落点，不设就用 slog.Default()。
//
// nil 表示回落到 slog.Default()，而且是**调用时**才解析——所以应用在
// dbpool.Init 之后再 slog.SetDefault 也来得及。
// 想给 SQL 日志单独一个 handler（写去别的文件、单独控级别）就显式调这个。
func SetLogger(l *slog.Logger) { sqlLog = l }

// sqlLog 见 SetLogger。只由 SetLogger 写，读的时候统一走 log()。
var sqlLog *slog.Logger

func log() *slog.Logger {
	if sqlLog != nil {
		return sqlLog
	}
	return slog.Default()
}

var ErrAdvisoryLockNotAcquired = errors.New("advisory lock not acquired")

// Close 关掉连接池。进程退出前收摊用，重复调用安全。
//
// 调用时机有要求：必须排在 HTTP 服务优雅关闭**之后**。pgxpool 的 Close 会
// 阻塞到所有借出的连接归还，并且拒绝后续的 Acquire——在途请求还没跑完就调
// 它，等于把它们打死在半路。
//
// 收益不大（进程退出时 OS 本来就会关 socket，PG 一样会清理），主要是让
// PG 和 pg_bouncer 看到的是"客户端主动断开"而不是"连接消失"。
func Close() {
	if dbpool == nil {
		return
	}
	dbpool.Close()
}

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
	transactionTimeoutMs = pdb.TransactionTimeoutMs
	showSQL = pdb.ShowSQL
	return nil
}

func Query(ctx context.Context, fn func(context.Context) error) error {
	// 不允许在事务中调用 Query。
	// 否则容易出现事务里已经修改数据，但 Query 却从
	// pool 的另一条 PostgreSQL connection 读取的问题。
	if dbctx.HasSQLTx(ctx) {
		return errors.New("dbpool.Query cannot be called inside transaction")
	}

	ctx = dbctx.WithQuerier(ctx, wrapLogger(dbpool))

	return fn(ctx)
}

func WithTx(ctx context.Context, fn func(context.Context) error) error {
	return WithTxPrepare(ctx, nil, fn)
}

func WithTxPrepare(ctx context.Context, prepare func(context.Context, pgx.Tx) error, fn func(context.Context) error) (err error) {
	// 整个事务一个总预算。statement_timeout 只管单条语句，一堆语句加起来慢
	// 它拦不住；PG 17 之前也没有事务级的 GUC。
	if transactionTimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(transactionTimeoutMs)*time.Millisecond)
		defer cancel()
	}

	// hookCtx 在放入 SQLTx/Transaction 之前保存：钩子运行时已经不在事务里，
	// 不能让它看到事务上下文，否则 dbpool.Query 会误判"还在事务中"。
	hookCtx := ctx

	tx, err := dbpool.Begin(ctx)
	if err != nil {
		return err
	}

	txn := NewTransaction()
	db := wrapLogger(tx)
	ctx = dbctx.WithSQLTx(ctx, db)
	ctx = dbctx.WithTransaction(ctx, txn)

	defer func() {
		// 钩子保留调用方的值，但去掉取消和超时：事务总预算不应该把
		// 提交后的发包、事件打死。钩子需要超时就自己控制。
		runCtx := context.WithoutCancel(hookCtx)

		if r := recover(); r != nil {
			_ = tx.Rollback(context.Background())
			txn.runAfterRollback(runCtx)
			panic(r)
		}

		if err != nil {
			_ = tx.Rollback(context.Background())
			txn.runAfterRollback(runCtx)
			return
		}

		// 所有业务执行完成后，按注册顺序 flush dirty object。
		if err = txn.Flush(ctx); err != nil {
			_ = tx.Rollback(context.Background())
			txn.runAfterRollback(runCtx)
			return
		}

		// Commit 用 WithoutCancel：业务语句吃上面那个总预算，但 COMMIT 一旦
		// 发出去就必须等到结果。pgx 在 ctx 取消时会发 CancelRequest 并关连接，
		// 而 COMMIT 可能已经被 PG 执行完了——那时客户端就不知道到底提交没有，
		// 重试会导致重复发放。换掉 ctx 之后，"Commit 失败"只剩进程被杀、
		// TCP 断这类不可控来源。
		//
		// 注意这时候事务状态仍可能因为进程被杀而变成不确定的，所以照旧不依赖
		// 下面的 Rollback 兜底。
		// Commit 返回错误时结果不确定：可能已经提交，也可能没有。
		// 这时候两个钩子都不能跑——跑 after-commit 可能在未提交时发包，
		// 跑 after-rollback 可能在已提交时发失败通知。只把错误交给调用方。
		if err = tx.Commit(context.WithoutCancel(ctx)); err != nil {
			return
		}

		txn.runAfterCommit(runCtx)
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
	return WithTxPrepare(
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
	return WithTxPrepare(
		ctx,
		func(ctx context.Context, tx pgx.Tx) error {
			return prepare(ctx, tx, true, lockKey2{ns: ns, id: id})
		},
		fn,
	)
}

// WithAdvisoryLock 拿不到锁时会阻塞
func WithAdvisoryLock(ctx context.Context, key int64, fn func(context.Context) error) error {
	return WithTxPrepare(
		ctx,
		func(ctx context.Context, tx pgx.Tx) error {
			return prepare(ctx, tx, false, lockKey(key))
		},
		fn,
	)
}

func WithAdvisoryLock2(ctx context.Context, ns, id int32, fn func(context.Context) error) error {
	return WithTxPrepare(
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
