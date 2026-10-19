package dbpool

import (
	"context"
	"errors"
	"testing"
	"time"

	"pdbgen/dbctx"
	"pdbgen/ptable"
)

// TestRollbackRestoresLoadedRecord 业务返回错误后，事务里的对象必须回到加载时的状态，
// 不能把未提交的修改留在内存里。
func TestRollbackRestoresLoadedRecord(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uid := time.Now().UnixNano()
	const actId int32 = 20
	defer cleanupRedEnvelope(t, ctx, uid)

	seedRedEnvelope(t, ctx, uid, actId, 2, 3)

	var obj *ptable.RedEnvelope
	sentinel := errors.New("business failed")
	err := WithTx(ctx, func(ctx context.Context) error {
		obj = ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid, actId)
		if obj == nil {
			t.Fatal("查不到准备的数据")
		}
		obj.SetTodayCount(99)
		obj.SetTotal(100)
		return sentinel
	})

	if !errors.Is(err, sentinel) {
		t.Fatalf("期望业务错误原样返回，实际: %v", err)
	}
	if obj.TodayCount() != 2 || obj.Total() != 3 {
		t.Fatalf("回滚后内存对象应恢复，实际 %d/%d", obj.TodayCount(), obj.Total())
	}
	if stmt, _ := obj.FlushStmt(); stmt != "" {
		t.Fatalf("回滚后 dirty 应清零，FlushStmt 应返回空，实际: %s", stmt)
	}

	// 数据库也回滚了
	if err := WithTx(ctx, func(ctx context.Context) error {
		v := ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid, actId)
		if v.TodayCount() != 2 || v.Total() != 3 {
			t.Fatalf("数据库应回滚，实际 %d/%d", v.TodayCount(), v.Total())
		}
		return nil
	}); err != nil {
		t.Fatalf("校验数据库失败: %v", err)
	}
}

// TestRollbackRestoresOnPanic panic 路径同样先恢复内存对象，再把 panic 原样抛出。
func TestRollbackRestoresOnPanic(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uid := time.Now().UnixNano() + 1
	const actId int32 = 21
	defer cleanupRedEnvelope(t, ctx, uid)

	seedRedEnvelope(t, ctx, uid, actId, 2, 3)

	var obj *ptable.RedEnvelope
	msg := catchPanic(t, func() {
		_ = WithTx(ctx, func(ctx context.Context) error {
			obj = ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid, actId)
			if obj == nil {
				t.Fatal("查不到准备的数据")
			}
			obj.SetTodayCount(99)
			panic("boom")
		})
	})

	if msg != "boom" {
		t.Fatalf("原始 panic 应该原样抛出，实际: %q", msg)
	}
	if obj.TodayCount() != 2 {
		t.Fatalf("panic 回滚后内存对象应恢复，实际 %d", obj.TodayCount())
	}
}

// TestRollbackResetsInsertedRecord Insert 后业务失败：对象要回到未插入状态，
// 主键清零、loaded=false，可以被重新 Insert 或丢弃。
func TestRollbackResetsInsertedRecord(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uid := time.Now().UnixNano() + 2
	const actId int32 = 22
	defer cleanupRedEnvelope(t, ctx, uid)

	var obj *ptable.RedEnvelope
	sentinel := errors.New("business failed")
	err := WithTx(ctx, func(ctx context.Context) error {
		obj = ptable.NewRedEnvelope()
		obj.SetUid(uid)
		obj.SetActId(actId)
		obj.SetTodayCount(1)
		ptable.UserRedEnvelopeTable.Insert(ctx, obj)
		if obj.Id() == 0 {
			t.Fatal("Insert 应该回填主键")
		}
		return sentinel
	})

	if !errors.Is(err, sentinel) {
		t.Fatalf("期望业务错误原样返回，实际: %v", err)
	}
	if obj.Id() != 0 {
		t.Fatalf("Insert 回滚后 id 应清零，实际 %d", obj.Id())
	}
	if stmt, _ := obj.FlushStmt(); stmt != "" {
		t.Fatalf("Insert 回滚后 loaded=false，FlushStmt 应返回空，实际: %s", stmt)
	}
	if n, err := countRedEnvelope(ctx, uid); err != nil || n != 0 {
		t.Fatalf("数据库应该没有这行，实际 n=%d err=%v", n, err)
	}
}

// TestAfterRollbackHookSeesRestoredState 回滚钩子必须在内存恢复之后执行，
// 否则它会看到未提交的脏值。
func TestAfterRollbackHookSeesRestoredState(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uid := time.Now().UnixNano() + 3
	const actId int32 = 23
	defer cleanupRedEnvelope(t, ctx, uid)

	seedRedEnvelope(t, ctx, uid, actId, 2, 0)

	var obj *ptable.RedEnvelope
	hookToday := int32(-1)
	sentinel := errors.New("business failed")
	err := WithTx(ctx, func(ctx context.Context) error {
		obj = ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid, actId)
		if obj == nil {
			t.Fatal("查不到准备的数据")
		}
		obj.SetTodayCount(99)

		dbctx.AfterRollback(ctx, func(context.Context) {
			hookToday = obj.TodayCount()
		})
		return sentinel
	})

	if !errors.Is(err, sentinel) {
		t.Fatalf("期望业务错误原样返回，实际: %v", err)
	}
	if hookToday != 2 {
		t.Fatalf("AfterRollback 钩子应看到恢复后的值 2，实际 %d", hookToday)
	}
}

// TestRollbackRestoresMultipleCachedRecords 一次事务里改多行，失败后全部恢复。
func TestRollbackRestoresMultipleCachedRecords(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uid1 := time.Now().UnixNano() + 4
	uid2 := uid1 + 1
	const actId int32 = 24
	defer cleanupRedEnvelope(t, ctx, uid1)
	defer cleanupRedEnvelope(t, ctx, uid2)

	seedRedEnvelope(t, ctx, uid1, actId, 1, 0)
	seedRedEnvelope(t, ctx, uid2, actId, 2, 0)

	var a, b *ptable.RedEnvelope
	sentinel := errors.New("business failed")
	err := WithTx(ctx, func(ctx context.Context) error {
		a = ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid1, actId)
		b = ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid2, actId)
		if a == nil || b == nil {
			t.Fatal("查不到准备的数据")
		}
		a.SetTodayCount(10)
		b.SetTodayCount(20)
		return sentinel
	})

	if !errors.Is(err, sentinel) {
		t.Fatalf("期望业务错误原样返回，实际: %v", err)
	}
	if a.TodayCount() != 1 || b.TodayCount() != 2 {
		t.Fatalf("回滚后两行都应恢复，实际 %d/%d", a.TodayCount(), b.TodayCount())
	}
}
