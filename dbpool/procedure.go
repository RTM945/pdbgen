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
// 单 bigint 和双 int32 是 PostgreSQL 里两个互不重叠的 key 空间，key 的含义由调用方决定；
// pdb.xml 里声明了 lock 的表，行的修改要持有 bigint 形式的那把锁（docs/design.md 2.8）。
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

// bigint 返回单 bigint 形式的锁的 key；双 int32 形式和不加锁时 ok 为 false。
func (l LockSpec) bigint() (key int64, ok bool) {
	if len(l.args) != 1 {
		return 0, false
	}
	return l.args[0].(int64), true
}

// ProcedureOptions 描述一次 Procedure 的执行参数。
type ProcedureOptions struct {
	// Lock 为零值时不加锁。
	Lock LockSpec

	// Timeout 是这次 Procedure 的额外预算，<= 0 表示只用 Config.TransactionTimeout；
	// 两者都设置时取先到期的那个。COMMIT 不受预算影响（见 Config.CommitTimeout）。
	Timeout time.Duration
}

// WithTx 开一个不加锁的事务，等价于 Procedure(ctx, ProcedureOptions{}, fn)。
func WithTx(ctx context.Context, fn func(context.Context) error) error {
	return Procedure(ctx, ProcedureOptions{}, fn)
}

// Procedure 是事务的唯一入口：BEGIN（带超时）→ 咨询锁 → fn → flush → COMMIT。
//
//   - fn 返回错误、fn 里的 SQL 出错（生成代码的查询、Insert）、flush 失败时回滚，把错误返回给调用方；
//     fn panic 时回滚后原样重抛；fn 调用 runtime.Goexit（测试里的 t.Fatal）也回滚。
//     回滚时内存对象恢复到进事务时的样子，再执行 AfterRollback 钩子。COMMIT 成功后执行 AfterCommit 钩子。
//     fn 只执行一次，不自动重试。
//   - 超过预算返回 ErrTimeout；咨询锁没拿到返回 ErrAdvisoryLockNotAcquired；
//     COMMIT 等不到结果返回 ErrCommitUnknown（这时不回滚内存，也不执行钩子）。
//   - 不能嵌套：ctx 里已经有事务时返回 ErrNestedTransaction。钩子拿到的 ctx 不带事务，可以开新事务。
//   - 结束后（提交、回滚、提交结果不确定）事务里的实体全部脱离事务，再改会 panic。
func Procedure(ctx context.Context, opts ProcedureOptions, fn func(context.Context) error) error {
	if dbctx.InTransaction(ctx) {
		return ErrNestedTransaction
	}
	d := active.Load()
	if d == nil {
		return ErrNotOpen
	}
	return d.procedure(ctx, opts, fn)
}

func (d *db) procedure(ctx context.Context, opts ProcedureOptions, fn func(context.Context) error) (err error) {
	if timeout := earliest(d.budget, opts.Timeout); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, timeout, ErrTimeout)
		defer cancel()
	}
	// 钩子在事务之外执行：保留调用方的值，去掉取消和超时——事务预算不应该把提交后的发包打死。
	hookCtx := context.WithoutCancel(ctx)

	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{BeginQuery: d.beginSQL})
	if err != nil {
		return d.failed(ctx, err)
	}
	txn := newTransaction(d.wrap(tx))
	txn.logSQL = d.showSQL
	// 加锁在 fn 之前：fn 里登记的行，都是持有这把锁之后读出来的
	txn.lockKey, txn.locked = opts.Lock.bigint()
	ctx = dbctx.WithTransaction(ctx, txn)

	ended := false
	defer func() {
		if ended {
			return
		}
		// 没有走到结尾：fn 里 SQL 出错（abort）、fn panic，或者调用了 runtime.Goexit。
		// 一律回滚：半截的修改不能提交，连接也必须还回池里。
		r := recover()
		d.rollback(hookCtx, tx, txn)
		switch aerr := dbctx.AbortError(r); {
		case aerr != nil:
			err = d.failed(ctx, aerr)
		case r != nil:
			d.stats.panicked.Add(1)
			panic(r)
		default:
			d.stats.failed.Add(1) // runtime.Goexit：goroutine 正在退出，没有人接收返回值
		}
	}()

	if err = opts.Lock.acquire(ctx, txn.SQLTx()); err == nil {
		if err = fn(ctx); err == nil {
			err = txn.flush(ctx)
		}
	}
	if err != nil {
		ended = true
		d.rollback(hookCtx, tx, txn)
		return d.failed(ctx, err)
	}

	err = d.commit(ctx, tx)
	ended = true
	switch {
	case err == nil:
		txn.end()
		d.stats.committed.Add(1)
		runHooks(hookCtx, "after-commit", txn.afterCommit)
		return nil
	case errors.Is(err, pgx.ErrTxCommitRollback):
		// 事务之前已经出错（比如业务自己执行的 SQL 失败了却没有返回错误），PostgreSQL 把 COMMIT
		// 当成了 ROLLBACK：结果确定，按回滚处理。
		d.rollback(hookCtx, tx, txn)
		return d.failed(ctx, err)
	default:
		// 结果不确定：可能提交了也可能没有。既不回滚内存，也不跑任何钩子——发成功或失败通知都可能是错的。
		txn.end()
		d.stats.unknown.Add(1)
		return fmt.Errorf("%w: %w", ErrCommitUnknown, err)
	}
}

// commit 发 COMMIT 并等结果。COMMIT 不受预算限制：一旦发出去就要等到结果——ctx 到期时 pgx 会断开连接，
// 而 COMMIT 可能已经在 PG 上执行完了，客户端就不知道到底提交没有，重试会重复发放。
// 但也不能无限等：网络半断时 pgx 会一直等到 TCP 超时。超过 commitTimeout 按结果不确定处理。
func (d *db) commit(ctx context.Context, tx pgx.Tx) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.commitTimeout)
	defer cancel()
	return tx.Commit(ctx)
}

// rollback 恢复内存对象，回滚 SQL 事务，再执行 AfterRollback 钩子。
func (d *db) rollback(hookCtx context.Context, tx pgx.Tx, txn *transaction) {
	txn.rollback()
	ctx, cancel := context.WithTimeout(hookCtx, d.commitTimeout)
	defer cancel()
	_ = tx.Rollback(ctx) // 连接断了或者等超时也没关系：连接会被关掉，服务端随之回滚
	runHooks(hookCtx, "after-rollback", txn.afterRollback)
}

// failed 记一次没有提交的 Procedure，返回交给调用方的错误：预算到期造成的带上 ErrTimeout。
func (d *db) failed(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, ErrAdvisoryLockNotAcquired):
		d.stats.busy.Add(1)
	case overBudget(ctx, err):
		d.stats.timedOut.Add(1)
		return fmt.Errorf("%w: %w", ErrTimeout, err)
	default:
		d.stats.failed.Add(1)
	}
	return err
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
