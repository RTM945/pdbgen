package dbpool

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pdbgen/dbctx"
	"pdbgen/ptable"
	"pdbgen/readxml"
)

// 这个文件验证事务边界：Procedure / Query 怎么结束、返回什么，以及 Open / Close 的守卫。

func selectRedEnvelope(t *testing.T, ctx context.Context, uid int64, actId int32) *ptable.RedEnvelope {
	t.Helper()
	var v *ptable.RedEnvelope
	if err := Query(ctx, func(ctx context.Context) error {
		v = ptable.UserRedEnvelopeTable.SelectByUidActId(ctx, uid, actId)
		return nil
	}); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	return v
}

// TestGoexitRollsBack fn 里调用 runtime.Goexit（测试里的 t.Fatal / t.FailNow 就是它）：
// 事务回滚，AfterRollback 执行、AfterCommit 不执行。以前这时会把半截的修改提交掉。
func TestGoexitRollsBack(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uid := time.Now().UnixNano() + 6
	const actId int32 = 60
	defer cleanupRedEnvelope(t, ctx, uid)
	seedRedEnvelope(t, ctx, uid, actId, 1, 0)
	before := ReadStats().Procedures

	var committed, rolledBack bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = WithTx(ctx, func(ctx context.Context) error {
			ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid, actId).SetTodayCount(99)
			dbctx.AfterCommit(ctx, func(context.Context) { committed = true })
			dbctx.AfterRollback(ctx, func(context.Context) { rolledBack = true })
			runtime.Goexit()
			return nil
		})
	}()
	<-done

	if committed || !rolledBack {
		t.Fatalf("Goexit 应该回滚：committed=%v rolledBack=%v", committed, rolledBack)
	}
	if v := selectRedEnvelope(t, ctx, uid, actId); v.TodayCount() != 1 {
		t.Fatalf("Goexit 之后修改不应该落库，实际 today_count=%d", v.TodayCount())
	}
	if after := ReadStats().Procedures; after.Failed != before.Failed+1 || after.Committed != before.Committed {
		t.Fatalf("Goexit 应该记一次 Failed，实际 %+v → %+v", before, after)
	}
}

// TestSQLErrorIsReturned 生成代码里的 SQL 出错时，Procedure 回滚后把错误返回给调用方，而不是 panic。
// 预算到期造成的错误带上 ErrTimeout，同时还是 context.DeadlineExceeded；错误里写着是哪条语句。
func TestSQLErrorIsReturned(t *testing.T) {
	setupTestDB(t)
	before := ReadStats().Procedures

	var err error
	msg := catchPanic(t, func() {
		err = Procedure(t.Context(), ProcedureOptions{Timeout: 50 * time.Millisecond}, func(ctx context.Context) error {
			<-ctx.Done() // 业务太慢，预算到期
			ptable.UserRedEnvelopeTable.GetById(ctx, 1)
			t.Error("查询出错之后不应该继续执行")
			return nil
		})
	})
	if msg != "" {
		t.Fatalf("SQL 错误应该作为返回值，而不是 panic: %s", msg)
	}
	if !errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("预算到期应该返回 ErrTimeout（同时是 DeadlineExceeded），实际: %v", err)
	}
	if !strings.Contains(err.Error(), `SELECT ... FROM "public"."user_red_envelope" WHERE "id" = $1`) {
		t.Fatalf("错误里应该写明是哪条语句，实际: %v", err)
	}
	if after := ReadStats().Procedures; after.TimedOut != before.TimedOut+1 {
		t.Fatalf("应该记一次 TimedOut，实际 %+v → %+v", before, after)
	}
}

// TestCallerCancelIsNotTimeout 调用方自己取消（客户端断开）不是预算到期：返回 context.Canceled，不带 ErrTimeout。
func TestCallerCancelIsNotTimeout(t *testing.T) {
	setupTestDB(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := Procedure(ctx, ProcedureOptions{Timeout: time.Minute}, func(ctx context.Context) error {
		cancel()
		ptable.UserRedEnvelopeTable.GetById(ctx, 1)
		return nil
	})
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrTimeout) {
		t.Fatalf("调用方取消应该返回 context.Canceled、不带 ErrTimeout，实际: %v", err)
	}
}

// TestQueryReturnsSQLError Query 回调里 SQL 出错同样作为返回值；回调自己的 panic 照旧上抛。
func TestQueryReturnsSQLError(t *testing.T) {
	reopen(t, func(c *Config) { c.TransactionTimeout = 50 * time.Millisecond })

	err := Query(t.Context(), func(ctx context.Context) error {
		<-ctx.Done()
		ptable.UserRedEnvelopeTable.SelectById(ctx, 1)
		return nil
	})
	if !errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Query 超过预算应该返回 ErrTimeout，实际: %v", err)
	}

	msg := catchPanic(t, func() {
		_ = Query(t.Context(), func(context.Context) error { panic("boom") })
	})
	if msg != "boom" {
		t.Fatalf("回调自己的 panic 应该原样上抛，实际: %q", msg)
	}
}

