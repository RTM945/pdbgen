package ptable

import "testing"

// 这个文件是手写测试，不是生成产物；用来验证 Delete 的状态机，
// 不依赖 PostgreSQL。

func TestRedEnvelopeDeleteFlush(t *testing.T) {
	v := loadedRedEnvelope(7, 100, 1, 123, 2, 3)

	v.Delete()
	if !v.IsDeleted() {
		t.Fatal("Delete 后 IsDeleted 应为 true")
	}

	stmt, args := v.FlushStmt()
	want := `DELETE FROM "user_red_envelope" WHERE "id" = $1`
	if stmt != want {
		t.Fatalf("DELETE 语句不符，期望 %q，实际 %q", want, stmt)
	}
	if len(args) != 1 || args[0] != int64(7) {
		t.Fatalf("DELETE 参数不符，实际 %#v", args)
	}

	if err := v.FlushDone(1); err != nil {
		t.Fatalf("FlushDone 失败: %v", err)
	}
	if v.IsDeleted() {
		t.Fatal("FlushDone 后应为未加载，deleted 清零")
	}
	if stmt, _ := v.FlushStmt(); stmt != "" {
		t.Fatalf("已删除的对象不应再产生语句，实际 %q", stmt)
	}
}

func TestRedEnvelopeDeleteRollbackSnapshot(t *testing.T) {
	v := loadedRedEnvelope(7, 100, 1, 123, 2, 3)
	state := v.SnapshotState()

	v.Delete()
	v.RestoreState(state)

	if v.IsDeleted() {
		t.Fatal("RestoreState 后 deleted 应恢复为 false")
	}
	if stmt, _ := v.FlushStmt(); stmt != "" {
		t.Fatalf("恢复后没有脏位，不应产生语句，实际 %q", stmt)
	}
	if v.TodayCount() != 2 || v.Total() != 3 {
		t.Fatalf("恢复后字段值不符，实际 %d/%d", v.TodayCount(), v.Total())
	}
}

func TestRedEnvelopeDeleteMissing(t *testing.T) {
	v := loadedRedEnvelope(7, 100, 1, 123, 2, 3)
	v.Delete()

	if err := v.FlushDone(0); err == nil {
		t.Fatal("删除 0 行应返回 ErrRedEnvelopeNotFound")
	}
}

func TestRedEnvelopeResetToLoadedClearsDelete(t *testing.T) {
	v := loadedRedEnvelope(7, 100, 1, 123, 2, 3)
	v.Delete()
	v.ResetToLoaded()

	if v.IsDeleted() {
		t.Fatal("ResetToLoaded 后 deleted 应为 false")
	}
	if stmt, _ := v.FlushStmt(); stmt != "" {
		t.Fatalf("ResetToLoaded 后不应产生语句，实际 %q", stmt)
	}
}
