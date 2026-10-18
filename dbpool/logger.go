package dbpool

import (
	"context"
	"fmt"
	"pdbgen/dbctx"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type loggingDB struct {
	dbctx.DB
}

func (o *loggingDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	start := time.Now()

	tag, err := o.DB.Exec(ctx, sql, args...)

	logSQL(start, sql, args, err)

	return tag, err
}

func (o *loggingDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	start := time.Now()

	rows, err := o.DB.Query(ctx, sql, args...)

	logSQL(start, sql, args, err)

	return rows, err
}

func (o *loggingDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	start := time.Now()

	row := o.DB.QueryRow(ctx, sql, args...)

	return &loggingRow{
		row:   row,
		start: start,
		sql:   sql,
		args:  args,
	}
}

// wrapLogger 只在 showSQL 打开时才包这一层。包上之后每条 SQL 都会记一条
// message 是 "sql" 的日志：成功 Info，失败 Error（err 作为属性带上）。
func wrapLogger(db dbctx.DB) dbctx.DB {
	if !showSQL {
		return db
	}
	return &loggingDB{DB: db}
}

func logSQL(start time.Time, sql string, args []any, err error) {
	stmt := formatSQL(sql, args)
	cost := time.Since(start)

	// 用 []string 而不是裸 string：项目的 logging handler 会把 []string
	// 按行原样展开，不再给整句加引号、也不转义里面标识符的双引号。
	// 这样日志里的 "user" 这类带引号的表名可以直接复制去 psql 跑。
	if err != nil {
		log().Error("sql", "cost", cost, "stmt", []string{stmt}, "err", err)
		return
	}
	log().Info("sql", "cost", cost, "stmt", []string{stmt})
}

// logBatch 一次往返里排队的多条语句。它们是一个整体，所以记成一条而不是 N 条，
// 否则日志里看不出"这几条是一起发的"。
func logBatch(start time.Time, batch *pgx.Batch, err error) {
	if !showSQL {
		return
	}

	stmts := make([]string, 0, len(batch.QueuedQueries))
	for _, q := range batch.QueuedQueries {
		stmts = append(stmts, formatSQL(q.SQL, q.Arguments))
	}

	cost := time.Since(start)
	if err != nil {
		log().Error("sql batch", "cost", cost, "count", len(stmts), "stmts", stmts, "err", err)
		return
	}
	log().Info("sql batch", "cost", cost, "count", len(stmts), "stmts", stmts)
}

var placeholderRE = regexp.MustCompile(`\$([1-9][0-9]*)`)

// formatSQL 把 $1 这类占位符换回实际值，日志里的语句就能直接粘去 psql 跑。
func formatSQL(sql string, args []any) string {
	sql = placeholderRE.ReplaceAllStringFunc(sql, func(s string) string {
		n, _ := strconv.Atoi(s[1:])
		return fmt.Sprintf("%%[%d]s", n)
	})

	values := make([]any, len(args))
	for i, arg := range args {
		values[i] = sqlLiteral(arg)
	}

	sql = fmt.Sprintf(sql, values...)
	sql = strings.TrimSpace(sql)
	if !strings.HasSuffix(sql, ";") {
		sql += ";"
	}
	return sql
}

func sqlLiteral(v any) string {
	switch x := v.(type) {
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'"
	default:
		return fmt.Sprintf("%v", x)
	}
}

// loggingRow 拖到 Scan 才记：只调 QueryRow 不 Scan 的话语句根本没发出去。
type loggingRow struct {
	row    pgx.Row
	start  time.Time
	sql    string
	args   []any
	logged bool
}

func (r *loggingRow) Scan(dest ...any) error {
	err := r.row.Scan(dest...)

	if !r.logged {
		r.logged = true
		logSQL(r.start, r.sql, r.args, err)
	}

	return err
}
