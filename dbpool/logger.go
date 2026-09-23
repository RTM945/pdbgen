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

func (o *loggingDB) Exec(
	ctx context.Context,
	sql string,
	args ...any,
) (pgconn.CommandTag, error) {
	start := time.Now()

	tag, err := o.DB.Exec(ctx, sql, args...)

	o.log(start, sql, args, err)

	return tag, err
}

func (o *loggingDB) Query(
	ctx context.Context,
	sql string,
	args ...any,
) (pgx.Rows, error) {
	start := time.Now()

	rows, err := o.DB.Query(ctx, sql, args...)

	o.log(start, sql, args, err)

	return rows, err
}

func (o *loggingDB) QueryRow(
	ctx context.Context,
	sql string,
	args ...any,
) pgx.Row {
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
		"sql",
		"cost", elapsed,
		"sql", formatSQL(sql, args),
		"error", err,
	)
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

	return fmt.Sprintf(sql, values...)
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
				"sql",
				"cost", time.Since(r.start),
				"sql", formatSQL(r.sql, r.args),
				"error", err,
			)
		}
	}

	return err
}
