package dbpool

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"pdbgen/dbctx"
	"pdbgen/readxml"
)

var (
	ErrAdvisoryLockNotAcquired = errors.New("advisory lock not acquired")

	// ErrNestedTransaction：ctx 里已经有事务时又开事务。嵌套事务会从池里另拿一条连接：
	// 同一把锁自己等自己、同一行在两个事务里各有一个对象互相覆盖、并发高时连接池互等。
	ErrNestedTransaction = errors.New("dbpool: nested transaction: ctx already carries a transaction")

	// ErrRowNotFound：提交前 flush 的 UPDATE / DELETE 没有命中任何行，这一行已经不在了。
	ErrRowNotFound = errors.New("dbpool: row not found at flush")

	// ErrNotOpen：还没有 Open / Init（或者已经 Close）就开事务、查询。
	ErrNotOpen = errors.New("dbpool: not open: call dbpool.Open or dbpool.Init first")

	// ErrAlreadyOpen：已经打开时又 Open / Init。包级函数用的默认句柄同一时刻只有一个，别的库用 New 开自己的句柄。
	ErrAlreadyOpen = errors.New("dbpool: already open")

	// ErrInTransaction：在事务里调用 Query。Query 从池里另取连接，读不到本事务改过的数据。
	ErrInTransaction = errors.New("dbpool: Query called inside a transaction: read with GetByXxx / ListByXxx / GetAll instead")

	// ErrTimeout：超过了 Procedure / Query 的客户端预算（Config.TransactionTimeout、ProcedureOptions.Timeout）。
	// 这样的错误同时满足 errors.Is(err, context.DeadlineExceeded)；调用方自己的 ctx 到期或被取消时不带它。
	ErrTimeout = errors.New("dbpool: transaction budget exceeded")

	// ErrCommitUnknown：COMMIT 发出去了，但没等到结果（连接断了，或者超过了 Config.CommitTimeout），
	// 可能提交了也可能没有。内存对象不回滚，两种钩子都不执行；调用方应丢弃这批对象，必要时按业务核对。
	ErrCommitUnknown = errors.New("dbpool: commit outcome unknown")
)

// Config 是连接池和事务的运行时参数。零值的字段用默认值：连接池的各项沿用 pgx 的默认值
// （URL 里的 pool_max_conns 等参数照常生效），超时为 0 表示不设。pdb.xml 里的写法见 SchemaConfig。
type Config struct {
	URL string // PostgreSQL 连接串

	// Name 是这个库的名字（pdb.xml 的 name），开出来的事务、查询都带着它；
	// 生成的代码按自己包里的 DBName 取事务，对不上时 panic（见 dbctx.SQLTxFromCtx）。
	Name string

	MaxConns          int32
	MinConns          int32
	MaxConnLifetime   time.Duration
	MaxConnIdleTime   time.Duration
	HealthCheckPeriod time.Duration

	// AppName 是连接的 application_name，在 pg_stat_activity 里区分是哪个服务。
	AppName string

	// 拼进每个事务的 BEGIN（SET LOCAL），加不加锁的事务都生效，见 docs/design.md 第 3 节。
	StatementTimeout                time.Duration
	IdleInTransactionSessionTimeout time.Duration
	LockTimeout                     time.Duration

	// TransactionTimeout 是一次 Procedure（BEGIN 到 COMMIT 之前）或一次 Query 回调的客户端预算，
	// 超过时返回 ErrTimeout。statement_timeout 只管单条语句，很多条语句加起来慢它拦不住。
	TransactionTimeout time.Duration

	// CommitTimeout 是等 COMMIT / ROLLBACK 结果的上限，默认 30s。COMMIT 等不到结果按 ErrCommitUnknown 处理。
	CommitTimeout time.Duration

	// ShowSQL 把每条语句（参数换成字面量）记一条 Info 日志，落点见 SetLogger。
	ShowSQL bool
}

const defaultCommitTimeout = 30 * time.Second

// SchemaConfig 取出 pdb.xml 根元素上的运行时参数。没写的属性是 0，表示用默认值（见 Config）。
func SchemaConfig(s *readxml.Schema) Config {
	sec := func(n int) time.Duration { return time.Duration(n) * time.Second }
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	return Config{
		URL:                             s.URL,
		Name:                            s.Name,
		MaxConns:                        s.PoolMaxConns,
		MinConns:                        s.PoolMinConns,
		MaxConnLifetime:                 sec(s.PoolMaxConnLifetime),
		MaxConnIdleTime:                 sec(s.PoolMaxConnIdleTime),
		HealthCheckPeriod:               sec(s.PoolHealthCheckPeriod),
		AppName:                         s.AppName,
		StatementTimeout:                ms(s.StatementTimeoutMs),
		IdleInTransactionSessionTimeout: ms(s.IdleInTransactionSessionTimeoutMs),
		LockTimeout:                     ms(s.LockTimeoutMs),
		TransactionTimeout:              ms(s.TransactionTimeoutMs),
		CommitTimeout:                   ms(s.CommitTimeoutMs),
		ShowSQL:                         s.ShowSQL,
	}
}

