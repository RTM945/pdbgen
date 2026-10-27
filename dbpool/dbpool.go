package dbpool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pdbgen/dbctx"
	"pdbgen/readxml"
)

var (
	dbpool *pgxpool.Pool

	// beginSQL 开启每个事务：BEGIN 加上 pdb.xml 配置的 SET LOCAL。不带参数的 Exec 走简单协议，
	// 多条语句一次往返发完——所以无论加不加锁，三个超时都生效，也不多花往返。
	beginSQL = "BEGIN"

	// budget 是一次 Procedure 或 Query 回调的客户端总预算（transactionTimeoutMs）。
	// statement_timeout 只管单条语句，很多条语句加起来慢它拦不住；PG 17 之前也没有事务级的 GUC。
	budget time.Duration

	showSQL bool
)

var (
	ErrAdvisoryLockNotAcquired = errors.New("advisory lock not acquired")

	// ErrNestedTransaction：ctx 里已经有事务时又开事务。嵌套事务会从池里另拿一条连接：
	// 同一把锁自己等自己、同一行在两个事务里各有一个对象互相覆盖、并发高时连接池互等。
	ErrNestedTransaction = errors.New("dbpool: nested transaction: ctx already carries a transaction")

	// ErrRowNotFound：提交前 flush 的 UPDATE / DELETE 没有命中任何行，这一行已经不在了。
	ErrRowNotFound = errors.New("dbpool: row not found at flush")
)

func Init(ctx context.Context, pdb *readxml.Schema) error {
	cfg, err := pgxpool.ParseConfig(pdb.URL)
	if err != nil {
		return err
	}
	cfg.MaxConns = pdb.PoolMaxConns
	cfg.MinConns = pdb.PoolMinConns
	cfg.MaxConnLifetime = time.Duration(pdb.PoolMaxConnLifetime) * time.Second
	cfg.MaxConnIdleTime = time.Duration(pdb.PoolMaxConnIdleTime) * time.Second
	cfg.HealthCheckPeriod = time.Duration(pdb.PoolHealthCheckPeriod) * time.Second
	// extended protocol + 不缓存 prepared statement，配合 pgBouncer 的 transaction pooling。
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	if pdb.AppName != "" {
		cfg.ConnConfig.RuntimeParams["application_name"] = pdb.AppName
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	dbpool = pool
	beginSQL = buildBeginSQL(pdb.StatementTimeoutMs, pdb.IdleInTransactionSessionTimeoutMs, pdb.LockTimeoutMs)
	budget = time.Duration(pdb.TransactionTimeoutMs) * time.Millisecond
	showSQL = pdb.ShowSQL
	return nil
}

func buildBeginSQL(statementTimeoutMs, idleInTransactionSessionTimeoutMs, lockTimeoutMs int) string {
	var b strings.Builder
	b.WriteString("BEGIN")
	for _, s := range []struct {
		name string
		ms   int
	}{
		{"statement_timeout", statementTimeoutMs},
		{"idle_in_transaction_session_timeout", idleInTransactionSessionTimeoutMs},
		{"lock_timeout", lockTimeoutMs},
	} {
		if s.ms > 0 {
			fmt.Fprintf(&b, "; SET LOCAL %s = %d", s.name, s.ms)
		}
	}
	return b.String()
}

// Close 关掉连接池，重复调用安全。
//
// 必须排在 HTTP 服务优雅关闭**之后**：pgxpool 的 Close 会阻塞到所有借出的连接归还，
// 并拒绝后续的 Acquire——在途请求还没跑完就调它，等于把它们打死在半路。
func Close() {
	if dbpool != nil {
		dbpool.Close()
	}
}

// Query 在事务外执行只读回调：SelectByXxx / SelectAll 从连接池读，每次查询各取一条连接。
// 不能在事务里调用——事务里改过的数据，从池里另一条连接读不到。
//
// 这里没有事务，SET LOCAL 管不到；回调受 budget 限制，服务端的上限要靠角色级的
// statement_timeout（见 docs/design.md）。
func Query(ctx context.Context, fn func(context.Context) error) error {
	if dbctx.InTransaction(ctx) {
		return errors.New("dbpool.Query cannot be called inside transaction")
	}
	if budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	return fn(dbctx.WithQuerier(ctx, wrapLogger(dbpool)))
}
