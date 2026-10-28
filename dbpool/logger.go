package dbpool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"pdbgen/dbctx"
)

// SetLogger 换掉 SQL 日志的落点，不设就用 slog.Default()。
//
// nil 表示回落到 slog.Default()，而且是**调用时**才解析——所以应用在
// dbpool.Init 之后再 slog.SetDefault 也来得及。
func SetLogger(l *slog.Logger) { sqlLog = l }

// sqlLog 见 SetLogger。只由 SetLogger 写，读的时候统一走 log()。
var sqlLog *slog.Logger

func log() *slog.Logger {
	if sqlLog != nil {
		return sqlLog
	}
	return slog.Default()
}

type loggingSQLTx struct {
	dbctx.SQLTx
}

func (o *loggingSQLTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	start := time.Now()
	tag, err := o.SQLTx.Exec(ctx, sql, args...)
	logSQL(start, sql, args, err)
	return tag, err
}

func (o *loggingSQLTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	start := time.Now()
	rows, err := o.SQLTx.Query(ctx, sql, args...)
	logSQL(start, sql, args, err)
	return rows, err
}

func (o *loggingSQLTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	start := time.Now()
	return &loggingRow{row: o.SQLTx.QueryRow(ctx, sql, args...), start: start, sql: sql, args: args}
}

// wrapLogger 只在 showSQL 打开时才包这一层。包上之后每条 SQL 都会记一条
// message 是 "sql" 的日志：成功 Info，失败 Error（err 作为属性带上）。
func wrapLogger(tx dbctx.SQLTx) dbctx.SQLTx {
	if !showSQL {
		return tx
	}
	return &loggingSQLTx{SQLTx: tx}
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

var placeholderRE = regexp.MustCompile(`\$[1-9][0-9]*`)

// formatSQL 把 $1 这类占位符换成参数的字面量，日志里的语句可以直接粘去 psql 跑。
// 直接替换文本，不经过 fmt 的格式串，SQL 里的 % 原样保留。
func formatSQL(sql string, args []any) string {
	sql = placeholderRE.ReplaceAllStringFunc(sql, func(p string) string {
		if n, _ := strconv.Atoi(p[1:]); n <= len(args) {
			return sqlLiteral(args[n-1])
		}
		return p
	})
	sql = strings.TrimSpace(sql)
	if !strings.HasSuffix(sql, ";") {
		sql += ";"
	}
	return sql
}

// sqlLiteral 把参数写成 SQL 字面量。
func sqlLiteral(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'"
	default:
		return fmt.Sprint(x)
	}
}

// loggingRow 拖到 Scan 才记：错误要到 Scan 才拿得到。
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
		logErr := err
		if errors.Is(err, pgx.ErrNoRows) {
			logErr = nil // 查不到是正常结果（GetByXxx 返回 nil），不是 SQL 错误
		}
		logSQL(r.start, r.sql, r.args, logErr)
	}
	return err
}
