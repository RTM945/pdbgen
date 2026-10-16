package dbpool

import (
	"strings"
	"testing"

	"pdbgen/dbctx"
)

// rowStub 是 dbctx.Flusher 的最小实现，用来单独验证 UnitOfWork 的重复行检测，
// 不依赖数据库和生成代码。
type rowStub struct {
	table string
	id    int64
}

func (s *rowStub) TableName() string { return s.table }

func (s *rowStub) Identity() int64 { return s.id }

func (s *rowStub) FlushStmt() (string, []any) {
	return "UPDATE " + s.table + " SET x = 1 WHERE id = $1", []any{s.id}
}

func (s *rowStub) FlushDone(int64) error { return nil }

// TestRegisterDetectsDuplicateRow 同一个事务里两个对象指向同一行 → panic。
// 这是唯一会静默丢数据的场景：两条 UPDATE 各写各的绝对值，后写的覆盖先写的。
func TestRegisterDetectsDuplicateRow(t *testing.T) {
	u := NewUnitOfWork()
	first := &rowStub{table: "user_mail", id: 7}
	u.Register(first)

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("同一行登记两个对象应该 panic")
		}
		msg := ""
		if s, ok := r.(string); ok {
			msg = s
		}
		if !strings.Contains(msg, "user_mail") || !strings.Contains(msg, "id=7") {
			t.Fatalf("panic 信息应该带上表名和主键，实际: %s", msg)
		}
	}()

	u.Register(&rowStub{table: "user_mail", id: 7})
}

// TestRegisterSamePointerIsNotDuplicate 同一个对象登记两次不是重复行——
// QueryOne / QueryList 都会登记，业务代码也可能重复注册同一个指针。
func TestRegisterSamePointerIsNotDuplicate(t *testing.T) {
	u := NewUnitOfWork()
	obj := &rowStub{table: "user_mail", id: 7}

	u.Register(obj)
	u.Register(obj) // 不应该 panic

	if len(u.items) != 1 {
		t.Fatalf("同一个对象应该只收集一次，实际 %d 次", len(u.items))
	}
}

// TestRegisterDifferentRowsIsFine 不同行、不同表都可以共存。
func TestRegisterDifferentRowsIsFine(t *testing.T) {
	u := NewUnitOfWork()
	u.Register(&rowStub{table: "user_mail", id: 7})
	u.Register(&rowStub{table: "user_mail", id: 8})
	u.Register(&rowStub{table: "user_gm_mail", id: 7})

	if len(u.items) != 3 {
		t.Fatalf("期望收集 3 个对象，实际 %d", len(u.items))
	}
}

// TestRegisterZeroIdentitySkipped 主键未回填（还没 Insert）的对象没有身份，
// 不参与检测，也不会互相误伤。
func TestRegisterZeroIdentitySkipped(t *testing.T) {
	u := NewUnitOfWork()
	u.Register(&rowStub{table: "user_mail", id: 0})
	u.Register(&rowStub{table: "user_mail", id: 0})

	if len(u.items) != 2 {
		t.Fatalf("期望收集 2 个对象，实际 %d", len(u.items))
	}
	if len(u.rows) != 0 {
		t.Fatalf("没有主键的对象不应该进 rows，实际 %d 条", len(u.rows))
	}
}

var _ dbctx.Flusher = (*rowStub)(nil)
