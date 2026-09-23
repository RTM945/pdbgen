package dbpool

import (
	"context"
	"pdbgen/dbctx"

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
	o.log(sql, args)
	return o.DB.Exec(ctx, sql, args...)
}

func (o *loggingDB) Query(
	ctx context.Context,
	sql string,
	args ...any,
) (pgx.Rows, error) {
	o.log(sql, args)
	return o.DB.Query(ctx, sql, args...)
}

func (o *loggingDB) QueryRow(
	ctx context.Context,
	sql string,
	args ...any,
) pgx.Row {
	o.log(sql, args)
	return o.DB.QueryRow(ctx, sql, args...)
}

func (o *loggingDB) log(sql string, args []any) {
	if o.logger == nil {
		return
	}

	o.logger.Printf(
		"sql",
		"sql", sql,
		"args", args,
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
