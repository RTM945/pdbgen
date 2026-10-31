package dbctx

import (
	"context"
	"errors"
	"fmt"
	"math/bits"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier 是查询需要的最小接口：事务外是连接池，事务里是 SQLTx。
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// SQLTx 是数据库事务，只负责执行 SQL；提交和回滚由 dbpool.Procedure 负责。
type SQLTx interface {
	Querier
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// Scanner 是 pgx.Row 和 pgx.Rows 的公共部分，生成代码的 scan 函数共用它。
type Scanner interface {
	Scan(dest ...any) error
}

// RecordState 是生成代码的整条实体快照，运行时只负责保存和回传。
type RecordState any

// Flusher 是生成的实体。Transaction 收集它们，提交前把修改合并成一个 Batch 落库。
type Flusher interface {
	// TableName 和 Identity 给出一行的身份；Identity 为 0 表示还没 Insert。
	TableName() string
	Identity() int64

	// FlushStmt 返回待执行的 UPDATE（只含脏列）或 DELETE，没有修改时返回空串；
	// FlushDone 在这条语句执行成功后调用。
	FlushStmt() (query string, args []any)
	FlushDone()

	// DirtyColumns 返回改过、还没落库的列（位掩码），CheckPredicate 用它判断查询条件是否失效。
	DirtyColumns() uint64

	// IsDeleted 报告这一行是否在本事务里标记了删除；查询会把它当作不存在。
	IsDeleted() bool

	// SnapshotState / RestoreState 保存、恢复整条实体，事务失败时用。
	SnapshotState() RecordState
	RestoreState(state RecordState)

	// Detach 把已加载的实体标记为脱离事务，之后 SetXxx / Delete 会 panic。
	Detach()
}

// Owned 是 pdb.xml 里声明了 lock 的表生成的实体：每一行由 key 等于 lock 列的值的 bigint 咨询锁保护。
// 事务登记从库里查出来的行时，没有持有这把锁就调用 MarkForeign，这一行在本事务里只读：
// 不持锁去改，和持锁的请求并发时，后提交的 UPDATE 会用绝对值覆盖先提交的（丢更新）。
type Owned interface {
	Flusher

	// LockKey 返回 lock 列的名字和这一行的值，也就是保护这一行的那把锁的 key。
	LockKey() (column string, key int64)

	// MarkForeign 标记这一行不受当前事务持有的锁保护：之后 SetXxx / Delete 会 panic（PanicForeign）。
	MarkForeign()
}

// Transaction 是一次业务事务的工作集：SQL 事务、身份表、提交/回滚后任务。
// 由 dbpool.Procedure 创建并放进 ctx，Procedure 结束即失效。
type Transaction interface {
	SQLTx() SQLTx

	// GetCachedRecord 返回事务里已经缓存的记录（xdb: getCachedTRecord）。
	GetCachedRecord(table string, id int64) (Flusher, bool)

	// AddCachedRecord 把记录登记进事务并返回规范记录：同一行已经有记录时返回已有的
	// （xdb: addCachedTRecord）。
	AddCachedRecord(obj Flusher) Flusher

	// Records 按登记顺序返回事务里的记录。
	Records() []Flusher

	AfterCommit(fn func(context.Context))
	AfterRollback(fn func(context.Context))
}

type transactionKey struct{}
type querierKey struct{}

func WithTransaction(ctx context.Context, txn Transaction) context.Context {
	return context.WithValue(ctx, transactionKey{}, txn)
}

func TransactionFromCtxOptional(ctx context.Context) (Transaction, bool) {
	txn, ok := ctx.Value(transactionKey{}).(Transaction)
	return txn, ok
}

// InTransaction 报告 ctx 是否带着事务。
func InTransaction(ctx context.Context) bool {
	_, ok := TransactionFromCtxOptional(ctx)
	return ok
}

func mustTransaction(ctx context.Context, what string) Transaction {
	txn, ok := TransactionFromCtxOptional(ctx)
	if !ok {
		panic("pdbgen: " + what + " 只能在 dbpool.Procedure 里调用")
	}
	return txn
}

// SQLTxFromCtx 返回 ctx 里的 SQL 事务，不在事务里时 panic。
// GetByXxx / ListByXxx / GetAll / Insert 都经过这里。
func SQLTxFromCtx(ctx context.Context) SQLTx {
	return mustTransaction(ctx, "GetByXxx / ListByXxx / GetAll / Insert").SQLTx()
}

func WithQuerier(ctx context.Context, q Querier) context.Context {
	return context.WithValue(ctx, querierKey{}, q)
}

// QuerierFromCtx 返回 dbpool.Query 放进 ctx 的连接池，SelectByXxx / SelectAll 都经过这里。
// 在事务里调用会 panic：从池里另取一条连接读不到本事务的修改。
func QuerierFromCtx(ctx context.Context) Querier {
	if InTransaction(ctx) {
		panic("pdbgen: SelectByXxx / SelectAll 不能在事务里调用，事务里用 GetByXxx / ListByXxx / GetAll")
	}
	q, ok := ctx.Value(querierKey{}).(Querier)
	if !ok {
		panic("pdbgen: SelectByXxx / SelectAll 只能在 dbpool.Query 里调用")
	}
	return q
}

// AfterCommit 注册 COMMIT 明确成功后执行的任务（发包、RPC、事件等外部副作用）。
// 只能在事务里调用：在没有提交点的地方注册"提交后"任务，一般是 ctx 传错了。
//
// 钩子同步执行、按注册顺序；拿到的 ctx 保留调用方的值，但不带事务，也不带取消/超时。
func AfterCommit(ctx context.Context, fn func(context.Context)) {
	mustTransaction(ctx, "dbctx.AfterCommit").AfterCommit(fn)
}

// AfterRollback 注册事务回滚后执行的任务（失败通知），只能在事务里调用。
// 执行时内存对象已经恢复到进事务时的样子。
func AfterRollback(ctx context.Context, fn func(context.Context)) {
	mustTransaction(ctx, "dbctx.AfterRollback").AfterRollback(fn)
}

// GetCachedRecordAs 是生成的 GetById 的缓存优先查询：事务里已经有这一行就直接返回，
// 不发 SELECT；本事务标记了删除时返回零值和 true。非事务或未命中返回 false。
func GetCachedRecordAs[T Flusher](ctx context.Context, table string, id int64) (T, bool) {
	var zero T
	txn, ok := TransactionFromCtxOptional(ctx)
	if !ok {
		return zero, false
	}
	rec, ok := txn.GetCachedRecord(table, id)
	if !ok {
		return zero, false
	}
	if rec.IsDeleted() {
		return zero, true
	}
	return rec.(T), true
}

// GetOrAddCachedRecord 把 obj 交给当前事务，返回这一行在事务里的规范记录：
// 同一行已经有记录时丢弃 obj、返回已有的。
// 不在事务里时把 obj 标记为脱离事务——事务外查出来的实体改了也不会落库，误改直接 panic。
func GetOrAddCachedRecord[T Flusher](ctx context.Context, obj T) T {
	if txn, ok := TransactionFromCtxOptional(ctx); ok {
		return txn.AddCachedRecord(obj).(T)
	}
	obj.Detach()
	return obj
}

// abort 是生成代码把 SQL 错误（连接断开、超时、约束冲突……）送到事务边界的方式：
// dbpool.Procedure / dbpool.Query 接住它，回滚后把里面的 error 当返回值交给调用方。
// 业务代码里不用逐个检查 SQL 错误，调用方也拿得到 error（errors.Is / errors.As 照常可用）。
// 误用（脱离事务、事务外调用、改了条件列再查）不是 abort，照旧 panic。
type abort struct{ err error }

func (a *abort) Error() string { return a.err.Error() }
func (a *abort) Unwrap() error { return a.err }

// AbortError 返回 recover 到的 r 里的 SQL 错误；r 不是 abort（业务 panic、误用、nil）时返回 nil。
// 只给 dbpool 的事务边界用。
func AbortError(r any) error {
	if a, ok := r.(*abort); ok {
		return a.err
	}
	return nil
}

// fail 以 err 结束当前的 Procedure / Query，错误里带上出错的语句（缩写，见 summary）。
func fail(query string, err error) {
	panic(&abort{fmt.Errorf("pdbgen: %s: %w", summary(query), err)})
}

// summary 把生成的语句缩成错误信息里看得懂的样子：SELECT 的列清单换成 ...，INSERT 只留表名。
func summary(query string) string {
	if rest, ok := strings.CutPrefix(query, "SELECT "); ok {
		if _, from, ok := strings.Cut(rest, " FROM "); ok {
			return "SELECT ... FROM " + from
		}
	}
	if head, _, ok := strings.Cut(query, " ("); ok && strings.HasPrefix(head, "INSERT INTO ") {
		return head
	}
	return query
}

// QueryOne 是单行查询的公共实现：查不到返回零值；查到的行解析成事务里的规范记录，
// 本事务标记了删除的行当作查不到。SQL 出错时结束当前的 Procedure / Query（见 abort）。
func QueryOne[T Flusher](ctx context.Context, q Querier, query string, scan func(Scanner) (T, error), args ...any) T {
	var zero T
	obj, err := scan(q.QueryRow(ctx, query, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return zero
	}
	if err != nil {
		fail(query, err)
	}
	if obj = GetOrAddCachedRecord(ctx, obj); obj.IsDeleted() {
		return zero
	}
	return obj
}

// QueryList 是列表查询的公共实现，规则同 QueryOne。
func QueryList[T Flusher](ctx context.Context, q Querier, query string, scan func(Scanner) (T, error), args ...any) []T {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		fail(query, err)
	}
	defer rows.Close()

	var result []T
	for rows.Next() {
		obj, err := scan(rows)
		if err != nil {
			fail(query, err)
		}
		if obj = GetOrAddCachedRecord(ctx, obj); !obj.IsDeleted() {
			result = append(result, obj)
		}
	}
	if err := rows.Err(); err != nil {
		fail(query, err)
	}
	return result
}

// InsertReturning 执行生成代码的 INSERT ... RETURNING <主键>，把主键写进 id。
// SQL 出错（比如撞了唯一索引）时结束当前的 Procedure（见 abort）。
func InsertReturning(ctx context.Context, tx SQLTx, query string, id *int64, args ...any) {
	if err := tx.QueryRow(ctx, query, args...).Scan(id); err != nil {
		fail(query, err)
	}
}

// CheckPredicate 是事务里条件查询的守卫：table 表有记录在 columns 这些列上改了、还没落库时 panic。
// 修改要到提交前才统一落库，SELECT 按库里的旧值过滤，结果会多出或漏掉这些行——
// 所以同一事务里，用作查询条件的列必须先查后改。按主键查不受影响（主键不能改）。
func CheckPredicate(ctx context.Context, table string, columns uint64) {
	txn, ok := TransactionFromCtxOptional(ctx)
	if !ok {
		return
	}
	for _, rec := range txn.Records() {
		if rec.TableName() == table && rec.DirtyColumns()&columns != 0 {
			panic(fmt.Sprintf(
				"pdbgen: %s id=%d 在本事务里改了查询条件用到的列、还没落库，不能再按这些列查 %s"+
					"（SELECT 按库里的旧值过滤，结果会多行或漏行）；请把查询挪到修改之前",
				table, rec.Identity(), table,
			))
		}
	}
}

// PanicDetached 是生成代码 SetXxx / Delete 的守卫，实体已经脱离事务时调用。
func PanicDetached(rec Flusher) {
	panic(fmt.Sprintf(
		"pdbgen: %s id=%d 已脱离事务（在事务外查出来的，或者所属事务已经结束），改了也不会落库；"+
			"请在 dbpool.Procedure 里用 GetByXxx 重新查询后再改",
		rec.TableName(), rec.Identity(),
	))
}

// PanicForeign 是生成代码 SetXxx / Delete 的守卫：这一行由一把当前事务没有持有的锁保护（见 Owned）。
func PanicForeign(rec Owned) {
	col, key := rec.LockKey()
	panic(fmt.Sprintf(
		"pdbgen: %s id=%d 由 %s=%d 的锁保护，当前事务没有持有这把锁，不能改它："+
			"和持锁的请求并发时，后提交的 UPDATE 会覆盖先提交的（丢更新）。"+
			"请在持有这把锁的 Procedure 里改（ProcedureOptions{Lock: dbpool.AdvisoryLock(%d)}，"+
			"同时改几个玩家的就把几个 key 都写上）",
		rec.TableName(), rec.Identity(), col, key, key,
	))
}

// UpdateStmt 拼只含脏列的 UPDATE，供生成代码的 FlushStmt 使用。
// columns[i] / values[i] 是第 i 个可更新列（已加引号），dirty 的第 i 位表示它改过；主键是最后一个参数。
func UpdateStmt(table, pkColumn string, pk int64, columns []string, dirty uint64, values ...any) (string, []any) {
	var sb strings.Builder
	args := make([]any, 0, bits.OnesCount64(dirty)+1)
	sb.WriteString("UPDATE " + table + " SET ")
	for i, col := range columns {
		if dirty&(1<<i) == 0 {
			continue
		}
		if len(args) > 0 {
			sb.WriteString(", ")
		}
		args = append(args, values[i])
		sb.WriteString(col + " = $" + strconv.Itoa(len(args)))
	}
	args = append(args, pk)
	sb.WriteString(" WHERE " + pkColumn + " = $" + strconv.Itoa(len(args)))
	return sb.String(), args
}
