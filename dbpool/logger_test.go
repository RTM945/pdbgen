package dbpool

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

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
	oldLog := sqlLog
	sqlLog = slog.New(slog.NewTextHandler(&buf, nil))
	t.Cleanup(func() { sqlLog = oldLog })

	row := &loggingRow{row: rowResult{err: pgx.ErrNoRows}, sql: "SELECT 1"}
	if err := row.Scan(); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("调用方仍然应该拿到 ErrNoRows，实际: %v", err)
	}
	if log := buf.String(); !strings.Contains(log, "level=INFO") {
		t.Fatalf("查不到应该记 INFO，实际:\n%s", log)
	}
}
