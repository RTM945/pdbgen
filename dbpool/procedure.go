package dbpool

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type lockMode uint8

const (
	lockNone lockMode = iota
	lockTry
	lockBlock
	lockTry2
	lockBlock2
)

// LockSpec 描述 Procedure 使用的咨询锁。
// 零值表示不加锁；具体值用 TryAdvisoryLock / AdvisoryLock /
// TryAdvisoryLock2 / AdvisoryLock2 构造。
type LockSpec struct {
	mode lockMode
	key  int64
	ns   int32
	id   int32
}

// TryAdvisoryLock 返回单 bigint 的 try 锁；拿不到返回 ErrAdvisoryLockNotAcquired。
func TryAdvisoryLock(key int64) LockSpec {
	return LockSpec{mode: lockTry, key: key}
}

// AdvisoryLock 返回单 bigint 的阻塞锁。
func AdvisoryLock(key int64) LockSpec {
	return LockSpec{mode: lockBlock, key: key}
}

// TryAdvisoryLock2 返回双 int32 的 try 锁。
func TryAdvisoryLock2(ns, id int32) LockSpec {
	return LockSpec{mode: lockTry2, ns: ns, id: id}
}

// AdvisoryLock2 返回双 int32 的阻塞锁。
func AdvisoryLock2(ns, id int32) LockSpec {
	return LockSpec{mode: lockBlock2, ns: ns, id: id}
}

// prepare 把 LockSpec 翻译成 WithTxPrepare 的 prepare 钩子。
func (l LockSpec) prepare() func(context.Context, pgx.Tx) error {
	switch l.mode {
	case lockNone:
		return nil
	case lockTry:
		return func(ctx context.Context, tx pgx.Tx) error {
			return prepare(ctx, tx, true, lockKey(l.key))
		}
	case lockBlock:
		return func(ctx context.Context, tx pgx.Tx) error {
			return prepare(ctx, tx, false, lockKey(l.key))
		}
	case lockTry2:
		return func(ctx context.Context, tx pgx.Tx) error {
			return prepare(ctx, tx, true, lockKey2{ns: l.ns, id: l.id})
		}
	case lockBlock2:
		return func(ctx context.Context, tx pgx.Tx) error {
			return prepare(ctx, tx, false, lockKey2{ns: l.ns, id: l.id})
		}
	default:
		panic("pdbgen: invalid lock mode")
	}
}

// ProcedureOptions 描述一次 Procedure 的执行参数。
type ProcedureOptions struct {
	// Lock 为零值时不开锁，等价于 WithTx。
	Lock LockSpec

	// Timeout 是这次 Procedure 的额外时间预算。
	// <= 0 表示只使用 pdb.xml 里的 transactionTimeoutMs；两者同时生效时
	// 取更早到期的那个。COMMIT 仍然用 WithoutCancel，不受这个预算影响。
	Timeout time.Duration
}

// Procedure 是事务的统一入口：Begin → 可选咨询锁 → fn → flush → commit。
// 外部副作用（发包、RPC、事件）仍然通过 dbctx.AfterCommit / AfterRollback 注册。
//
// Phase 4 不做重试：fn 只执行一次。重试需要内存回滚（memento），等 Phase 5 再加。
// WithTx / WithTxPrepare / With*AdvisoryLock* 保留为薄封装。
func Procedure(ctx context.Context, opts ProcedureOptions, fn func(context.Context) error) error {
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	return WithTxPrepare(ctx, opts.Lock.prepare(), fn)
}
