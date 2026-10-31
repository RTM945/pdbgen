package dbpool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"pdbgen/dbctx"
)

// SetLogger 换掉 pdbgen 日志（SQL 日志、钩子 panic）的落点，不设就用 slog.Default()。
// 任何时候都可以调用，包括 Open 之后、请求进行当中。
//
// nil 表示回落到 slog.Default()，而且是**调用时**才解析——所以应用之后再 slog.SetDefault 也来得及。
func SetLogger(l *slog.Logger) { sqlLog.Store(l) }

// sqlLog 见 SetLogger。SetLogger 随时可能写，每条日志都不加锁地读，所以是 atomic。
var sqlLog atomic.Pointer[slog.Logger]

func log() *slog.Logger {
	if l := sqlLog.Load(); l != nil {
		return l
	}
	return slog.Default()
}

type loggingSQLTx struct {
	dbctx.SQLTx
}

func (o *loggingSQLTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	start := time.Now()
	tag, err := o.SQLTx.Exec(ctx, sql, args...)
	logSQL(ctx, start, sql, args, err)
	return tag, err
}

func (o *loggingSQLTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	start := time.Now()
	rows, err := o.SQLTx.Query(ctx, sql, args...)
	logSQL(ctx, start, sql, args, err)
	return rows, err
}

func (o *loggingSQLTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	start := time.Now()
	return &loggingRow{row: o.SQLTx.QueryRow(ctx, sql, args...), ctx: ctx, start: start, sql: sql, args: args}
}

// wrap 只在 ShowSQL 打开时才包这一层。包上之后每条 SQL 都会记一条
// message 是 "sql" 的日志：成功 Info，失败 Error（err 作为属性带上）。
func (d *db) wrap(tx dbctx.SQLTx) dbctx.SQLTx {
	if !d.showSQL {
		return tx
	}
	return &loggingSQLTx{SQLTx: tx}
}

// logSQL 记一条语句。先看这个级别会不会输出，不会就不格式化：格式化一条语句要几百纳秒、好几次分配。
func logSQL(ctx context.Context, start time.Time, sql string, args []any, err error) {
	cost := time.Since(start)
	level := slog.LevelInfo
	if err != nil {
		level = slog.LevelError
	}
	l := log()
	if !l.Enabled(ctx, level) {
		return
	}

	// 用 []string 而不是裸 string：项目的 logging handler 会把 []string
	// 按行原样展开，不再给整句加引号、也不转义里面标识符的双引号。
	// 这样日志里的 "user" 这类带引号的表名可以直接复制去 psql 跑。
	attrs := []slog.Attr{slog.Duration("cost", cost), slog.Any("stmt", []string{formatSQL(sql, args)})}
	if err != nil {
		attrs = append(attrs, slog.Any("err", err))
	}
	l.LogAttrs(ctx, level, "sql", attrs...)
}

// logBatch 一次往返里排队的多条语句。它们是一个整体，所以记成一条而不是 N 条，
// 否则日志里看不出"这几条是一起发的"。
func logBatch(ctx context.Context, start time.Time, batch *pgx.Batch, err error) {
	cost := time.Since(start)
	level := slog.LevelInfo
	if err != nil {
		level = slog.LevelError
	}
	l := log()
	if !l.Enabled(ctx, level) {
		return
	}

	stmts := make([]string, 0, len(batch.QueuedQueries))
	for _, q := range batch.QueuedQueries {
		stmts = append(stmts, formatSQL(q.SQL, q.Arguments))
	}
	attrs := []slog.Attr{slog.Duration("cost", cost), slog.Int("count", len(stmts)), slog.Any("stmts", stmts)}
	if err != nil {
		attrs = append(attrs, slog.Any("err", err))
	}
	l.LogAttrs(ctx, level, "sql batch", attrs...)
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
	ctx    context.Context
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
		logSQL(r.ctx, r.start, r.sql, r.args, logErr)
	}
	return err
}
