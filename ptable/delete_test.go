package ptable

import (
	"context"
	"strings"
	"testing"
)

// 这个文件是手写测试，不是生成产物；用生成的 RedEnvelope / User 验证实体的状态机，
// 不依赖 PostgreSQL。

func loadedRedEnvelope() *RedEnvelope {
	return &RedEnvelope{id: 7, uid: 100, actId: 1, lastRefreshAt: 123, todayCount: 2, total: 3, loaded: true}
}

// mustPanic 跑 fn，断言它 panic 且信息包含 want。
func mustPanic(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		t.Helper()
		r := recover()
		if r == nil {
			t.Fatalf("应该 panic（期望包含 %q）", want)
		}
		if msg, _ := r.(string); !strings.Contains(msg, want) {
			t.Fatalf("panic 信息不对，期望包含 %q，实际: %v", want, r)
		}
	}()
	fn()
}

func TestRedEnvelopeDeleteFlush(t *testing.T) {
	v := loadedRedEnvelope()

	v.Delete()
	if !v.IsDeleted() {
		t.Fatal("Delete 后 IsDeleted 应为 true")
	}

	stmt, args := v.FlushStmt()
	if want := `DELETE FROM "user_red_envelope" WHERE "id" = $1`; stmt != want {
		t.Fatalf("DELETE 语句不符，期望 %q，实际 %q", want, stmt)
	}
	if len(args) != 1 || args[0] != int64(7) {
		t.Fatalf("DELETE 参数不符，实际 %#v", args)
	}

	v.FlushDone()
	if v.IsDeleted() {
		t.Fatal("FlushDone 后应为未加载，deleted 清零")
	}
	if stmt, _ := v.FlushStmt(); stmt != "" {
		t.Fatalf("已删除的对象不应再产生语句，实际 %q", stmt)
	}
}

func TestRedEnvelopeUpdateOnlyDirtyColumns(t *testing.T) {
	v := loadedRedEnvelope()
	v.SetTotal(4)
	v.SetTodayCount(2) // 值没变，不置脏位

	stmt, args := v.FlushStmt()
	if want := `UPDATE "user_red_envelope" SET "total" = $1 WHERE "id" = $2`; stmt != want {
		t.Fatalf("UPDATE 语句不符，期望 %q，实际 %q", want, stmt)
	}
	if len(args) != 2 || args[0] != int32(4) || args[1] != int64(7) {
		t.Fatalf("UPDATE 参数不符，实际 %#v", args)
	}
	if v.DirtyColumns() != dirtyRedEnvelopeTotal {
		t.Fatalf("DirtyColumns 应该只有 total，实际 %b", v.DirtyColumns())
	}

	v.FlushDone()
	if stmt, _ := v.FlushStmt(); stmt != "" || v.DirtyColumns() != 0 {
		t.Fatalf("FlushDone 后不应再有待写入的修改，实际 %q", stmt)
	}
}

func TestRedEnvelopeDeleteRollbackSnapshot(t *testing.T) {
	v := loadedRedEnvelope()
	state := v.SnapshotState()

	v.SetTotal(99)
	v.Delete()
	v.RestoreState(state)

	if v.IsDeleted() || v.Total() != 3 {
		t.Fatalf("RestoreState 后应回到快照，实际 deleted=%v total=%d", v.IsDeleted(), v.Total())
	}
	if stmt, _ := v.FlushStmt(); stmt != "" {
		t.Fatalf("恢复后没有脏位，不应产生语句，实际 %q", stmt)
	}
}

// TestRedEnvelopeDetachBlocksWrites 脱离事务的实体可以读，不能改。
func TestRedEnvelopeDetachBlocksWrites(t *testing.T) {
	v := loadedRedEnvelope()
	v.Detach()

	if v.TodayCount() != 2 {
		t.Fatalf("脱离事务后仍然可以读，实际 %d", v.TodayCount())
	}
	mustPanic(t, "user_red_envelope id=7 已脱离事务", func() { v.SetTodayCount(5) })
	mustPanic(t, "已脱离事务", func() { v.SetTodayCount(2) }) // 值没变也不行：写的意图本身就是错的
	mustPanic(t, "已脱离事务", v.Delete)
}

// TestNewRedEnvelopeDetachIsNoop 还没插入的新对象改了不会丢（Insert 时才写），Detach 不影响它。
func TestNewRedEnvelopeDetachIsNoop(t *testing.T) {
	v := NewRedEnvelope()
	v.Detach()
	v.SetTotal(1)
	if v.Total() != 1 {
		t.Fatalf("新对象应该可以改，实际 %d", v.Total())
	}
}

// TestInsertRejectsLoadedRecord 已经对应数据库里一行的对象不能再 Insert。
func TestInsertRejectsLoadedRecord(t *testing.T) {
	mustPanic(t, "Insert 只接受 NewRedEnvelope()", func() {
		UserRedEnvelopeTable.Insert(context.Background(), loadedRedEnvelope())
	})
}