// poolConfig 把 Config 叠到 URL 解析出来的 pgx 配置上：只覆盖非零的字段。
func (c Config) poolConfig() (*pgxpool.Config, error) {
	for _, f := range []struct {
		name     string
		negative bool
	}{
		{"MaxConns", c.MaxConns < 0},
		{"MinConns", c.MinConns < 0},
		{"MaxConnLifetime", c.MaxConnLifetime < 0},
		{"MaxConnIdleTime", c.MaxConnIdleTime < 0},
		{"HealthCheckPeriod", c.HealthCheckPeriod < 0},
		{"StatementTimeout", c.StatementTimeout < 0},
		{"IdleInTransactionSessionTimeout", c.IdleInTransactionSessionTimeout < 0},
		{"LockTimeout", c.LockTimeout < 0},
		{"TransactionTimeout", c.TransactionTimeout < 0},
		{"CommitTimeout", c.CommitTimeout < 0},
	} {
		if f.negative {
			return nil, fmt.Errorf("dbpool: Config.%s must not be negative", f.name)
		}
	}

	pc, err := pgxpool.ParseConfig(c.URL)
	if err != nil {
		return nil, err
	}
	if c.MaxConns > 0 {
		pc.MaxConns = c.MaxConns
	}
	if c.MinConns > 0 {
		pc.MinConns = c.MinConns
	}
	if pc.MinConns > pc.MaxConns {
		return nil, fmt.Errorf("dbpool: MinConns %d is greater than MaxConns %d", pc.MinConns, pc.MaxConns)
	}
	if c.MaxConnLifetime > 0 {
		pc.MaxConnLifetime = c.MaxConnLifetime
	}
	if c.MaxConnIdleTime > 0 {
		pc.MaxConnIdleTime = c.MaxConnIdleTime
	}
	if c.HealthCheckPeriod > 0 {
		pc.HealthCheckPeriod = c.HealthCheckPeriod
	}
	// extended protocol + 不缓存 prepared statement，配合 pgBouncer 的 transaction pooling。
	pc.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	if c.AppName != "" {
		pc.ConnConfig.RuntimeParams["application_name"] = c.AppName
	}
	return pc, nil
}

// DB 是 New 建立的一个库的连接池和它的运行参数。打开之后只读，计数器除外。
// 只连一个库时用包级函数（Open / Procedure / Query），它们用的是默认句柄；
// 再连别的库就 New 一个 DB，用它的同名方法。
type DB struct {
	name          string // Config.Name
	pool          *pgxpool.Pool
	beginSQL      string        // BEGIN 加上 SET LOCAL，见 buildBeginSQL
	budget        time.Duration // Config.TransactionTimeout
	commitTimeout time.Duration
	showSQL       bool
	stats         procedureCounters
}

// active 是包级函数用的默认句柄。每次 Procedure / Query 都不加锁地读它，只有 Open / Close 写，所以是 atomic：
// 关服时等在途工作超时后照样 Close，晚到的调用拿到 ErrNotOpen，而不是数据竞争。
var active atomic.Pointer[DB]

// New 建立一个库的连接池。和 pgxpool 一样不预先连库：连不上要到第一次用的时候才知道。
func New(ctx context.Context, cfg Config) (*DB, error) {
	pc, err := cfg.poolConfig()
	if err != nil {
		return nil, err
	}
	d := &DB{
		name:          cfg.Name,
		beginSQL:      buildBeginSQL(cfg.StatementTimeout, cfg.IdleInTransactionSessionTimeout, cfg.LockTimeout),
		budget:        cfg.TransactionTimeout,
		commitTimeout: cmp.Or(cfg.CommitTimeout, defaultCommitTimeout),
		showSQL:       cfg.ShowSQL,
	}
	if d.pool, err = pgxpool.NewWithConfig(ctx, pc); err != nil {
		return nil, err
	}
	return d, nil
}

// Open 用 New 建立默认句柄，之后包级的 Procedure / WithTx / Query 都用它；已经打开时返回 ErrAlreadyOpen。
func Open(ctx context.Context, cfg Config) error {
	if active.Load() != nil {
		return ErrAlreadyOpen
	}
	d, err := New(ctx, cfg)
	if err != nil {
		return err
	}
	if !active.CompareAndSwap(nil, d) {
		d.pool.Close()
		return ErrAlreadyOpen
	}
	return nil
}

// Init 用 pdb.xml 根元素上的参数打开连接池，等价于 Open(ctx, SchemaConfig(pdb))。
func Init(ctx context.Context, pdb *readxml.Schema) error {
	return Open(ctx, SchemaConfig(pdb))
}

