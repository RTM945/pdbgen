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
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// Scanner 抽象 pgx.Row 和 pgx.Rows 的公共部分，
// 让 GetByXxx/ListByXxx 的生成代码共用同一个 scan 函数。
type Scanner interface {
	Scan(dest ...any) error
}

// Flusher 是可以被 UnitOfWork 收集、在事务提交前批量落库的对象。
// FlushStmt 返回空 query 表示这个对象没有被修改过，无需落库。
// FlushDone 在对应语句执行成功后回调，用来同步 orig 状态、清 dirty，
// 并根据受影响行数判断这一行是否还存在。
type Flusher interface {
	FlushStmt() (query string, args []any)
	FlushDone(rowsAffected int64) error
}

type UnitOfWork interface {
	Register(obj Flusher)
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

// Track 把 obj 登记到当前 ctx 绑定的 UnitOfWork。
// 非事务查询（例如 GM/RPC 的 SelectXxx）没有 UnitOfWork，是空操作。
func Track(ctx context.Context, obj Flusher) {
	value := ctx.Value(unitOfWorkKey{})
	if value == nil {
		return
	}
	value.(UnitOfWork).Register(obj)
}

// QueryOne 是 GetByXxx/SelectByXxx 生成代码的公共实现：
// 查询单行，无结果返回零值，查到则登记进 UnitOfWork（若有）。
func QueryOne[T Flusher](
	ctx context.Context,
	q Querier,
	query string,
	scan func(Scanner) (T, error),
	args ...any,
) T {
	var zero T
	row := q.QueryRow(ctx, query, args...)
	obj, err := scan(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return zero
		}
		panic(err)
	}
	Track(ctx, obj)
	return obj
}

// QueryList 是 ListByXxx/SelectListByXxx/GetAll/SelectAll 的公共实现。
func QueryList[T Flusher](
	ctx context.Context,
	q Querier,
	query string,
	scan func(Scanner) (T, error),
	args ...any,
) []T {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		panic(err)
	}
	defer rows.Close()

	var result []T
	for rows.Next() {
		obj, err := scan(rows)
		if err != nil {
			panic(err)
		}
		Track(ctx, obj)
		result = append(result, obj)
	}
	if err := rows.Err(); err != nil {
		panic(err)
	}
	return result
}
