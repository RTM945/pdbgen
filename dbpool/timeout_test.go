package dbpool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"pdbgen/dbctx"
	"pdbgen/ptable"
)

func TestBuildBeginSQL(t *testing.T) {
	if got := buildBeginSQL(0, 0, 0, 0); got != "BEGIN" {
		t.Fatalf("没配超时时只有 BEGIN，实际 %q", got)
	}
	want := "BEGIN; SET LOCAL statement_timeout = 5000; SET LOCAL lock_timeout = 1000"
	if got := buildBeginSQL(5*time.Second, 0, time.Second, 0); got != want {
		t.Fatalf("期望 %q，实际 %q", want, got)
	}
	// 不足 1ms 的超时向上取整：PostgreSQL 里 0 表示不限
	want = "BEGIN; SET LOCAL idle_in_transaction_session_timeout = 1"
	if got := buildBeginSQL(0, 300*time.Microsecond, 0, 0); got != want {
		t.Fatalf("期望 %q，实际 %q", want, got)
	}
	want = "BEGIN; SET LOCAL transaction_timeout = 8000"
	if got := buildBeginSQL(0, 0, 0, 8*time.Second); got != want {
		t.Fatalf("期望 %q，实际 %q", want, got)
	}
}

// TestServerTransactionTimeoutMustExceedBudget 服务端的 transaction_timeout 不比客户端预算长时 Open 报错：
// 否则服务端先断开会话，调用方拿到的是断连而不是 ErrTimeout。
func TestServerTransactionTimeoutMustExceedBudget(t *testing.T) {
	for _, tc := range []struct {
		budget, server time.Duration
		ok             bool
	}{
		{0, 0, true}, {5 * time.Second, 0, true}, {0, 5 * time.Second, true},
		{5 * time.Second, 8 * time.Second, true},
		{5 * time.Second, 5 * time.Second, false}, {5 * time.Second, time.Second, false},
		{0, -1, false},
	} {
		_, err := Config{URL: "postgres://x@127.0.0.1/x", TransactionTimeout: tc.budget, ServerTransactionTimeout: tc.server}.poolConfig()
		if (err == nil) != tc.ok {
			t.Errorf("TransactionTimeout=%v ServerTransactionTimeout=%v：期望 ok=%v，实际 err=%v", tc.budget, tc.server, tc.ok, err)
		}
	}
}

// TestServerTransactionTimeoutEndsSlowTransaction 每条语句都不慢、加起来超过 transaction_timeout 的事务，
// 服务端会断开会话。要 PG 17+，更早的库跳过。
func TestServerTransactionTimeoutEndsSlowTransaction(t *testing.T) {
	setupTestDB(t)
	var version int
	if err := Query(t.Context(), func(ctx context.Context) error {
		return dbctx.ReaderFromCtx(ctx, ptable.DBName).QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&version)
	}); err != nil {
		t.Fatalf("查版本失败: %v", err)
	}
	if version < 170000 {
		t.Skipf("transaction_timeout 要 PG 17+，测试库是 %d", version)
	}
	reopen(t, func(c *Config) {
		c.StatementTimeout = time.Second
		c.TransactionTimeout = 0 // 只看服务端
		c.ServerTransactionTimeout = 300 * time.Millisecond
	})

	start := time.Now()
	err := WithTx(t.Context(), func(ctx context.Context) error {
		for range 10 {
			if _, err := dbctx.SQLTxFromCtx(ctx, ptable.DBName).Exec(ctx, "SELECT pg_sleep(0.1)"); err != nil {
				return err
			}
		}
		return nil
	})
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != "25P04" {
		t.Fatalf("期望 transaction_timeout 断开会话（25P04），实际: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 800*time.Millisecond {
		t.Fatalf("应该在 300ms 左右结束，实际 %v", elapsed)
	}
}

// TestTimeoutsApplyToEveryTransaction pdb.xml 里的三个超时对所有事务生效，不管加不加锁。
func TestTimeoutsApplyToEveryTransaction(t *testing.T) {
	setupTestDB(t)

	key := time.Now().UnixNano()
	for name, lock := range map[string]LockSpec{
		"不加锁":       {},
		"try 锁":     TryAdvisoryLock(key),
		"双 int32 锁": AdvisoryLock2(1, int32(key)),
	} {
		t.Run(name, func(t *testing.T) {
			var got [3]int
			err := Procedure(t.Context(), ProcedureOptions{Lock: lock}, func(ctx context.Context) error {
				return dbctx.SQLTxFromCtx(ctx, ptable.DBName).QueryRow(ctx, `SELECT
					(SELECT setting::int FROM pg_settings WHERE name = 'statement_timeout'),
					(SELECT setting::int FROM pg_settings WHERE name = 'idle_in_transaction_session_timeout'),
					(SELECT setting::int FROM pg_settings WHERE name = 'lock_timeout')`,
				).Scan(&got[0], &got[1], &got[2])
			})
			if err != nil {
				t.Fatalf("事务失败: %v", err)
			}
			want := [3]int{testSchema.StatementTimeoutMs, testSchema.IdleInTransactionSessionTimeoutMs, testSchema.LockTimeoutMs}
			for i := range want {
				if want[i] > 0 && got[i] != want[i] {
					t.Fatalf("事务里的超时应该是 pdb.xml 的配置 %v，实际 %v", want, got)
				}
			}
		})
	}
}

// TestStatementTimeoutAppliesWithoutLock 不加锁的事务里，慢语句同样会被服务端的 statement_timeout 取消。
func TestStatementTimeoutAppliesWithoutLock(t *testing.T) {
	reopen(t, func(c *Config) { c.StatementTimeout = 50 * time.Millisecond })

	err := WithTx(t.Context(), func(ctx context.Context) error {
		_, err := dbctx.SQLTxFromCtx(ctx, ptable.DBName).Exec(ctx, "SELECT pg_sleep(1)")
		return err
	})
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != "57014" {
		t.Fatalf("期望 statement_timeout 取消语句（57014），实际: %v", err)
	}
}

// TestQueryHasBudget dbpool.Query 的回调也受 transactionTimeoutMs 的预算限制。
func TestQueryHasBudget(t *testing.T) {
	reopen(t, func(c *Config) { c.TransactionTimeout = time.Second })

	var deadline time.Time
	var ok bool
	if err := Query(t.Context(), func(ctx context.Context) error {
		deadline, ok = ctx.Deadline()
		return nil
	}); err != nil {
		t.Fatalf("Query 失败: %v", err)
	}
	if !ok || time.Until(deadline) > time.Second {
		t.Fatalf("Query 的 ctx 应该带 1s 以内的 deadline，实际 ok=%v deadline=%v", ok, deadline)
	}
}

func TestEarliest(t *testing.T) {
	for _, tc := range []struct{ a, b, want time.Duration }{
		{0, 0, 0}, {time.Second, 0, time.Second}, {0, time.Second, time.Second},
		{time.Second, 2 * time.Second, time.Second}, {3 * time.Second, 2 * time.Second, 2 * time.Second},
		{-1, time.Second, time.Second},
	} {
		if got := earliest(tc.a, tc.b); got != tc.want {
			t.Errorf("earliest(%v, %v) = %v，期望 %v", tc.a, tc.b, got, tc.want)
		}
	}
}
