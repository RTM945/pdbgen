package dbpool

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"pdbgen/dbctx"
)

// recordKey 是一条缓存记录的身份：表名 + 主键。
type recordKey struct {
	table string
	id    int64
}

// transaction 实现 dbctx.Transaction：一次 Procedure 的工作集。
type transaction struct {
	tx     dbctx.SQLTx
	logSQL bool // Config.ShowSQL：flush 的 Batch 记一条日志

	// 持有的 bigint 咨询锁（ProcedureOptions.Lock）：声明了 lock 的表，只有这把锁保护的行可以改
	lockKey int64
	locked  bool

	items         []dbctx.Flusher                     // 登记顺序，也是 flush 顺序
	snapshots     map[dbctx.Flusher]dbctx.RecordState // 第一次登记时的整条快照，回滚用
	cachedRecords map[recordKey]dbctx.Flusher         // 身份表：同一行在事务里只有一个对象

	afterCommit   []func(context.Context)
	afterRollback []func(context.Context)
}

var _ dbctx.Transaction = (*transaction)(nil)

func newTransaction(tx dbctx.SQLTx) *transaction {
	return &transaction{
		tx:            tx,
		snapshots:     make(map[dbctx.Flusher]dbctx.RecordState),
		cachedRecords: make(map[recordKey]dbctx.Flusher),
	}
}

func (t *transaction) SQLTx() dbctx.SQLTx { return t.tx }

func (t *transaction) GetCachedRecord(table string, id int64) (dbctx.Flusher, bool) {
	rec, ok := t.cachedRecords[recordKey{table, id}]
	return rec, ok
}

// AddCachedRecord 见 dbctx.Transaction。第一次见到的对象拍快照、进 flush 列表。
// Insert 会登记两次：主键回填前（只拍快照，回滚时恢复成插入前的样子）和回填后（进身份表）。
func (t *transaction) AddCachedRecord(obj dbctx.Flusher) dbctx.Flusher {
	if id := obj.Identity(); id != 0 {
		key := recordKey{obj.TableName(), id}
		if rec, ok := t.cachedRecords[key]; ok {
			return rec
		}
		t.cachedRecords[key] = obj
	}
	if _, ok := t.snapshots[obj]; !ok {
		t.snapshots[obj] = obj.SnapshotState()
		t.items = append(t.items, obj)
		// 第一次登记、主键不是 0：从库里查出来的行（Insert 的对象第一次登记时主键还是 0，
		// 它是本事务新建的行，没有别的写者）。归一把锁保护、本事务又没持有那把锁的，只读。
		if o, ok := obj.(dbctx.Owned); ok && obj.Identity() != 0 {
			if _, key := o.LockKey(); !t.holds(key) {
				o.MarkForeign()
			}
		}
	}
	return obj
}

// holds 报告事务是否持有 key 这把 bigint 咨询锁。
func (t *transaction) holds(key int64) bool {
	return t.locked && t.lockKey == key
}

func (t *transaction) Records() []dbctx.Flusher { return t.items }

func (t *transaction) AfterCommit(fn func(context.Context)) {
	t.afterCommit = append(t.afterCommit, fn)
}

func (t *transaction) AfterRollback(fn func(context.Context)) {
	t.afterRollback = append(t.afterRollback, fn)
}

// flush 把所有待写入的记录合并成一个 Batch 一次往返发出，按登记顺序回调 FlushDone。
func (t *transaction) flush(ctx context.Context) error {
	batch := &pgx.Batch{}
	var pending []dbctx.Flusher
	for _, obj := range t.items {
		if query, args := obj.FlushStmt(); query != "" {
			batch.Queue(query, args...)
			pending = append(pending, obj)
		}
	}
	if len(pending) == 0 {
		return nil
	}

	start := time.Now()
	br := t.tx.SendBatch(ctx, batch)
	var err error
	for _, obj := range pending {
		var tag pgconn.CommandTag
		if tag, err = br.Exec(); err != nil {
			break
		}
		if tag.RowsAffected() == 0 {
			err = fmt.Errorf("%w: %s id=%d", ErrRowNotFound, obj.TableName(), obj.Identity())
			break
		}
		obj.FlushDone()
	}
	if closeErr := br.Close(); err == nil {
		err = closeErr
	}
	if t.logSQL {
		logBatch(ctx, start, batch, err)
	}
	return err
}

// rollback 把登记过的记录恢复成进事务时的快照（Insert 过的对象回到插入前），然后结束工作集。
func (t *transaction) rollback() {
	for obj, state := range t.snapshots {
		obj.RestoreState(state)
	}
	t.end()
}

// end 结束工作集：登记过的记录全部脱离事务（之后再改会 panic），并释放对它们的引用。
func (t *transaction) end() {
	for _, obj := range t.items {
		obj.Detach()
	}
	t.items, t.snapshots, t.cachedRecords = nil, nil, nil
}

// runHooks 按注册顺序执行钩子。单个钩子 panic 只记日志，不影响后续钩子，
// 也不会把已经提交的事务变成失败。
func runHooks(ctx context.Context, kind string, hooks []func(context.Context)) {
	for i, fn := range hooks {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log().ErrorContext(ctx, "pdbgen: "+kind+" hook panic", "index", i, "panic", r, "stack", string(debug.Stack()))
				}
			}()
			fn(ctx)
		}()
	}
}
