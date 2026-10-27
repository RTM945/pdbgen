package dbpool

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"pdbgen/dbctx"
	"pdbgen/ptable"
)

// 这个文件验证框架把约定写成了约束：不支持的写法当场报错，而不是静默出错。

// TestNestedProcedureRejected 事务里再开事务直接返回 ErrNestedTransaction（而不是另拿一条连接、
// 自己等自己的锁），内层不执行，外层不受影响。事务里调用 dbpool.Query 同样报错。
func TestNestedProcedureRejected(t *testing.T) {
	setupTestDB(t)

	key := time.Now().UnixNano()
	var innerRan bool
	err := Procedure(t.Context(), ProcedureOptions{Lock: AdvisoryLock(key)}, func(ctx context.Context) error {
		for _, opts := range []ProcedureOptions{{}, {Lock: TryAdvisoryLock(key)}} {
			err := Procedure(ctx, opts, func(context.Context) error {
				innerRan = true
				return nil
			})
			if !errors.Is(err, ErrNestedTransaction) {
				t.Errorf("嵌套事务应返回 ErrNestedTransaction，实际: %v", err)
			}
		}
		if err := Query(ctx, func(context.Context) error { return nil }); err == nil {
			t.Error("事务里调用 dbpool.Query 应该报错")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("外层事务失败: %v", err)
	}
	if innerRan {
		t.Fatal("被拒绝的内层事务不应该执行")
	}
}

// TestProcedureInsideAfterCommitHook 提交后钩子的 ctx 不带事务，可以开新事务。
func TestProcedureInsideAfterCommitHook(t *testing.T) {
	setupTestDB(t)

	var hookErr error
	var innerRan bool
	if err := WithTx(t.Context(), func(ctx context.Context) error {
		dbctx.AfterCommit(ctx, func(ctx context.Context) {
			hookErr = WithTx(ctx, func(context.Context) error {
				innerRan = true
				return nil
			})
		})
		return nil
	}); err != nil {
		t.Fatalf("事务失败: %v", err)
	}
	if hookErr != nil || !innerRan {
		t.Fatalf("钩子里应该可以开新事务，实际 err=%v ran=%v", hookErr, innerRan)
	}
}

// TestSelectedRecordIsDetached 事务外查出来的实体可以读，改它直接 panic——改了也不会落库。
func TestSelectedRecordIsDetached(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uid := time.Now().UnixNano()
	const actId int32 = 40
	defer cleanupRedEnvelope(t, ctx, uid)
	seedRedEnvelope(t, ctx, uid, actId, 2, 3)

	var v *ptable.RedEnvelope
	if err := Query(ctx, func(ctx context.Context) error {
		v = ptable.UserRedEnvelopeTable.SelectByUidActId(ctx, uid, actId)
		return nil
	}); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if v == nil || v.TodayCount() != 2 {
		t.Fatalf("事务外查出来的实体应该可以读，实际 %v", v)
	}
	if msg := catchPanic(t, func() { v.SetTodayCount(5) }); !strings.Contains(msg, "已脱离事务") {
		t.Fatalf("改事务外查出来的实体应该 panic，实际: %q", msg)
	}
}

// TestRecordDetachedAfterTransaction 事务结束后（提交或回滚）实体脱离事务，再改直接 panic。
func TestRecordDetachedAfterTransaction(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uid := time.Now().UnixNano() + 1
	const actId int32 = 41
	defer cleanupRedEnvelope(t, ctx, uid)
	seedRedEnvelope(t, ctx, uid, actId, 2, 3)

	for _, fail := range []bool{false, true} {
		var v *ptable.RedEnvelope
		_ = WithTx(ctx, func(ctx context.Context) error {
			v = ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid, actId)
			v.SetTotal(v.Total() + 1)
			if fail {
				return errors.New("business failed")
			}
			return nil
		})
		if msg := catchPanic(t, func() { v.SetTotal(100) }); !strings.Contains(msg, "已脱离事务") {
			t.Fatalf("fail=%v: 事务结束后改实体应该 panic，实际: %q", fail, msg)
		}
	}
}

// TestRolledBackInsertCanBeRetried Insert 回滚后对象回到插入前，还是一个新对象：
// 可以继续改，也可以在新事务里重新插入。
func TestRolledBackInsertCanBeRetried(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uid := time.Now().UnixNano() + 2
	const actId int32 = 42
	defer cleanupRedEnvelope(t, ctx, uid)

	v := ptable.NewRedEnvelope()
	v.SetUid(uid)
	v.SetActId(actId)
	_ = WithTx(ctx, func(ctx context.Context) error {
		ptable.UserRedEnvelopeTable.Insert(ctx, v)
		return errors.New("business failed")
	})
	if v.Id() != 0 {
		t.Fatalf("Insert 回滚后主键应清零，实际 %d", v.Id())
	}

	v.SetTodayCount(7)
	if err := WithTx(ctx, func(ctx context.Context) error {
		ptable.UserRedEnvelopeTable.Insert(ctx, v)
		return nil
	}); err != nil {
		t.Fatalf("重新插入失败: %v", err)
	}
	if n, err := countRedEnvelope(ctx, uid); err != nil || n != 1 {
		t.Fatalf("应该插入 1 行，实际 n=%d err=%v", n, err)
	}
}

// TestQueryByModifiedColumnPanics 事务里改了查询条件用到的列、还没落库，再按这些列查同一张表
// 直接 panic（SELECT 按库里的旧值过滤会多行或漏行）。改别的列不受影响，按主键查走缓存也不受影响。
func TestQueryByModifiedColumnPanics(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uid := time.Now().UnixNano() + 3
	defer cleanupRedEnvelope(t, ctx, uid)
	seedRedEnvelope(t, ctx, uid, 50, 1, 0)
	seedRedEnvelope(t, ctx, uid, 51, 1, 0)

	var msg string
	_ = WithTx(ctx, func(ctx context.Context) error {
		a := ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid, 50)
		a.SetTodayCount(9) // 不在查询条件里的列
		if ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid, 51) == nil {
			t.Error("改了非条件列之后应该可以照常查询")
		}
		if ptable.UserRedEnvelopeTable.GetById(ctx, a.Id()) != a {
			t.Error("按主键查应该命中事务缓存")
		}

		a.SetActId(52) // 查询条件里的列
		msg = catchPanic(t, func() { ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid, 52) })
		return errors.New("rollback")
	})
	if !strings.Contains(msg, "改了查询条件用到的列") {
		t.Fatalf("按改过的条件列查询应该 panic，实际: %q", msg)
	}
}