// buildBeginSQL 拼出每个事务的开头：BEGIN 加上 SET LOCAL。不带参数的 Exec 走简单协议，
// 多条语句一次往返发完——所以无论加不加锁，三个超时都生效，也不多花往返。
// 超时按毫秒向上取整：PostgreSQL 里 0 表示不限，不足 1ms 的超时不能变成 0。
func buildBeginSQL(statementTimeout, idleInTransactionSessionTimeout, lockTimeout time.Duration) string {
	var b strings.Builder
	b.WriteString("BEGIN")
	for _, s := range []struct {
		name string
		d    time.Duration
	}{
		{"statement_timeout", statementTimeout},
		{"idle_in_transaction_session_timeout", idleInTransactionSessionTimeout},
		{"lock_timeout", lockTimeout},
	} {
		if s.d > 0 {
			fmt.Fprintf(&b, "; SET LOCAL %s = %d", s.name, (s.d+time.Millisecond-1)/time.Millisecond)
		}
	}
	return b.String()
}

// Close 关掉默认句柄，重复调用安全。之后包级的 Procedure / Query 返回 ErrNotOpen，可以再 Open。
//
// 必须排在 HTTP 服务优雅关闭**之后**：pgxpool 的 Close 会阻塞到所有借出的连接归还，
// 并拒绝后续的 Acquire——在途请求还没跑完就调它，等于把它们打死在半路。
func Close() {
	if d := active.Swap(nil); d != nil {
		d.Close()
	}
}

// Close 关掉这个句柄的连接池，重复调用安全，时机同包级的 Close。之后再用它，SQL 返回连接池已关闭的错误。
func (d *DB) Close() { d.pool.Close() }

// Query 在事务外执行只读回调：SelectByXxx / SelectAll 从连接池读，每次查询各取一条连接。
// 不能在事务里调用（返回 ErrInTransaction）——事务里改过的数据，从池里另一条连接读不到。
//
// 回调里 SQL 出错时，Query 返回这个错误（回调不需要逐个检查）；回调自己的 panic 原样上抛。
// 这里没有事务，SET LOCAL 管不到；回调受 TransactionTimeout 限制（超过返回 ErrTimeout），
// 服务端的上限要靠角色级的 statement_timeout（见 docs/design.md）。
func Query(ctx context.Context, fn func(context.Context) error) error {
	d := active.Load()
	if d == nil {
		return ErrNotOpen
	}
	return d.Query(ctx, fn)
}

// Query 同包级的 Query，用这个句柄的连接池。
func (d *DB) Query(ctx context.Context, fn func(context.Context) error) (err error) {
	if dbctx.InTransaction(ctx) {
		return ErrInTransaction
	}
	if d.budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, d.budget, ErrTimeout)
		defer cancel()
	}
	defer func() {
		if r := recover(); r != nil {
			aerr := dbctx.AbortError(r)
			if aerr == nil {
				panic(r)
			}
			err = aerr
		}
		if overBudget(ctx, err) {
			err = fmt.Errorf("%w: %w", ErrTimeout, err)
		}
	}()
	return fn(dbctx.WithQuerier(ctx, d.name, d.wrap(d.pool)))
}

// overBudget 报告 err 是不是这次 Procedure / Query 自己的预算到期造成的，
// 而不是调用方的 ctx 到期或被取消（那种情况 context.Cause 不是 ErrTimeout）。
func overBudget(ctx context.Context, err error) bool {
	return err != nil && errors.Is(context.Cause(ctx), ErrTimeout) && !errors.Is(err, ErrTimeout) &&
		(errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err))
}

// Stats 是 Open 以来的计数快照，由使用方导出到自己的监控（比如用 expvar.Func 包一层）。
type Stats struct {
	Procedures ProcedureStats

	// Pool 是 pgxpool 的统计：连接数、借连接的次数和等待时间等。没有打开时为 nil。
	Pool *pgxpool.Stat
}

// ProcedureStats 按结果给 Procedure 计数，每次调用只记一项。被拒绝的调用（嵌套、没有打开）不计。
type ProcedureStats struct {
	Committed uint64 // 提交成功
	Busy      uint64 // 咨询锁没拿到（ErrAdvisoryLockNotAcquired）
	TimedOut  uint64 // 超过预算（ErrTimeout）
	Failed    uint64 // 其他原因没有提交：fn 返回错误、SQL 出错、flush 失败、BEGIN 失败、COMMIT 被当成 ROLLBACK、runtime.Goexit
	Panicked  uint64 // fn panic，回滚后原样上抛
	Unknown   uint64 // 提交结果不确定（ErrCommitUnknown）
}

// procedureCounters 由执行 Procedure 的各个 goroutine 不加锁地累加、ReadStats 读取，所以是 atomic。
type procedureCounters struct {
	committed, busy, timedOut, failed, panicked, unknown atomic.Uint64
}

// ReadStats 返回默认句柄的计数；没有打开时返回零值。
func ReadStats() Stats {
	d := active.Load()
	if d == nil {
		return Stats{}
	}
	return d.Stats()
}

// Stats 返回这个句柄的计数。
func (d *DB) Stats() Stats {
	return Stats{
		Procedures: ProcedureStats{
			Committed: d.stats.committed.Load(),
			Busy:      d.stats.busy.Load(),
			TimedOut:  d.stats.timedOut.Load(),
			Failed:    d.stats.failed.Load(),
			Panicked:  d.stats.panicked.Load(),
			Unknown:   d.stats.unknown.Load(),
		},
		Pool: d.pool.Stat(),
	}
}
