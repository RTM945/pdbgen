package dbpool

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"pdbgen/dbctx"
	"pdbgen/ptable"
)

// TestRunHooksInOrder 钩子按注册顺序执行。
func TestRunHooksInOrder(t *testing.T) {
	txn := newTransaction(nil)
	var order []string
	txn.AfterCommit(func(context.Context) { order = append(order, "a") })
	txn.AfterCommit(func(context.Context) { order = append(order, "b") })

	runHooks(context.Background(), "after-commit", txn.afterCommit)

	if !reflect.DeepEqual(order, []string{"a", "b"}) {
		t.Fatalf("钩子应按注册顺序执行，实际: %v", order)
	}
}

// TestHookPanicDoesNotStopLaterHooks 单个钩子 panic 不影响后续钩子。
func TestHookPanicDoesNotStopLaterHooks(t *testing.T) {
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(oldLogger)

	var secondRan bool
	runHooks(context.Background(), "after-commit", []func(context.Context){
		func(context.Context) { panic("boom") },
		func(context.Context) { secondRan = true },
	})

	if !secondRan {
		t.Fatal("前一个钩子 panic 后，后续钩子仍应执行")
	}
}

func insertRedEnvelope(ctx context.Context, uid int64, actId int32) {
	v := ptable.NewRedEnvelope()
	v.SetUid(uid)
	v.SetActId(actId)
	v.SetTodayCount(1)
	ptable.UserRedEnvelopeTable.Insert(ctx, v)
}

func cleanupRedEnvelope(t *testing.T, ctx context.Context, uid int64) {
	t.Helper()

	if err := WithTx(ctx, func(ctx context.Context) error {
		_, err := dbctx.SQLTxFromCtx(ctx).Exec(
			ctx, `DELETE FROM user_red_envelope WHERE uid = $1`, uid,
		)
		return err
	}); err != nil {
		t.Errorf("清理 user_red_envelope uid=%d 失败: %v", uid, err)
	}
}

func countRedEnvelope(ctx context.Context, uid int64) (int, error) {
	var n int
	err := Query(ctx, func(ctx context.Context) error {
		return dbctx.QuerierFromCtx(ctx).QueryRow(
			ctx, `SELECT count(*) FROM user_red_envelope WHERE uid = $1`, uid,
		).Scan(&n)
	})
	return n, err
}

// TestAfterCommitRunsAfterCommit 提交后钩子必须看到已经提交的数据，
// 并且拿到的 ctx 里没有事务本身。
func TestAfterCommitRunsAfterCommit(t *testing.T) {
	setupTestDB(t)

	type ctxKey struct{}
	ctx := context.WithValue(t.Context(), ctxKey{}, "request-value")
	uid := time.Now().UnixNano()
	const actId int32 = 10
	defer cleanupRedEnvelope(t, ctx, uid)

	var (
		commitRan   bool
		rollbackRan bool
		hookErr     error
		hookSawRow  bool
		hookCtxOK   bool
		hookGotVal  bool
	)

	if err := WithTx(ctx, func(ctx context.Context) error {
		insertRedEnvelope(ctx, uid, actId)

		dbctx.AfterCommit(ctx, func(hookCtx context.Context) {
			commitRan = true

			hookCtxOK = !dbctx.InTransaction(hookCtx)
			hookGotVal = hookCtx.Value(ctxKey{}) == "request-value"

			n, err := countRedEnvelope(hookCtx, uid)
			if err != nil {
				hookErr = err
				return
			}
			hookSawRow = n == 1
		})
		dbctx.AfterRollback(ctx, func(context.Context) {
			rollbackRan = true
		})
		return nil
	}); err != nil {
		t.Fatalf("WithTx 失败: %v", err)
	}

	if !commitRan {
		t.Fatal("提交成功后 after-commit 钩子应该执行")
	}
	if rollbackRan {
		t.Fatal("提交成功后 after-rollback 钩子不应该执行")
	}
	if hookErr != nil {
		t.Fatalf("钩子里查库失败: %v", hookErr)
	}
	if !hookSawRow {
		t.Fatal("after-commit 钩子应该能看到已经提交的行")
	}
	if !hookCtxOK {
		t.Fatal("after-commit 钩子的 ctx 不应该带 SQLTx/Transaction")
	}
	if !hookGotVal {
		t.Fatal("after-commit 钩子的 ctx 应该保留调用方的值")
	}
}

