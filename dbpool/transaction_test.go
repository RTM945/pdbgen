package dbpool

import (
	"testing"

	"pdbgen/dbctx"
)

// rowStub 是 dbctx.Flusher 的最小实现，用来单独验证 transaction 的缓存记录解析，
// 不依赖数据库和生成代码。
type rowStub struct {
	table    string
	id       int64
	value    int
	dirty    uint64
	deleted  bool
	detached bool
}

func (s *rowStub) TableName() string { return s.table }

func (s *rowStub) Identity() int64 { return s.id }

func (s *rowStub) FlushStmt() (string, []any) {
	if s.dirty == 0 {
		return "", nil
	}
	return "UPDATE " + s.table + " SET x = 1 WHERE id = $1", []any{s.id}
}

func (s *rowStub) FlushDone() { s.dirty = 0 }

func (s *rowStub) DirtyColumns() uint64 { return s.dirty }

func (s *rowStub) IsDeleted() bool { return s.deleted }

func (s *rowStub) SnapshotState() dbctx.RecordState { return *s }

func (s *rowStub) RestoreState(state dbctx.RecordState) { *s = state.(rowStub) }

func (s *rowStub) Detach() { s.detached = true }

var _ dbctx.Flusher = (*rowStub)(nil)

// TestAddCachedRecordReturnsExisting 同一行的第二个对象不会登记，
// 返回第一次登记的对象（xdb: getCachedTRecord 命中后直接用已有记录）。
func TestAddCachedRecordReturnsExisting(t *testing.T) {
	txn := newTransaction(nil)
	first := &rowStub{table: "user_mail", id: 7}
	second := &rowStub{table: "user_mail", id: 7}

	if got := txn.AddCachedRecord(first); got != first {
		t.Fatalf("第一次登记应返回对象本身，实际 %p", got)
	}
	if got := txn.AddCachedRecord(second); got != first {
		t.Fatalf("同一行第二次登记应返回已有对象，实际 %p", got)
	}
	if len(txn.items) != 1 || len(txn.snapshots) != 1 {
		t.Fatalf("被丢弃的对象不应进 flush 列表或拍快照，实际 items=%d snapshots=%d", len(txn.items), len(txn.snapshots))
	}
	if rec, ok := txn.GetCachedRecord("user_mail", 7); !ok || rec != first {
		t.Fatalf("GetCachedRecord 应返回第一次登记的对象")
	}
}

// TestAddCachedRecordSamePointer 同一个对象重复登记不会重复进 flush 列表。
func TestAddCachedRecordSamePointer(t *testing.T) {
	txn := newTransaction(nil)
	obj := &rowStub{table: "user_mail", id: 7}

	txn.AddCachedRecord(obj)
	txn.AddCachedRecord(obj)

	if len(txn.items) != 1 {
		t.Fatalf("同一个对象应该只收集一次，实际 %d 次", len(txn.items))
	}
}

// TestAddCachedRecordDifferentRows 不同行、不同表都可以共存。
func TestAddCachedRecordDifferentRows(t *testing.T) {
	txn := newTransaction(nil)
	txn.AddCachedRecord(&rowStub{table: "user_mail", id: 7})
	txn.AddCachedRecord(&rowStub{table: "user_mail", id: 8})
	txn.AddCachedRecord(&rowStub{table: "user_gm_mail", id: 7})

	if len(txn.items) != 3 {
		t.Fatalf("期望收集 3 个对象，实际 %d", len(txn.items))
	}
}

// TestAddCachedRecordAfterIdentityAssigned 对应 Insert 的时序：
// 对象先以 Identity=0 登记（只拍快照），RETURNING id 之后再登记一次进身份表，
// 不会重复进 flush 列表。
func TestAddCachedRecordAfterIdentityAssigned(t *testing.T) {
	txn := newTransaction(nil)
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
	txn := newTransaction(nil)
	if rec, ok := txn.GetCachedRecord("user_mail", 7); ok || rec != nil {
		t.Fatalf("未登记的行不应命中，实际 rec=%v ok=%v", rec, ok)
	}
}

// TestRollbackRestoresSnapshotsAndDetaches 回滚恢复所有快照，然后全部脱离事务。
func TestRollbackRestoresSnapshotsAndDetaches(t *testing.T) {
	txn := newTransaction(nil)
	a := &rowStub{table: "user_mail", id: 1, value: 1}
	b := &rowStub{table: "user_gm_mail", id: 2, value: 2}
	txn.AddCachedRecord(a)
	txn.AddCachedRecord(b)

	a.value, a.dirty = 10, 1
	b.value = 20
	txn.rollback()

	if a.value != 1 || b.value != 2 || a.dirty != 0 {
		t.Fatalf("回滚后应恢复所有快照，实际 a=%d(dirty=%d) b=%d", a.value, a.dirty, b.value)
	}
	if !a.detached || !b.detached {
		t.Fatal("回滚后记录应全部脱离事务")
	}
	if len(txn.items) != 0 || txn.snapshots != nil || txn.cachedRecords != nil {
		t.Fatal("回滚后应释放对记录的引用")
	}
}

// TestRollbackRestoresInsertedRecord Insert 的对象回滚后回到插入前：主键为 0，不在身份表里。
func TestRollbackRestoresInsertedRecord(t *testing.T) {
	txn := newTransaction(nil)
	obj := &rowStub{table: "user_mail", value: 5}
	txn.AddCachedRecord(obj) // 主键回填前登记

	obj.id, obj.value = 7, 99
	txn.AddCachedRecord(obj) // 主键回填后登记
	txn.rollback()

	if obj.id != 0 || obj.value != 5 {
		t.Fatalf("Insert 回滚后应回到插入前，实际 id=%d value=%d", obj.id, obj.value)
	}
	if _, ok := txn.GetCachedRecord("user_mail", 7); ok {
		t.Fatal("Insert 回滚后身份表不应再有这行")
	}
}
