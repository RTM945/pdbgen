package dbpool

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestFormatSQL(t *testing.T) {
	for _, tc := range []struct {
		sql  string
		args []any
		want string
	}{
		// SQL 里的 % 不是格式动词
		{`SELECT 1 WHERE "name" LIKE 'abc%' AND "id" = $1`, []any{int64(42)}, `SELECT 1 WHERE "name" LIKE 'abc%' AND "id" = 42;`},
		{`SELECT "id" % 10 = $1`, []any{3}, `SELECT "id" % 10 = 3;`},
		{`UPDATE "t" SET "a" = $1, "b" = $2 WHERE "id" = $3`, []any{nil, "it's", int64(7)}, `UPDATE "t" SET "a" = NULL, "b" = 'it''s' WHERE "id" = 7;`},
		{`SELECT $1, $10, $3`, []any{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, `SELECT 1, 10, 3;`},
		{`SELECT $1, $2`, []any{true}, `SELECT true, $2;`},
	} {
		if got := formatSQL(tc.sql, tc.args); got != tc.want {
			t.Errorf("formatSQL(%q)\n期望 %s\n实际 %s", tc.sql, tc.want, got)
		}
	}
}

type rowResult struct{ err error }

func (r rowResult) Scan(...any) error { return r.err }

// TestNoRowsIsNotLoggedAsError 查不到是正常结果：记 INFO，而不是 ERROR。
func TestNoRowsIsNotLoggedAsError(t *testing.T) {
	var buf bytes.Buffer
	useLogger(t, slog.New(slog.NewTextHandler(&buf, nil)))

	row := &loggingRow{row: rowResult{err: pgx.ErrNoRows}, ctx: t.Context(), sql: "SELECT 1"}
	if err := row.Scan(); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("调用方仍然应该拿到 ErrNoRows，实际: %v", err)
	}
	if log := buf.String(); !strings.Contains(log, "level=INFO") {
		t.Fatalf("查不到应该记 INFO，实际:\n%s", log)
	}
}

// useLogger 在这个测试里把 pdbgen 的日志换成 l，结束时换回去。
func useLogger(t *testing.T, l *slog.Logger) {
	old := sqlLog.Load()
	SetLogger(l)
	t.Cleanup(func() { SetLogger(old) })
}

// TestLogSkipsDisabledLevel 级别关掉时不格式化语句：showSQL 打开、Info 却不输出时，不该白付格式化的开销。
func TestLogSkipsDisabledLevel(t *testing.T) {
	var buf bytes.Buffer
	useLogger(t, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))

	args := []any{int64(1)}
	if n := testing.AllocsPerRun(100, func() { logSQL(t.Context(), time.Now(), "SELECT $1", args, nil) }); n != 0 {
		t.Fatalf("Info 关掉时记一条 SQL 日志不应该分配，实际 %.0f 次", n)
	}
	logSQL(t.Context(), time.Now(), "SELECT $1", args, errors.New("boom"))
	if log := buf.String(); !strings.Contains(log, "level=ERROR") || !strings.Contains(log, "SELECT 1;") {
		t.Fatalf("失败的语句照样要记 Error，实际:\n%s", log)
	}
}