// TestOpenGuards 同一时刻只有一个连接池；没打开时开事务、查询返回 ErrNotOpen，而不是空指针。
func TestOpenGuards(t *testing.T) {
	reopen(t, func(*Config) {})

	if err := Init(t.Context(), testSchema); !errors.Is(err, ErrAlreadyOpen) {
		t.Fatalf("已经打开时再 Init 应该返回 ErrAlreadyOpen，实际: %v", err)
	}

	Close()
	Close() // 重复调用安全
	if err := WithTx(t.Context(), func(context.Context) error { return nil }); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("关闭后开事务应该返回 ErrNotOpen，实际: %v", err)
	}
	if err := Query(t.Context(), func(context.Context) error { return nil }); !errors.Is(err, ErrNotOpen) {
		t.Fatalf("关闭后查询应该返回 ErrNotOpen，实际: %v", err)
	}
	if s := ReadStats(); s.Pool != nil {
		t.Fatalf("关闭后 ReadStats 应该是零值，实际 %+v", s)
	}
}

// TestOpenUsesPgxDefaults 只给连接串：其余各项用 pgx 的默认值。以前没写的属性被当成 0 覆盖上去，
// poolHealthCheckPeriod 没写时 pgx 的后台 goroutine 会 panic，整个进程崩掉。
func TestOpenUsesPgxDefaults(t *testing.T) {
	reopen(t, func(c *Config) { *c = Config{URL: c.URL} })

	want, err := pgxpool.ParseConfig(testSchema.URL)
	if err != nil {
		t.Fatal(err)
	}
	got := active.Load().pool.Config()
	if got.MaxConns != want.MaxConns || got.MinConns != want.MinConns ||
		got.MaxConnLifetime != want.MaxConnLifetime || got.MaxConnIdleTime != want.MaxConnIdleTime ||
		got.HealthCheckPeriod != want.HealthCheckPeriod {
		t.Fatalf("没配置的项应该用 pgx 的默认值：\n期望 max=%d min=%d life=%v idle=%v health=%v\n实际 max=%d min=%d life=%v idle=%v health=%v",
			want.MaxConns, want.MinConns, want.MaxConnLifetime, want.MaxConnIdleTime, want.HealthCheckPeriod,
			got.MaxConns, got.MinConns, got.MaxConnLifetime, got.MaxConnIdleTime, got.HealthCheckPeriod)
	}
	if got.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeExec {
		t.Fatalf("应该用 QueryExecModeExec（配合 pgBouncer），实际 %v", got.ConnConfig.DefaultQueryExecMode)
	}
	if err := WithTx(t.Context(), func(context.Context) error { return nil }); err != nil {
		t.Fatalf("用默认配置开事务失败: %v", err)
	}
}

// TestOpenAppliesConfig 非零的配置项覆盖 pgx 的默认值。
func TestOpenAppliesConfig(t *testing.T) {
	reopen(t, func(c *Config) {
		c.MaxConns, c.MinConns = 7, 1
		c.MaxConnLifetime, c.MaxConnIdleTime, c.HealthCheckPeriod = time.Minute, 30*time.Second, 2*time.Second
		c.AppName = "pdbgen-test"
	})

	got := active.Load().pool.Config()
	if got.MaxConns != 7 || got.MinConns != 1 || got.MaxConnLifetime != time.Minute ||
		got.MaxConnIdleTime != 30*time.Second || got.HealthCheckPeriod != 2*time.Second {
		t.Fatalf("配置没有生效: max=%d min=%d life=%v idle=%v health=%v",
			got.MaxConns, got.MinConns, got.MaxConnLifetime, got.MaxConnIdleTime, got.HealthCheckPeriod)
	}
	if name := got.ConnConfig.RuntimeParams["application_name"]; name != "pdbgen-test" {
		t.Fatalf("application_name 应该是 pdbgen-test，实际 %q", name)
	}
}

