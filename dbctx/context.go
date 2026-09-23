package dbctx

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type txKey struct{}
type querierKey struct{}
type unitOfWorkKey struct{}

type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

type DB interface {
	Querier
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type UnitOfWork interface {
	Register(obj any, update func(context.Context) error)
}

func HasTx(ctx context.Context) bool {
	return ctx.Value(txKey{}) != nil
}

func TxFromCtx(ctx context.Context) DB {
	return ctx.Value(txKey{}).(DB)
}

func TxFromCtxOptional(ctx context.Context) (DB, bool) {
	res := ctx.Value(txKey{})
	if res == nil {
		return nil, false
	}

	return res.(DB), true
}

func QuerierFromCtx(ctx context.Context) Querier {
	if _, ok := TxFromCtxOptional(ctx); ok {
		panic("SelectByXXX cannot be called inside transaction")
	}

	return ctx.Value(querierKey{}).(Querier)
}

func WithTx(ctx context.Context, tx DB) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

func WithQuerier(ctx context.Context, q Querier) context.Context {
	return context.WithValue(ctx, querierKey{}, q)
}

func WithUnitOfWork(ctx context.Context, uow UnitOfWork) context.Context {
	return context.WithValue(ctx, unitOfWorkKey{}, uow)
}

func RegisterDirtyObject(ctx context.Context, obj any, update func(context.Context) error) {
	value := ctx.Value(unitOfWorkKey{})
	if value == nil {
		// 非事务查询，例如 GM/RPC SelectXXX。
		return
	}

	uow := value.(UnitOfWork)

	uow.Register(obj, update)
}
