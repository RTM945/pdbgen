package dbpool

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"pdbgen/ptable"
	"pdbgen/readxml"
)

// 集成测试要连真实 PG（见 README 的 docker 命令）。连不上时默认 skip，让没有数据库的
// 环境仍然能跑 go test ./...；设置 PDBGEN_REQUIRE_DB=1（CI 里）则直接失败，免得
// "全部 skip" 被当成 "全部通过"。
var (
	testSchema  *readxml.Schema
	testInitErr error
)

// TestMain 让整个包的测试共用连接池，并在结束时收摊。
// 连不上库时把错误交给各个用例，由它们 skip。
func TestMain(m *testing.M) {
	testSchema, testInitErr = readxml.LoadSchema("../pdb.xml")
	if testInitErr == nil {
		testInitErr = Init(context.Background(), testSchema)
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

	skip := t.Skipf
	if os.Getenv("PDBGEN_REQUIRE_DB") != "" {
		skip = t.Fatalf
	}
	if testInitErr != nil {
		skip("跳过集成测试，初始化失败: %v", testInitErr)
	}
	// 注意这里的 dbpool 是包内那个池变量（包级 var dbpool *pgxpool.Pool），
	// dbpool.Ping 是 (*pgxpool.Pool).Ping 的方法值，不是包级函数。
	if err := dbpool.Ping(ctx); err != nil {
		skip("跳过集成测试，连不上 pdb.xml 里的数据库（需要 README 里的 docker 环境）: %v", err)
	}
}

// catchPanic 跑 fn 并返回 panic 的内容，没 panic 就返回空串。
// panic 由 Procedure 的 defer 回滚后原样重抛，必须包住整个 WithTx 调用。
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

func seedRedEnvelope(t *testing.T, ctx context.Context, uid int64, actId int32, today, total int32) {
	t.Helper()

	if err := WithTx(ctx, func(ctx context.Context) error {
		v := ptable.NewRedEnvelope()
		v.SetUid(uid)
		v.SetActId(actId)
		v.SetTodayCount(today)
		v.SetTotal(total)
		ptable.UserRedEnvelopeTable.Insert(ctx, v)
		return nil
	}); err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}
}

// TestSameRowReturnsSameCachedRecord 同一行被索引和主键各查一次时，
// 第二次查询返回第一次的缓存记录（xdb: getCachedTRecord 命中），
// 而不是两个对象各改各的。提交时只发一条 UPDATE。
func TestSameRowReturnsSameCachedRecord(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uid := time.Now().UnixNano()
	const actId int32 = 1
	defer cleanupRedEnvelope(t, ctx, uid)

	seedRedEnvelope(t, ctx, uid, actId, 2, 3)

	if err := WithTx(ctx, func(ctx context.Context) error {
		byIndex := ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid, actId)
		if byIndex == nil {
			t.Fatal("查不到准备的数据")
		}
		byIndex.SetTodayCount(11)

		byPK := ptable.UserRedEnvelopeTable.GetById(ctx, byIndex.Id())
		if byPK == nil {
			t.Fatal("按主键查不到数据")
		}
		if byPK != byIndex {
			t.Fatalf("同一行应该返回同一个缓存记录，实际 %p != %p", byPK, byIndex)
		}

		byPK.SetTotal(42)
		return nil
	}); err != nil {
		t.Fatalf("事务失败: %v", err)
	}

	if err := WithTx(ctx, func(ctx context.Context) error {
		v := ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid, actId)
		if v.TodayCount() != 11 || v.Total() != 42 {
			t.Fatalf(
				"期望 today_count=11 total=42，实际 %d/%d",
				v.TodayCount(), v.Total(),
			)
		}
		return nil
	}); err != nil {
		t.Fatalf("校验结果失败: %v", err)
	}
}

// TestSameRowReadOnlyReturnsSameCachedRecord 即使一行都没改，
// 第二次查询也返回同一个缓存记录——身份表和脏不脏无关。
func TestSameRowReadOnlyReturnsSameCachedRecord(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uid := time.Now().UnixNano() + 1
	const actId int32 = 2
	defer cleanupRedEnvelope(t, ctx, uid)

	seedRedEnvelope(t, ctx, uid, actId, 1, 0)

	if err := WithTx(ctx, func(ctx context.Context) error {
		a := ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid, actId)
		if a == nil {
			t.Fatal("查不到准备的数据")
		}
		b := ptable.UserRedEnvelopeTable.GetById(ctx, a.Id())
		if b != a {
			t.Fatalf("同一行应该返回同一个缓存记录，实际 %p != %p", b, a)
		}
		return nil
	}); err != nil {
		t.Fatalf("事务失败: %v", err)
	}
}

// TestListAndSingleQueryReturnSameCachedRecord QueryList 登记过的行，
// 再按主键查一次也返回列表里的同一个对象。
func TestListAndSingleQueryReturnSameCachedRecord(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uid := time.Now().UnixNano() + 2
	const actId int32 = 3
	defer cleanupRedEnvelope(t, ctx, uid)

	seedRedEnvelope(t, ctx, uid, actId, 1, 0)

	if err := WithTx(ctx, func(ctx context.Context) error {
		var found *ptable.RedEnvelope
		for _, v := range ptable.UserRedEnvelopeTable.GetAll(ctx) {
			if v.Uid() == uid && v.ActId() == actId {
				found = v
				break
			}
		}
		if found == nil {
			t.Fatal("GetAll 里找不到准备的数据")
		}

		byID := ptable.UserRedEnvelopeTable.GetById(ctx, found.Id())
		if byID != found {
			t.Fatalf("列表和单行查询应该返回同一个缓存记录，实际 %p != %p", byID, found)
		}
		return nil
	}); err != nil {
		t.Fatalf("事务失败: %v", err)
	}
}

// TestNewTransactionSeesCommittedData 上一个事务提交后，新事务读到的是库里的值，
// 不会把上一个事务留在内存里的状态带过来。
func TestNewTransactionSeesCommittedData(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uid := time.Now().UnixNano() + 3
	const actId int32 = 4
	defer cleanupRedEnvelope(t, ctx, uid)

	seedRedEnvelope(t, ctx, uid, actId, 1, 0)

	var insertedID int64
	if err := WithTx(ctx, func(ctx context.Context) error {
		v := ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid, actId)
		insertedID = v.Id()
		return nil
	}); err != nil {
		t.Fatalf("查询准备数据失败: %v", err)
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
