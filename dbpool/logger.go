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
	logger logger
}

func (o *loggingDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	start := time.Now()

	tag, err := o.DB.Exec(ctx, sql, args...)

	o.log(start, sql, args, err)

	return tag, err
}

func (o *loggingDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	start := time.Now()

	rows, err := o.DB.Query(ctx, sql, args...)

	o.log(start, sql, args, err)

	return rows, err
}

func (o *loggingDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	start := time.Now()

	row := o.DB.QueryRow(ctx, sql, args...)

	return &loggingRow{
		row:    row,
		start:  start,
		sql:    sql,
		args:   args,
		logger: o.logger,
	}
}

func (o *loggingDB) log(start time.Time, sql string, args []any, err error) {
	if o.logger == nil {
		return
	}

	elapsed := time.Since(start)

	o.logger.Println(
		"COST", elapsed,
		"SQL", formatSQL(sql, args),
	)
	if err != nil {
		o.logger.Println(err)
	}
}

func wrapLogger(db dbctx.DB) dbctx.DB {
	if !showSQL || sqlLogger == nil {
		return db
	}

	return &loggingDB{
		DB:     db,
		logger: sqlLogger,
	}
}

func logBatch(start time.Time, batch *pgx.Batch, err error) {
	if !showSQL || sqlLogger == nil {
		return
	}
	sqls := make([]string, 0, len(batch.QueuedQueries))
	for _, q := range batch.QueuedQueries {
		sqls = append(sqls, formatSQL(q.SQL, q.Arguments))
	}
	sqlLogger.Printf(
		"SQL BATCH COST=%s COUNT=%d \nSQL=[\n\t%s\n]",
		time.Since(start),
		len(sqls),
		strings.Join(sqls, "\n\t"),
	)
	if err != nil {
		sqlLogger.Println(err)
	}
}

var placeholderRE = regexp.MustCompile(`\$([1-9][0-9]*)`)

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

type loggingRow struct {
	row    pgx.Row
	start  time.Time
	sql    string
	args   []any
	logger logger
	logged bool
}

func (r *loggingRow) Scan(dest ...any) error {
	err := r.row.Scan(dest...)

	if !r.logged {
		r.logged = true

		if r.logger != nil {
			r.logger.Println(
				"COST", time.Since(r.start),
				"SQL", formatSQL(r.sql, r.args),
			)
			if err != nil {
				r.logger.Println(err)
			}
		}
	}

	return err
}
