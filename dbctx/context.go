package dbctx

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type sqlTxKey struct{}
type querierKey struct{}
type transactionKey struct{}

type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// SQLTx 是数据库事务：执行 SQL、提交或回滚。
// 它只关心 SQL 原子性，不承载业务事务的语义；后者见 Transaction。
type SQLTx interface {
	Querier
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// Scanner 抽象 pgx.Row 和 pgx.Rows 的公共部分，
// 让 GetByXxx/ListByXxx 的生成代码共用同一个 scan 函数。
type Scanner interface {
	Scan(dest ...any) error
}

// Flusher 是可以被 Transaction 收集、在事务提交前批量落库的对象。
// FlushStmt 返回空 query 表示这个对象没有被修改过，无需落库。
// FlushDone 在对应语句执行成功后回调，用来同步 orig 状态、清 dirty，
// 并根据受影响行数判断这一行是否还存在。
//
// TableName 和 Identity 一起给出一行的身份，Transaction 用它们检测"同一个事务里
// 有两个对象指向同一行"——那会导致两条 UPDATE 各写各的、后写的静默覆盖先写的。
// 主键自增且还没回填（Insert 之前）时 Identity 返回 0，表示这个对象还没有身份。
type Flusher interface {
	TableName() string
	Identity() int64
	FlushStmt() (query string, args []any)
	FlushDone(rowsAffected int64) error
}

// Transaction 是一次业务事务的工作集：身份表、脏对象、回滚和提交后任务。
// 它由 Procedure 创建，随 Procedure 结束而销毁；SQL 事务本身见 SQLTx。
type Transaction interface {
	// GetCachedRecord 返回事务里已经缓存的记录（对应 xdb 的 getCachedTRecord）。
	GetCachedRecord(tableName string, id int64) (Flusher, bool)

	// AddCachedRecord 把记录放进事务缓存并返回规范记录
	// （对应 xdb 的 addCachedTRecord）。同一行已经有记录时返回已有记录。
	AddCachedRecord(obj Flusher) Flusher

	// AfterCommit 注册 COMMIT 明确成功后执行的任务。
	AfterCommit(fn func(context.Context))

	// AfterRollback 注册事务回滚后执行的任务。
	AfterRollback(fn func(context.Context))
}

func HasSQLTx(ctx context.Context) bool {
	return ctx.Value(sqlTxKey{}) != nil
}

func SQLTxFromCtx(ctx context.Context) SQLTx {
	return ctx.Value(sqlTxKey{}).(SQLTx)
}

func SQLTxFromCtxOptional(ctx context.Context) (SQLTx, bool) {
	res := ctx.Value(sqlTxKey{})
	if res == nil {
		return nil, false
	}
	return res.(SQLTx), true
}

func QuerierFromCtx(ctx context.Context) Querier {
	if _, ok := SQLTxFromCtxOptional(ctx); ok {
		panic("SelectByXXX cannot be called inside transaction")
	}
	return ctx.Value(querierKey{}).(Querier)
}

func WithSQLTx(ctx context.Context, tx SQLTx) context.Context {
	return context.WithValue(ctx, sqlTxKey{}, tx)
}

func WithQuerier(ctx context.Context, q Querier) context.Context {
	return context.WithValue(ctx, querierKey{}, q)
}

func WithTransaction(ctx context.Context, txn Transaction) context.Context {
	return context.WithValue(ctx, transactionKey{}, txn)
}

func TransactionFromCtxOptional(ctx context.Context) (Transaction, bool) {
	value := ctx.Value(transactionKey{})
	if value == nil {
		return nil, false
	}
	return value.(Transaction), true
}

// AfterCommit 把 fn 注册到当前事务，在 COMMIT 明确成功后执行。
// 没有事务时立即执行：没有提交点需要等待。
//
// 钩子拿到的 ctx 保留调用方的值（请求 ID、身份等），但不带事务本身，
// 也不带取消/超时——事务的总预算不应该把提交后的发包、事件打死。
// 钩子需要超时就自己控制。
func AfterCommit(ctx context.Context, fn func(context.Context)) {
	if fn == nil {
		return
	}
	txn, ok := TransactionFromCtxOptional(ctx)
	if !ok {
		fn(context.WithoutCancel(ctx))
		return
	}
	txn.AfterCommit(fn)
}

// AfterRollback 把 fn 注册到当前事务，在事务回滚后执行。
// 没有事务时立即执行，语义与 AfterCommit 对称。
func AfterRollback(ctx context.Context, fn func(context.Context)) {
	if fn == nil {
		return
	}
	txn, ok := TransactionFromCtxOptional(ctx)
	if !ok {
		fn(context.WithoutCancel(ctx))
		return
	}
	txn.AfterRollback(fn)
}

// GetOrAddCachedRecord 把 obj 解析到当前事务的缓存记录：
// 命中 (TableName, Identity) 时返回已有对象，否则登记 obj 并返回它。
// 非事务查询（例如 GM/RPC 的 SelectXxx）没有 Transaction，直接返回 obj。
//
// 对应 xdb 的 getCachedTRecord + addCachedTRecord 两步。pdbgen 是先扫描
// 再解析，所以命中时新扫出来的对象会被丢弃；对象本身没有 identity 时不进缓存。
func GetOrAddCachedRecord[T Flusher](ctx context.Context, obj T) T {
	txn, ok := TransactionFromCtxOptional(ctx)
	if !ok {
		return obj
	}

	id := obj.Identity()
	if id != 0 {
		if existing, ok := txn.GetCachedRecord(obj.TableName(), id); ok {
			canonical, ok := existing.(T)
			if !ok {
				panic(fmt.Sprintf(
					"pdbgen: %s id=%d 的缓存记录类型不一致：已有 %T，新来 %T",
					obj.TableName(), id, existing, obj,
				))
			}
			return canonical
		}
	}

	added := txn.AddCachedRecord(obj)
	canonical, ok := added.(T)
	if !ok {
		panic(fmt.Sprintf(
			"pdbgen: %s id=%d 的缓存记录类型不一致：返回 %T，期望 %T",
			obj.TableName(), id, added, obj,
		))
	}
	return canonical
}

// Deprecated: use GetOrAddCachedRecord.
func Track[T Flusher](ctx context.Context, obj T) T {
	return GetOrAddCachedRecord(ctx, obj)
}

// QueryOne 是 GetByXxx/SelectByXxx 生成代码的公共实现：
// 查询单行，无结果返回零值，查到则登记进 Transaction（若有）。
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
		if errors.Is(err, pgx.ErrNoRows) {
			return zero
		}
		panic(err)
	}
	return GetOrAddCachedRecord(ctx, obj)
}

// QueryList 是 ListByXxx/SelectListByXxx/GetAll/SelectAll 的生成代码公共实现。
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
		result = append(result, GetOrAddCachedRecord(ctx, obj))
	}
	if err := rows.Err(); err != nil {
		panic(err)
	}
	return result
}

// Deprecated: use SQLTx.
type TX = SQLTx

// Deprecated: use SQLTxFromCtx.
func TxFromCtx(ctx context.Context) SQLTx {
	return SQLTxFromCtx(ctx)
}

// Deprecated: use SQLTxFromCtxOptional.
func TxFromCtxOptional(ctx context.Context) (SQLTx, bool) {
	return SQLTxFromCtxOptional(ctx)
}

// Deprecated: use HasSQLTx.
func HasTx(ctx context.Context) bool {
	return HasSQLTx(ctx)
}

// Deprecated: use WithSQLTx.
func WithTx(ctx context.Context, tx SQLTx) context.Context {
	return WithSQLTx(ctx, tx)
}

// Deprecated: use Transaction.
type UnitOfWork = Transaction

// Deprecated: use WithTransaction.
func WithUnitOfWork(ctx context.Context, txn Transaction) context.Context {
	return WithTransaction(ctx, txn)
}