// TestDeleteThenInsertSameUniqueKeyFails 约定：同一事务里删了再插同一个唯一键会撞唯一索引
// （DELETE 在提交前才发，INSERT 立即执行）。数据库直接报错，整个事务回滚，不会静默出错。
func TestDeleteThenInsertSameUniqueKeyFails(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uid := time.Now().UnixNano() + 4
	const actId int32 = 53
	defer cleanupRedEnvelope(t, ctx, uid)
	seedRedEnvelope(t, ctx, uid, actId, 1, 0)

	msg := catchPanic(t, func() {
		_ = WithTx(ctx, func(ctx context.Context) error {
			ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid, actId).Delete()
			fresh := ptable.NewRedEnvelope()
			fresh.SetUid(uid)
			fresh.SetActId(actId)
			ptable.UserRedEnvelopeTable.Insert(ctx, fresh)
			return nil
		})
	})
	if !strings.Contains(msg, "23505") {
		t.Fatalf("应该撞唯一索引（SQLSTATE 23505），实际: %q", msg)
	}
	if n, err := countRedEnvelope(ctx, uid); err != nil || n != 1 {
		t.Fatalf("事务应该回滚、原来的行还在，实际 n=%d err=%v", n, err)
	}
}

// TestFlushMissingRowFails 提交前的 UPDATE 没命中任何行（这一行已经被删了）：
// 返回 ErrRowNotFound，整个事务回滚。
func TestFlushMissingRowFails(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uid := time.Now().UnixNano() + 5
	const actId int32 = 54
	defer cleanupRedEnvelope(t, ctx, uid)
	seedRedEnvelope(t, ctx, uid, actId, 1, 0)

	err := WithTx(ctx, func(ctx context.Context) error {
		v := ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid, actId)
		v.SetTotal(9)
		_, err := dbctx.SQLTxFromCtx(ctx).Exec(ctx, `DELETE FROM user_red_envelope WHERE id = $1`, v.Id())
		return err
	})
	if !errors.Is(err, ErrRowNotFound) {
		t.Fatalf("期望 ErrRowNotFound，实际: %v", err)
	}
	if n, err := countRedEnvelope(ctx, uid); err != nil || n != 1 {
		t.Fatalf("事务应该回滚、行还在，实际 n=%d err=%v", n, err)
	}
}