// TestOpenRejectsBadConfig 负数和互相矛盾的配置直接报错，不打开连接池。
func TestOpenRejectsBadConfig(t *testing.T) {
	reopen(t, func(*Config) {})
	Close()

	url := testSchema.URL
	for _, tc := range []struct {
		cfg  Config
		want string
	}{
		{Config{URL: url, MaxConns: -1}, "MaxConns"},
		{Config{URL: url, CommitTimeout: -time.Second}, "CommitTimeout"},
		{Config{URL: url, MaxConns: 2, MinConns: 3}, "MinConns"},
		{Config{URL: "not a url"}, ""},
	} {
		err := Open(t.Context(), tc.cfg)
		if err == nil {
			Close()
			t.Errorf("%+v 应该报错", tc.cfg)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v 的错误应该提到 %s，实际: %v", tc.cfg, tc.want, err)
		}
		if active.Load() != nil {
			t.Fatalf("配置有误时不应该打开连接池")
		}
	}
}

// TestSchemaConfig pdb.xml 的属性换算成 Config：秒、毫秒；没写的属性是 0，也就是用默认值。
func TestSchemaConfig(t *testing.T) {
	c := SchemaConfig(&readxml.Schema{
		URL: "postgres://x", PoolMaxConns: 9, PoolMaxConnLifetime: 60,
		StatementTimeoutMs: 1500, TransactionTimeoutMs: 5000, CommitTimeoutMs: 2000,
		ShowSQL: true, AppName: "a",
	})
	if c.URL != "postgres://x" || c.MaxConns != 9 || c.MaxConnLifetime != time.Minute ||
		c.StatementTimeout != 1500*time.Millisecond || c.TransactionTimeout != 5*time.Second ||
		c.CommitTimeout != 2*time.Second || !c.ShowSQL || c.AppName != "a" {
		t.Fatalf("换算不对: %+v", c)
	}
	if c.HealthCheckPeriod != 0 || c.MinConns != 0 || c.LockTimeout != 0 {
		t.Fatalf("没写的属性应该是 0（用默认值），实际 %+v", c)
	}
}

// hangingTx 的 COMMIT 永远等不到结果，模拟网络半断。
type hangingTx struct{ pgx.Tx }

func (hangingTx) Commit(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestCommitIsBounded COMMIT 不受调用方取消和预算的影响，但最多等 CommitTimeout。
func TestCommitIsBounded(t *testing.T) {
	d := &db{commitTimeout: 100 * time.Millisecond}

	start := time.Now()
	if err := d.commit(t.Context(), hangingTx{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("等不到结果时应该在 CommitTimeout 后返回，实际: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("COMMIT 等了 %v，应该在 CommitTimeout 左右返回", elapsed)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start = time.Now()
	_ = d.commit(ctx, hangingTx{})
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("调用方的 ctx 取消了，COMMIT 仍然应该等结果，实际只等了 %v", elapsed)
	}
}

// TestStats 每次 Procedure 按结果记一项。
func TestStats(t *testing.T) {
	reopen(t, func(*Config) {}) // 新的连接池，计数从 0 开始

	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(oldLogger)

	ctx := t.Context()
	ok := func(context.Context) error { return nil }
	_ = WithTx(ctx, ok)
	_ = WithTx(ctx, ok)
	_ = WithTx(ctx, func(context.Context) error { return errors.New("business failed") })
	catchPanic(t, func() { _ = WithTx(ctx, func(context.Context) error { panic("boom") }) })
	_ = Procedure(ctx, ProcedureOptions{Timeout: 20 * time.Millisecond}, func(ctx context.Context) error {
		<-ctx.Done()
		ptable.UserRedEnvelopeTable.GetById(ctx, 1)
		return nil
	})

	// try 锁冲突：另一个 Procedure 拿着锁
	key := time.Now().UnixNano()
	held, release := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		_ = Procedure(ctx, ProcedureOptions{Lock: AdvisoryLock(key)}, func(context.Context) error {
			close(held)
			<-release
			return nil
		})
	})
	<-held
	_ = Procedure(ctx, ProcedureOptions{Lock: TryAdvisoryLock(key)}, ok)
	close(release)
	wg.Wait()

	s := ReadStats()
	want := ProcedureStats{Committed: 3, Failed: 1, Panicked: 1, TimedOut: 1, Busy: 1}
	if s.Procedures != want {
		t.Fatalf("计数不对\n期望 %+v\n实际 %+v", want, s.Procedures)
	}
	if s.Pool == nil || s.Pool.MaxConns() <= 0 {
		t.Fatalf("应该带上连接池的统计，实际 %+v", s.Pool)
	}
}

// TestSetLoggerIsConcurrencySafe 请求进行当中换日志落点（go test -race 下检查）。
func TestSetLoggerIsConcurrencySafe(t *testing.T) {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	old := sqlLog.Load()
	defer SetLogger(old)

	var wg sync.WaitGroup
	wg.Go(func() {
		for range 100 {
			SetLogger(discard)
		}
	})
	for range 100 {
		logSQL(t.Context(), time.Now(), "SELECT 1", nil, nil)
	}
	wg.Wait()
}
