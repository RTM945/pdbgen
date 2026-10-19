package dbpool

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"pdbgen/dbctx"
	"pdbgen/ptable"
	"pdbgen/readxml"
)

// 集成测试要连真实 PG（见 README 的 docker 命令）。连不上直接 skip，
// 让没有数据库的环境仍然能跑 go test ./...。
var (
	testInitOnce sync.Once
	testInitErr  error
)

// TestMain 让整个包的测试共用连接池，并在结束时收摊。
// 连不上库时把错误交给各个用例，由它们 skip。
func TestMain(m *testing.M) {
	schema, err := readxml.LoadSchema("../pdb.xml")
	if err != nil {
		testInitErr = err
	} else {
		testInitErr = Init(context.Background(), schema)
	}

	code := m.Run()

	if testInitErr == nil {
		Close()
	}
	os.Exit(code)
}

func setupTestDB(t *testing.T) {
	t.Helper()

	// pgbouncer 刚起来时第一条连接可能要等几秒，超时给宽一点，避免把"还没预热"
	// 误判成"数据库没跑"而静默 skip。
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	if testInitErr != nil {
		t.Skipf("跳过集成测试，初始化失败: %v", testInitErr)
	}
	// 注意这里的 dbpool 是包内那个池变量（包级 var dbpool *pgxpool.Pool），
	// dbpool.Ping 是 (*pgxpool.Pool).Ping 的方法值，不是包级函数。
	if err := dbpool.Ping(ctx); err != nil {
		t.Skipf("跳过集成测试，连不上 pdb.xml 里的数据库（需要 README 里的 docker 环境）: %v", err)
	}
}

// catchPanic 跑 fn 并返回 panic 的内容，没 panic 就返回空串。
// panic 由 WithTxPrepare 的 defer 回滚后原样重抛，必须包住整个 WithTx 调用。
func catchPanic(t *testing.T, fn func()) (recovered string) {
	t.Helper()

	defer func() {
		if r := recover(); r != nil {
			recovered = fmt.Sprint(r)
		}
	}()

	fn()
	return ""
}

// TestDuplicateRowDetected 是这次改动的核心：同一事务里同一行被两条查询路径
// 各查一次、两边都改，必须 panic 回滚，而不是让后写的静默覆盖先写的。
func TestDuplicateRowDetected(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	// 每次跑用一个新 uid，避免和已有数据（以及上一次失败留下的数据）冲突。
	uid := time.Now().UnixNano()
	const actId int32 = 1

	defer func() {
		_ = WithTx(ctx, func(ctx context.Context) error {
			_, err := dbctx.SQLTxFromCtx(ctx).Exec(
				ctx, "DELETE FROM user_red_envelope WHERE uid = $1", uid,
			)
			return err
		})
	}()

	// 造一行并提交。
	if err := WithTx(ctx, func(ctx context.Context) error {
		v := ptable.NewRedEnvelope()
		v.SetUid(uid)
		v.SetActId(actId)
		v.SetTodayCount(2)
		v.SetTotal(3)
		ptable.UserRedEnvelopeTable.Insert(ctx, v)
		return nil
	}); err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}

	// 危险场景：索引查一次、主键再查一次，两个对象各改一列。
	// panic 由 WithTxPrepare 的 defer 回滚后原样重抛，所以 recover 必须包住
	// 整个 WithTx 调用，放在回调里是接不到的。
	msg := catchPanic(t, func() {
		_ = WithTx(ctx, func(ctx context.Context) error {
			byIndex := ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid, actId)
			if byIndex == nil {
				t.Fatal("查不到准备的数据")
			}
			byIndex.SetTodayCount(11)

			byPK := ptable.UserRedEnvelopeTable.GetById(ctx, byIndex.Id())
			if byPK == nil {
				t.Fatal("按主键查不到数据")
			}
			byPK.SetTotal(42)
			return nil
		})
	})

	if !strings.Contains(msg, "user_red_envelope") || !strings.Contains(msg, "id=") {
		t.Fatalf("panic 信息应该指明是哪张表的哪一行，实际: %s", msg)
	}
	t.Logf("检测到的报错: %s", msg)

	// 事务回滚了，两列都必须是原值——没有任何一列被写进去。
	if err := WithTx(ctx, func(ctx context.Context) error {
		v := ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid, actId)
		if v.TodayCount() != 2 || v.Total() != 3 {
			t.Fatalf(
				"panic 之后事务应该整体回滚，期望 today_count=2 total=3，实际 %d/%d",
				v.TodayCount(), v.Total(),
			)
		}
		return nil
	}); err != nil {
		t.Fatalf("校验回滚结果失败: %v", err)
	}
}

