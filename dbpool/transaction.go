package dbpool

import (
	"context"
	"log/slog"
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

// Transaction 是一次业务事务的工作集，
// 缓存本次事务内被查出来的记录，收集被修改过的对象，
// 在事务提交前统一生成 UPDATE 并合并成一个 Batch 一次性发送。
type Transaction struct {
	items         []dbctx.Flusher
	seen          map[dbctx.Flusher]struct{}
	cachedRecords map[recordKey]dbctx.Flusher

	// snapshots 是每个登记对象第一次进入事务时的整条快照。
	// newObjects 记录本次事务里 Insert 过的对象，回滚时用 ResetNew 而不是恢复快照。
	snapshots  map[dbctx.Flusher]dbctx.RecordState
	newObjects map[dbctx.Flusher]struct{}

	afterCommit   []func(context.Context)
	afterRollback []func(context.Context)
}

func NewTransaction() *Transaction {
	return &Transaction{
		items:         make([]dbctx.Flusher, 0, 16),
		seen:          make(map[dbctx.Flusher]struct{}),
		cachedRecords: make(map[recordKey]dbctx.Flusher),
		snapshots:     make(map[dbctx.Flusher]dbctx.RecordState),
		newObjects:    make(map[dbctx.Flusher]struct{}),
	}
}

// GetCachedRecord 实现 dbctx.Transaction（xdb: getCachedTRecord）。
func (t *Transaction) GetCachedRecord(tableName string, id int64) (dbctx.Flusher, bool) {
	if id == 0 {
		return nil, false
	}
	rec, ok := t.cachedRecords[recordKey{table: tableName, id: id}]
	return rec, ok
}

// AddCachedRecord 实现 dbctx.Transaction（xdb: addCachedTRecord）。
// 同一行已经有记录时返回已有记录；同一个指针重复登记不会重复进 flush 列表。
func (t *Transaction) AddCachedRecord(obj dbctx.Flusher) dbctx.Flusher {
	if obj == nil {
		return nil
	}

	// 第一次看到这个对象时拍快照。命中已有记录时新对象会被丢弃，
	// 不会走到这里；同一个指针重复登记也不会覆盖第一次的快照。
	if _, ok := t.snapshots[obj]; !ok {
		t.snapshots[obj] = obj.SnapshotState()
	}

	// 先处理身份：Identity 可能在对象第一次登记后才回填（Insert RETURNING id），
	// 所以不能因为 seen 命中就跳过缓存登记。
	if id := obj.Identity(); id != 0 {
		key := recordKey{table: obj.TableName(), id: id}
		if existing, ok := t.cachedRecords[key]; ok {
			return existing
		}
		t.cachedRecords[key] = obj
	}

	if _, exists := t.seen[obj]; exists {
		return obj
	}
	t.seen[obj] = struct{}{}
	t.items = append(t.items, obj)
	return obj
}

// RegisterNew 实现 dbctx.Transaction。Insert 在发 SQL 前调用，
// 记录 Insert 之前的状态；回滚时用 ResetNew 让对象可以重新插入或被丢弃。
func (t *Transaction) RegisterNew(obj dbctx.Flusher) {
	if obj == nil {
		return
	}
	if _, ok := t.snapshots[obj]; !ok {
		t.snapshots[obj] = obj.SnapshotState()
	}
	t.newObjects[obj] = struct{}{}
}

// Rollback 实现 dbctx.Transaction：恢复本次事务内所有缓存记录的快照。
// Insert 过的对象不恢复快照，而是 ResetNew（清主键/loaded/dirty）并移出身份表。
func (t *Transaction) Rollback() {
	for obj, state := range t.snapshots {
		if _, isNew := t.newObjects[obj]; isNew {
			if id := obj.Identity(); id != 0 {
				delete(t.cachedRecords, recordKey{table: obj.TableName(), id: id})
			}
			obj.ResetNew()
			continue
		}
		obj.RestoreState(state)
	}

	t.snapshots = nil
	t.newObjects = nil
	t.cachedRecords = nil
	t.items = nil
	t.seen = nil
}

// Deprecated: use AddCachedRecord.
func (t *Transaction) Register(obj dbctx.Flusher) {
	t.AddCachedRecord(obj)
}

// AfterCommit 实现 dbctx.Transaction，按注册顺序保存提交后任务。
func (t *Transaction) AfterCommit(fn func(context.Context)) {
	if fn == nil {
		return
	}
	t.afterCommit = append(t.afterCommit, fn)
}

// AfterRollback 实现 dbctx.Transaction，按注册顺序保存回滚后任务。
func (t *Transaction) AfterRollback(fn func(context.Context)) {
	if fn == nil {
		return
	}
	t.afterRollback = append(t.afterRollback, fn)
}

func (t *Transaction) runAfterCommit(ctx context.Context) {
	hooks := t.afterCommit
	t.afterCommit = nil
	runHooks(ctx, "after-commit", hooks)
}

func (t *Transaction) runAfterRollback(ctx context.Context) {
	hooks := t.afterRollback
	t.afterRollback = nil
	runHooks(ctx, "after-rollback", hooks)
}

// runHooks 顺序执行钩子。单个钩子 panic 只记日志，不影响后续钩子，
// 也不会把已经提交的事务变成失败。
func runHooks(ctx context.Context, kind string, hooks []func(context.Context)) {
	for i, fn := range hooks {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("pdbgen: "+kind+" hook panic",
						"index", i,
						"panic", r,
						"stack", string(debug.Stack()),
					)
				}
			}()
			fn(ctx)
		}()
	}
}

// Flush 从 ctx 里取出本次事务的 SQLTx（WithTxPrepare 已经 WithSQLTx 过），
// 把所有脏对象的 UPDATE 合并成一个 Batch 发送，
// 按注册顺序取回结果并回调 FlushDone。
// 任何一条失败都立即返回 err，WithTxPrepare 的 defer 会据此 Rollback。
func (t *Transaction) Flush(ctx context.Context) error {
	if len(t.items) == 0 {
		return nil
	}

	batch := &pgx.Batch{}
	pending := make([]dbctx.Flusher, 0, len(t.items))
	for _, obj := range t.items {
		query, args := obj.FlushStmt()
		if query == "" {
			continue // 没被修改过，跳过，不占一次往返
		}
		batch.Queue(query, args...)
		pending = append(pending, obj)
	}
	if len(pending) == 0 {
		return nil
	}

	db := dbctx.SQLTxFromCtx(ctx)
	start := time.Now()
	br := db.SendBatch(ctx, batch)

	var err error
	for _, obj := range pending {
		var tag pgconn.CommandTag
		if tag, err = br.Exec(); err != nil {
			break
		}
		if err = obj.FlushDone(tag.RowsAffected()); err != nil {
			break
		}
	}

	closeErr := br.Close()
	if err == nil {
		err = closeErr
	}

	logBatch(start, batch, err)
	return err
}

// Deprecated: use Transaction.
type UnitOfWork = Transaction

// Deprecated: use NewTransaction.
func NewUnitOfWork() *Transaction {
	return NewTransaction()
}
