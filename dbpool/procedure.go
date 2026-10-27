package dbpool

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"pdbgen/dbctx"
)

// LockSpec 描述 Procedure 在事务开头拿的咨询锁，零值表示不加锁。
// 单 bigint 和双 int32 是 PostgreSQL 里两个互不重叠的 key 空间，key 的含义由调用方决定。
type LockSpec struct {
	try  bool
	args []any // nil 表示不加锁
}

// TryAdvisoryLock 单 bigint 的 try 锁，拿不到返回 ErrAdvisoryLockNotAcquired。
func TryAdvisoryLock(key int64) LockSpec { return LockSpec{try: true, args: []any{key}} }

// AdvisoryLock 单 bigint 的阻塞锁，等待时间受 lock_timeout 限制；等超时也返回 ErrAdvisoryLockNotAcquired。
func AdvisoryLock(key int64) LockSpec { return LockSpec{args: []any{key}} }

// TryAdvisoryLock2 双 int32 的 try 锁。
func TryAdvisoryLock2(ns, id int32) LockSpec { return LockSpec{try: true, args: []any{ns, id}} }

// AdvisoryLock2 双 int32 的阻塞锁。
func AdvisoryLock2(ns, id int32) LockSpec { return LockSpec{args: []any{ns, id}} }

func (l LockSpec) acquire(ctx context.Context, tx dbctx.SQLTx) error {
	if l.args == nil {
		return nil
	}
	fn := "pg_advisory_xact_lock"
	if l.try {
		fn = "pg_try_advisory_xact_lock"
	}
	query := "SELECT " + fn + "($1)"
	if len(l.args) == 2 {
		query = "SELECT " + fn + "($1, $2)"
	}
	if !l.try {
		_, err := tx.Exec(ctx, query, l.args...)
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "55P03" {
			// 等到 lock_timeout 还没拿到：和 try 锁拿不到是同一个"忙"信号
			return fmt.Errorf("%w: %w", ErrAdvisoryLockNotAcquired, err)
		}
		return err
	}
	var locked bool
	if err := tx.QueryRow(ctx, query, l.args...).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return ErrAdvisoryLockNotAcquired
	}
	return nil
}

// ProcedureOptions 描述一次 Procedure 的执行参数。
type ProcedureOptions struct {
	// Lock 为零值时不加锁。
	Lock LockSpec

	// Timeout 是这次 Procedure 的额外预算，<= 0 表示只用 transactionTimeoutMs；
	// 两者都设置时取先到期的那个。COMMIT 不受预算影响。
	Timeout time.Duration
}

// WithTx 开一个不加锁的事务，等价于 Procedure(ctx, ProcedureOptions{}, fn)。
func WithTx(ctx context.Context, fn func(context.Context) error) error {
	return Procedure(ctx, ProcedureOptions{}, fn)
}

// Procedure 是事务的唯一入口：BEGIN（带超时）→ 咨询锁 → fn → flush → COMMIT。
//
//   - fn 返回错误或 panic 时回滚：内存对象恢复到进事务时的样子，再执行 AfterRollback 钩子，
//     panic 回滚后原样重抛。COMMIT 成功后执行 AfterCommit 钩子。fn 只执行一次，不自动重试。
//   - 不能嵌套：ctx 里已经有事务时返回 ErrNestedTransaction。钩子拿到的 ctx 不带事务，可以开新事务。
//   - 结束后（提交、回滚、提交结果不确定）事务里的实体全部脱离事务，再改会 panic。
func Procedure(ctx context.Context, opts ProcedureOptions, fn func(context.Context) error) (err error) {
	if dbctx.InTransaction(ctx) {
		return ErrNestedTransaction
	}
	if timeout := earliest(budget, opts.Timeout); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	// 钩子在事务之外执行：保留调用方的值，去掉取消和超时——事务预算不应该把提交后的发包打死。
	hookCtx := context.WithoutCancel(ctx)

	tx, err := dbpool.BeginTx(ctx, pgx.TxOptions{BeginQuery: beginSQL})
	if err != nil {
		return err
	}
	txn := newTransaction(wrapLogger(tx))
	ctx = dbctx.WithTransaction(ctx, txn)

	defer func() {
		r := recover()
		if r == nil && err == nil {
			if err = txn.flush(ctx); err == nil {
				// COMMIT 不受预算限制：一旦发出去就必须等到结果。ctx 到期时 pgx 会断开连接，
				// 而 COMMIT 可能已经在 PG 上执行完了——客户端不知道到底提交没有，重试就会重复发放。
				err = tx.Commit(context.WithoutCancel(ctx))
				if !errors.Is(err, pgx.ErrTxCommitRollback) {
					// 提交成功；或者结果不确定（可能提交了也可能没有），这时既不回滚内存，
					// 也不跑任何钩子——发成功或失败通知都可能是错的，交给调用方处理。
					txn.end()
					if err == nil {
						runHooks(hookCtx, "after-commit", txn.afterCommit)
					}
					return
				}
				// 事务之前已经出错，PostgreSQL 把 COMMIT 当成了 ROLLBACK：结果确定，按回滚处理。
			}
		}
		txn.rollback()
		_ = tx.Rollback(context.Background())
		runHooks(hookCtx, "after-rollback", txn.afterRollback)
		if r != nil {
			panic(r)
		}
	}()

	if err = opts.Lock.acquire(ctx, txn.SQLTx()); err != nil {
		return err
	}
	return fn(ctx)
}

// earliest 返回两个预算里先到期的那个，<= 0 表示不限。
func earliest(a, b time.Duration) time.Duration {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	}
	return min(a, b)
}
