package dbpool

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"pdbgen/dbctx"
)

// UnitOfWork 收集一次事务内所有被修改过的对象，
// 在事务提交前统一生成 UPDATE 并合并成一个 Batch 一次性发送。
type UnitOfWork struct {
	items []dbctx.Flusher
	seen  map[dbctx.Flusher]struct{}
}

func NewUnitOfWork() *UnitOfWork {
	return &UnitOfWork{
		items: make([]dbctx.Flusher, 0, 16),
		seen:  make(map[dbctx.Flusher]struct{}),
	}
}

// Register 实现 dbctx.UnitOfWork 接口。
// items 保证 Flush 顺序 = 注册顺序；seen 只用来去重，
// 同一个对象（同一个指针）多次注册只生效一次。
func (u *UnitOfWork) Register(obj dbctx.Flusher) {
	if obj == nil {
		return
	}
	if _, exists := u.seen[obj]; exists {
		return
	}
	u.seen[obj] = struct{}{}
	u.items = append(u.items, obj)
}

// Flush 从 ctx 里取出本次事务的 DB（withTx 已经 WithTx 过），
// 把所有脏对象的 UPDATE 合并成一个 Batch 发送，
// 按注册顺序取回结果并回调 FlushDone。
// 任何一条失败都立即返回 err，withTx 的 defer 会据此 Rollback。
func (u *UnitOfWork) Flush(ctx context.Context) error {
	if len(u.items) == 0 {
		return nil
	}

	batch := &pgx.Batch{}
	pending := make([]dbctx.Flusher, 0, len(u.items))
	for _, obj := range u.items {
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

	db := dbctx.TxFromCtx(ctx)
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
