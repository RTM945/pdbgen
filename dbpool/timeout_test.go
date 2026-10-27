package dbpool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"pdbgen/dbctx"
)

func TestBuildBeginSQL(t *testing.T) {
	if got := buildBeginSQL(0, 0, 0); got != "BEGIN" {
		t.Fatalf("没配超时时只有 BEGIN，实际 %q", got)
	}
	want := "BEGIN; SET LOCAL statement_timeout = 5000; SET LOCAL lock_timeout = 1000"
	if got := buildBeginSQL(5000, 0, 1000); got != want {
		t.Fatalf("期望 %q，实际 %q", want, got)
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
				return dbctx.SQLTxFromCtx(ctx).QueryRow(ctx, `SELECT
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
	setupTestDB(t)

	old := beginSQL
	beginSQL = buildBeginSQL(50, 0, 0)
	t.Cleanup(func() { beginSQL = old })

	err := WithTx(t.Context(), func(ctx context.Context) error {
		_, err := dbctx.SQLTxFromCtx(ctx).Exec(ctx, "SELECT pg_sleep(1)")
		return err
	})
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != "57014" {
		t.Fatalf("期望 statement_timeout 取消语句（57014），实际: %v", err)
	}
}

// TestQueryHasBudget dbpool.Query 的回调也受 transactionTimeoutMs 的预算限制。
func TestQueryHasBudget(t *testing.T) {
	old := budget
	budget = time.Second
	t.Cleanup(func() { budget = old })

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