// TestAfterRollbackRunsOnError 业务返回错误时：回滚钩子执行，提交钩子不执行，
// 事务里的 INSERT 被回滚。
func TestAfterRollbackRunsOnError(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uid := time.Now().UnixNano()
	const actId int32 = 11
	defer cleanupRedEnvelope(t, ctx, uid)

	sentinel := errors.New("business failed")
	var commitRan, rollbackRan bool

	err := WithTx(ctx, func(ctx context.Context) error {
		insertRedEnvelope(ctx, uid, actId)
		dbctx.AfterCommit(ctx, func(context.Context) { commitRan = true })
		dbctx.AfterRollback(ctx, func(context.Context) { rollbackRan = true })
		return sentinel
	})

	if !errors.Is(err, sentinel) {
		t.Fatalf("期望业务错误原样返回，实际: %v", err)
	}
	if commitRan {
		t.Fatal("回滚后 after-commit 钩子不应该执行")
	}
	if !rollbackRan {
		t.Fatal("回滚后 after-rollback 钩子应该执行")
	}
	if n, err := countRedEnvelope(ctx, uid); err != nil || n != 0 {
		t.Fatalf("事务应该回滚，期望 0 行，实际 n=%d err=%v", n, err)
	}
}

// TestAfterRollbackRunsOnPanic panic 路径也要先回滚再执行回滚钩子，
// 最后把原始 panic 原样抛出去。
func TestAfterRollbackRunsOnPanic(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uid := time.Now().UnixNano()
	const actId int32 = 12
	defer cleanupRedEnvelope(t, ctx, uid)

	var commitRan, rollbackRan bool
	msg := catchPanic(t, func() {
		_ = WithTx(ctx, func(ctx context.Context) error {
			insertRedEnvelope(ctx, uid, actId)
			dbctx.AfterCommit(ctx, func(context.Context) { commitRan = true })
			dbctx.AfterRollback(ctx, func(context.Context) { rollbackRan = true })
			panic("boom")
		})
	})

	if msg != "boom" {
		t.Fatalf("原始 panic 应该原样抛出，实际: %q", msg)
	}
	if commitRan {
		t.Fatal("panic 回滚后 after-commit 钩子不应该执行")
	}
	if !rollbackRan {
		t.Fatal("panic 回滚后 after-rollback 钩子应该执行")
	}
	if n, err := countRedEnvelope(ctx, uid); err != nil || n != 0 {
		t.Fatalf("事务应该回滚，期望 0 行，实际 n=%d err=%v", n, err)
	}
}

// TestAfterCommitHookPanicDoesNotAffectCommit 单个钩子 panic 只记日志，
// 后续钩子继续执行，已经提交的数据不受影响。
func TestAfterCommitHookPanicDoesNotAffectCommit(t *testing.T) {
	setupTestDB(t)

	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(oldLogger)

	ctx := t.Context()
	uid := time.Now().UnixNano()
	const actId int32 = 13
	defer cleanupRedEnvelope(t, ctx, uid)

	var secondRan bool
	err := WithTx(ctx, func(ctx context.Context) error {
		insertRedEnvelope(ctx, uid, actId)
		dbctx.AfterCommit(ctx, func(context.Context) { panic("hook boom") })
		dbctx.AfterCommit(ctx, func(context.Context) { secondRan = true })
		return nil
	})

	if err != nil {
		t.Fatalf("钩子 panic 不应该影响已经成功的提交: %v", err)
	}
	if !secondRan {
		t.Fatal("前一个钩子 panic 后，后续钩子仍应执行")
	}
	if n, err := countRedEnvelope(ctx, uid); err != nil || n != 1 {
		t.Fatalf("数据应该已经提交，期望 1 行，实际 n=%d err=%v", n, err)
	}
}

// TestAfterCommitHooksRunInRegistrationOrder 钩子按注册顺序执行。
func TestAfterCommitHooksRunInRegistrationOrder(t *testing.T) {
	setupTestDB(t)

	var order []string
	if err := WithTx(t.Context(), func(ctx context.Context) error {
		dbctx.AfterCommit(ctx, func(context.Context) { order = append(order, "first") })
		dbctx.AfterCommit(ctx, func(context.Context) { order = append(order, "second") })
		return nil
	}); err != nil {
		t.Fatalf("WithTx 失败: %v", err)
	}

	if !reflect.DeepEqual(order, []string{"first", "second"}) {
		t.Fatalf("钩子应该按注册顺序执行，实际: %v", order)
	}
}
