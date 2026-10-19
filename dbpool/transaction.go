package dbpool

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"pdbgen/dbctx"
)

// rowKey 是一行的身份：表名 + 主键。
type rowKey struct {
	table string
	id    int64
}

// Transaction 是一次业务事务的工作集，
// 收集本次事务内所有被修改过的对象，
// 在事务提交前统一生成 UPDATE 并合并成一个 Batch 一次性发送。
type Transaction struct {
	items []dbctx.Flusher
	seen  map[dbctx.Flusher]struct{}
	rows  map[rowKey]dbctx.Flusher
}

func NewTransaction() *Transaction {
	return &Transaction{
		items: make([]dbctx.Flusher, 0, 16),
		seen:  make(map[dbctx.Flusher]struct{}),
		rows:  make(map[rowKey]dbctx.Flusher),
	}
}

// Register 实现 dbctx.Transaction 接口。
// items 保证 Flush 顺序 = 注册顺序；seen 去重同一个对象（同一个指针）。
//
// rows 是防止静默丢数据的守卫：同一个事务里如果登记了两个对象指向同一行，
// 提交前会发出两条 UPDATE，各自只写自己的脏列，而写的是绝对值——后执行的那条
// 会把先执行的那条的结果覆盖掉，没有任何报错。这种情况几乎只会来自"同一行查了
// 两次"，所以这里直接 panic：请求失败、事务回滚，而不是悄悄写坏数据。
func (t *Transaction) Register(obj dbctx.Flusher) {
	if obj == nil {
		return
	}
	if _, exists := t.seen[obj]; exists {
		return
	}
	t.seen[obj] = struct{}{}
	t.items = append(t.items, obj)

	// 主键为 0（自增未回填、还没 Insert）的对象没有身份，不参与检测。
	id := obj.Identity()
	if id == 0 {
		return
	}

	key := rowKey{table: obj.TableName(), id: id}

	if existing, ok := t.rows[key]; ok {
		panic(fmt.Sprintf(
			"pdbgen: %s 表 id=%d 在同一个事务里有两个对象（%p 和 %p）。"+
				"提交前会发出两条 UPDATE，后写的会静默覆盖先写的。 "+
				"检查是不是对这一行查了两次——同一个请求里应该复用第一次查出来的对象。",
			key.table, key.id, existing, obj,
		))
	}

	t.rows[key] = obj
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