// TestDuplicateDetectedEvenWithoutConflict 记录守卫的严格程度：**只看有没有两个
// 对象指向同一行，不看它们脏不脏**。即使只有其中一个被改过、两条 UPDATE 其实不
// 会冲突，也会 panic。
//
// 这是刻意的：在注册（也就是第二次查询）时就炸，比等到 flush 再比较谁的脏位跟谁
// 重叠要简单得多，而"同一行查两次"本身就该改掉。代价是极少数无害的写法会被拦，
// 报错信息里写了怎么改——复用第一次查出来的对象。
func TestDuplicateDetectedEvenWithoutConflict(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uid := time.Now().UnixNano() + 1
	const actId int32 = 2

	defer func() {
		_ = WithTx(ctx, func(ctx context.Context) error {
			_, err := dbctx.SQLTxFromCtx(ctx).Exec(
				ctx, "DELETE FROM user_red_envelope WHERE uid = $1", uid,
			)
			return err
		})
	}()

	if err := WithTx(ctx, func(ctx context.Context) error {
		v := ptable.NewRedEnvelope()
		v.SetUid(uid)
		v.SetActId(actId)
		v.SetTodayCount(1)
		ptable.UserRedEnvelopeTable.Insert(ctx, v)
		return nil
	}); err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}

	msg := catchPanic(t, func() {
		_ = WithTx(ctx, func(ctx context.Context) error {
			a := ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid, actId)
			if a == nil {
				t.Fatal("查不到准备的数据")
			}
			b := ptable.UserRedEnvelopeTable.GetById(ctx, a.Id())
			if b == nil {
				t.Fatal("按主键查不到数据")
			}
			// 只改其中一个，另一个纯读——依然会被拦。
			a.SetTodayCount(5)
			return nil
		})
	})

	if msg == "" {
		t.Fatal("同一行查出两个对象就应该 panic，即使只有一个被修改")
	}

	// 事务回滚，数据保持原值。
	if err := WithTx(ctx, func(ctx context.Context) error {
		v := ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid, actId)
		if v.TodayCount() != 1 {
			t.Fatalf("panic 之后应该回滚，期望 today_count=1，实际 %d", v.TodayCount())
		}
		return nil
	}); err != nil {
		t.Fatalf("校验结果失败: %v", err)
	}
}

// TestNewTransactionSeesCommittedData 上一个事务提交后，新事务读到的是库里的值，
// 不会把上一个事务留在内存里的状态带过来。
func TestNewTransactionSeesCommittedData(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uid := time.Now().UnixNano() + 2
	const actId int32 = 3

	defer func() {
		_ = WithTx(ctx, func(ctx context.Context) error {
			_, err := dbctx.SQLTxFromCtx(ctx).Exec(
				ctx, "DELETE FROM user_red_envelope WHERE uid = $1", uid,
			)
			return err
		})
	}()

	var insertedID int64
	if err := WithTx(ctx, func(ctx context.Context) error {
		v := ptable.NewRedEnvelope()
		v.SetUid(uid)
		v.SetActId(actId)
		v.SetTodayCount(1)
		ptable.UserRedEnvelopeTable.Insert(ctx, v)
		insertedID = v.Id()
		return nil
	}); err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}

	if err := WithTx(ctx, func(ctx context.Context) error {
		v := ptable.UserRedEnvelopeTable.GetById(ctx, insertedID)
		if v == nil {
			t.Fatal("第二个事务查不到上一个事务提交的行")
		}
		if v.TodayCount() != 1 {
			t.Fatalf("expected today_count=1，actual %d", v.TodayCount())
		}
		return nil
	}); err != nil {
		t.Fatalf("第二个事务失败: %v", err)
	}
}
