package dbpool

import (
	"testing"

	"pdbgen/dbctx"
)

// rowStub 是 dbctx.Flusher 的最小实现，用来单独验证 Transaction 的缓存记录解析，
// 不依赖数据库和生成代码。
type rowStub struct {
	table string
	id    int64
	value int
}

func (s *rowStub) TableName() string { return s.table }

func (s *rowStub) Identity() int64 { return s.id }

func (s *rowStub) FlushStmt() (string, []any) {
	return "UPDATE " + s.table + " SET x = 1 WHERE id = $1", []any{s.id}
}

func (s *rowStub) FlushDone(int64) error { return nil }

func (s *rowStub) SnapshotState() dbctx.RecordState { return *s }

func (s *rowStub) RestoreState(state dbctx.RecordState) { *s = state.(rowStub) }

func (s *rowStub) ResetNew() { s.id = 0 }

// TestAddCachedRecordReturnsExisting 同一行的第二个对象不会登记，
// 返回第一次登记的对象（xdb: getCachedTRecord 命中后直接用已有记录）。
func TestAddCachedRecordReturnsExisting(t *testing.T) {
	txn := NewTransaction()
	first := &rowStub{table: "user_mail", id: 7}
	second := &rowStub{table: "user_mail", id: 7}

	if got := txn.AddCachedRecord(first); got != first {
		t.Fatalf("第一次登记应返回对象本身，实际 %p", got)
	}
	if got := txn.AddCachedRecord(second); got != first {
		t.Fatalf("同一行第二次登记应返回已有对象，实际 %p", got)
	}
	if len(txn.items) != 1 {
		t.Fatalf("同一行只应进一次 flush 列表，实际 %d 次", len(txn.items))
	}
	if rec, ok := txn.GetCachedRecord("user_mail", 7); !ok || rec != first {
		t.Fatalf("GetCachedRecord 应返回第一次登记的对象")
	}
}

// TestAddCachedRecordSamePointer 同一个对象重复登记不会重复进 flush 列表。
func TestAddCachedRecordSamePointer(t *testing.T) {
	txn := NewTransaction()
	obj := &rowStub{table: "user_mail", id: 7}

	txn.AddCachedRecord(obj)
	txn.AddCachedRecord(obj)

	if len(txn.items) != 1 {
		t.Fatalf("同一个对象应该只收集一次，实际 %d 次", len(txn.items))
	}
}

// TestAddCachedRecordDifferentRows 不同行、不同表都可以共存。
func TestAddCachedRecordDifferentRows(t *testing.T) {
	txn := NewTransaction()
	txn.AddCachedRecord(&rowStub{table: "user_mail", id: 7})
	txn.AddCachedRecord(&rowStub{table: "user_mail", id: 8})
	txn.AddCachedRecord(&rowStub{table: "user_gm_mail", id: 7})

	if len(txn.items) != 3 {
		t.Fatalf("期望收集 3 个对象，实际 %d", len(txn.items))
	}
}

// TestAddCachedRecordZeroIdentity 主键未回填（还没 Insert）的对象没有身份，
// 照常进 flush 列表，但不进身份表。
func TestAddCachedRecordZeroIdentity(t *testing.T) {
	txn := NewTransaction()
	txn.AddCachedRecord(&rowStub{table: "user_mail", id: 0})
	txn.AddCachedRecord(&rowStub{table: "user_mail", id: 0})

	if len(txn.items) != 2 {
		t.Fatalf("期望收集 2 个对象，实际 %d", len(txn.items))
	}
	if len(txn.cachedRecords) != 0 {
		t.Fatalf("没有主键的对象不应该进身份表，实际 %d 条", len(txn.cachedRecords))
	}
}

// TestAddCachedRecordAfterIdentityAssigned 对应 Insert 的时序：
// 对象先以 Identity=0 登记，RETURNING id 之后再登记一次，只更新身份表，
// 不会重复进 flush 列表。
func TestAddCachedRecordAfterIdentityAssigned(t *testing.T) {
	txn := NewTransaction()
	obj := &rowStub{table: "user_mail", id: 0}

	txn.AddCachedRecord(obj)
	if len(txn.cachedRecords) != 0 {
		t.Fatalf("Identity=0 时不应该进身份表")
	}

	obj.id = 7
	if got := txn.AddCachedRecord(obj); got != obj {
		t.Fatalf("回填主键后应返回对象本身，实际 %p", got)
	}
	if rec, ok := txn.GetCachedRecord("user_mail", 7); !ok || rec != obj {
		t.Fatalf("回填主键后 GetCachedRecord 应命中")
	}
	if len(txn.items) != 1 {
		t.Fatalf("回填主键不应重复进 flush 列表，实际 %d 次", len(txn.items))
	}
}

// TestGetCachedRecordMiss 没登记过的行返回 false。
func TestGetCachedRecordMiss(t *testing.T) {
	txn := NewTransaction()
	if rec, ok := txn.GetCachedRecord("user_mail", 7); ok || rec != nil {
		t.Fatalf("未登记的行不应命中，实际 rec=%v ok=%v", rec, ok)
	}
}

func TestRollbackRestoresSnapshots(t *testing.T) {
	txn := NewTransaction()
	obj := &rowStub{table: "user_mail", id: 7, value: 1}
	txn.AddCachedRecord(obj)

	obj.value = 99
	txn.Rollback()

	if obj.value != 1 {
		t.Fatalf("回滚后应恢复快照，实际 value=%d", obj.value)
	}
	if obj.id != 7 {
		t.Fatalf("回滚后 id 应恢复，实际 id=%d", obj.id)
	}
}

func TestRollbackRestoresMultipleRecords(t *testing.T) {
	txn := NewTransaction()
	a := &rowStub{table: "user_mail", id: 1, value: 1}
	b := &rowStub{table: "user_gm_mail", id: 2, value: 2}
	txn.AddCachedRecord(a)
	txn.AddCachedRecord(b)

	a.value = 10
	b.value = 20
	txn.Rollback()

	if a.value != 1 || b.value != 2 {
		t.Fatalf("回滚后应恢复所有快照，实际 a=%d b=%d", a.value, b.value)
	}
}

func TestRollbackResetsNewRecord(t *testing.T) {
	txn := NewTransaction()
	obj := &rowStub{table: "user_mail", id: 0, value: 5}
	txn.RegisterNew(obj)

	obj.id = 7
	obj.value = 99
	txn.AddCachedRecord(obj)

	if rec, ok := txn.GetCachedRecord("user_mail", 7); !ok || rec != obj {
		t.Fatalf("Insert 后应能按主键命中")
	}

	txn.Rollback()

	if obj.id != 0 {
		t.Fatalf("Insert 回滚后 id 应清零，实际 id=%d", obj.id)
	}
	if _, ok := txn.GetCachedRecord("user_mail", 7); ok {
		t.Fatal("Insert 回滚后身份表不应再有这行")
	}
}

var _ dbctx.Flusher = (*rowStub)(nil)
