package dbpool

import (
	"context"
	"errors"
	"testing"
	"time"

	"pdbgen/ptable"
)

func getRedEnvelopeID(t *testing.T, ctx context.Context, uid int64, actId int32) int64 {
	t.Helper()

	var id int64
	if err := WithTx(ctx, func(ctx context.Context) error {
		v := ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid, actId)
		if v == nil {
			t.Fatal("查不到准备的数据")
		}
		id = v.Id()
		return nil
	}); err != nil {
		t.Fatalf("查询准备数据失败: %v", err)
	}
	return id
}

// TestDeleteByIdCommits 删除在提交前 flush 成 DELETE，提交后行不存在。
func TestDeleteByIdCommits(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uid := time.Now().UnixNano()
	const actId int32 = 30
	defer cleanupRedEnvelope(t, ctx, uid)

	seedRedEnvelope(t, ctx, uid, actId, 1, 0)
	id := getRedEnvelopeID(t, ctx, uid, actId)

	if err := WithTx(ctx, func(ctx context.Context) error {
		if !ptable.UserRedEnvelopeTable.DeleteById(ctx, id) {
			t.Fatal("DeleteById 应该返回 true")
		}
		return nil
	}); err != nil {
		t.Fatalf("删除事务失败: %v", err)
	}

	if n, err := countRedEnvelope(ctx, uid); err != nil || n != 0 {
		t.Fatalf("删除后应该 0 行，实际 n=%d err=%v", n, err)
	}
}

// TestDeleteByIdMissing 删除不存在的行返回 false，不报错。
func TestDeleteByIdMissing(t *testing.T) {
	setupTestDB(t)

	missingID := time.Now().UnixNano()
	if err := WithTx(t.Context(), func(ctx context.Context) error {
		if ptable.UserRedEnvelopeTable.DeleteById(ctx, missingID) {
			t.Fatal("不存在的行 DeleteById 应该返回 false")
		}
		return nil
	}); err != nil {
		t.Fatalf("事务失败: %v", err)
	}
}

// TestDeleteRollbackRestores 删除后业务失败：数据库回滚，内存对象的 deleted 状态也恢复。
func TestDeleteRollbackRestores(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uid := time.Now().UnixNano() + 1
	const actId int32 = 31
	defer cleanupRedEnvelope(t, ctx, uid)

	seedRedEnvelope(t, ctx, uid, actId, 1, 0)
	id := getRedEnvelopeID(t, ctx, uid, actId)

	var obj *ptable.RedEnvelope
	sentinel := errors.New("business failed")
	err := WithTx(ctx, func(ctx context.Context) error {
		obj = ptable.UserRedEnvelopeTable.GetById(ctx, id)
		if obj == nil {
			t.Fatal("查不到准备的数据")
		}
		obj.Delete()
		return sentinel
	})

	if !errors.Is(err, sentinel) {
		t.Fatalf("期望业务错误原样返回，实际: %v", err)
	}
	if obj.IsDeleted() {
		t.Fatal("回滚后 IsDeleted 应该恢复为 false")
	}
	if stmt, _ := obj.FlushStmt(); stmt != "" {
		t.Fatalf("回滚后不应再有 DELETE，实际: %s", stmt)
	}
	if n, err := countRedEnvelope(ctx, uid); err != nil || n != 1 {
		t.Fatalf("数据库应该回滚，实际 n=%d err=%v", n, err)
	}
}

// TestDeleteHidesRowInTransaction 同一个事务里删除后，GetById / 唯一索引查询 /
// 列表查询都应该看不到这一行，即使 DELETE 还没 flush 到数据库。
func TestDeleteHidesRowInTransaction(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uid := time.Now().UnixNano() + 2
	const actId int32 = 32
	defer cleanupRedEnvelope(t, ctx, uid)

	seedRedEnvelope(t, ctx, uid, actId, 1, 0)
	id := getRedEnvelopeID(t, ctx, uid, actId)

	if err := WithTx(ctx, func(ctx context.Context) error {
		if !ptable.UserRedEnvelopeTable.DeleteById(ctx, id) {
			t.Fatal("DeleteById 应该返回 true")
		}
		if v := ptable.UserRedEnvelopeTable.GetById(ctx, id); v != nil {
			t.Fatal("删除后 GetById 应返回 nil")
		}
		if v := ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid, actId); v != nil {
			t.Fatal("删除后唯一索引查询应返回 nil")
		}
		for _, v := range ptable.UserRedEnvelopeTable.GetAll(ctx) {
			if v.Id() == id {
				t.Fatal("GetAll 不应包含已删除的行")
			}
		}
		if ptable.UserRedEnvelopeTable.DeleteById(ctx, id) {
			t.Fatal("重复删除应返回 false")
		}
		return nil
	}); err != nil {
		t.Fatalf("删除事务失败: %v", err)
	}

	if n, err := countRedEnvelope(ctx, uid); err != nil || n != 0 {
		t.Fatalf("提交后应该 0 行，实际 n=%d err=%v", n, err)
	}
}

// TestDeleteInsertedRow 同一个事务里先 Insert 再删除，提交后行不存在。
func TestDeleteInsertedRow(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uid := time.Now().UnixNano() + 3
	const actId int32 = 33
	defer cleanupRedEnvelope(t, ctx, uid)

	if err := WithTx(ctx, func(ctx context.Context) error {
		v := ptable.NewRedEnvelope()
		v.SetUid(uid)
		v.SetActId(actId)
		v.SetTodayCount(1)
		ptable.UserRedEnvelopeTable.Insert(ctx, v)
		if v.Id() == 0 {
			t.Fatal("Insert 应该回填主键")
		}
		if !ptable.UserRedEnvelopeTable.DeleteById(ctx, v.Id()) {
			t.Fatal("刚插入的行 DeleteById 应该返回 true")
		}
		return nil
	}); err != nil {
		t.Fatalf("事务失败: %v", err)
	}

	if n, err := countRedEnvelope(ctx, uid); err != nil || n != 0 {
		t.Fatalf("提交后应该 0 行，实际 n=%d err=%v", n, err)
	}
}
