package dbpool

import (
	"context"
	"errors"
	"strings"
	"testing"

	"pdbgen/dbctx"
	"pdbgen/ptable"
)

// TestSecondHandle New 出来的句柄和默认句柄各用各的连接池；生成的代码在别的库的事务、查询里直接 panic，
// 不把 SQL 发到那个库上。两个句柄连的其实是同一个库，只是 Name 不同，正好说明检查只看 Name。
func TestSecondHandle(t *testing.T) {
	setupTestDB(t)

	cfg := SchemaConfig(testSchema)
	cfg.Name = "other"
	other, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer other.Close()

	if err := other.WithTx(t.Context(), func(ctx context.Context) error {
		return dbctx.SQLTxFromCtx(ctx, "other").QueryRow(ctx, "SELECT 1").Scan(new(int))
	}); err != nil {
		t.Fatalf("新句柄的事务: %v", err)
	}
	if err := other.Query(t.Context(), func(ctx context.Context) error {
		return dbctx.QuerierFromCtx(ctx, "other").QueryRow(ctx, "SELECT 1").Scan(new(int))
	}); err != nil {
		t.Fatalf("新句柄的查询: %v", err)
	}

	const want = `开在库 "other" 上，要操作的表在库 ""`
	if got := catchPanic(t, func() {
		_ = other.WithTx(t.Context(), func(ctx context.Context) error {
			ptable.UserMailTable.GetById(ctx, 1)
			return nil
		})
	}); !strings.Contains(got, want) {
		t.Fatalf("别的库的事务里查表应该 panic（包含 %q），实际: %q", want, got)
	}
	if got := catchPanic(t, func() {
		_ = other.Query(t.Context(), func(ctx context.Context) error {
			ptable.UserMailTable.SelectById(ctx, 1)
			return nil
		})
	}); !strings.Contains(got, want) {
		t.Fatalf("别的库的查询里查表应该 panic（包含 %q），实际: %q", want, got)
	}

	// 默认句柄的事务里开别的库的事务，照旧是嵌套
	if err := WithTx(t.Context(), func(ctx context.Context) error {
		return other.WithTx(ctx, func(context.Context) error { return nil })
	}); !errors.Is(err, ErrNestedTransaction) {
		t.Fatalf("跨库嵌套应该返回 ErrNestedTransaction，实际: %v", err)
	}
}
