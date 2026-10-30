# pdbgen 设计说明

这份文档记录 pdbgen 的**运行时语义和设计决策**——那些光看代码看不出来、但写业务代码时必须遵守的东西。和 `CLAUDE.md` 的分工：`CLAUDE.md` 讲怎么改这个仓库（命令、生成链路、注意点），这里讲框架对使用者的承诺。

改动运行时的语义时要同时改这里，并在下面的"变更记录"里留痕。

---

## 1. 定位

pdbgen 是面向游戏服务端的 PostgreSQL 数据访问框架：`pdb.xml` 定义表结构，生成器产出建表/迁移 SQL 和一套 Go 代码。它不做通用 ORM 的权衡——约束条件很明确：

- 一次事务服务一个请求，事务入口挂咨询锁，同一把锁的请求天然串行；
- 表都是窄表，一次请求涉及的改动少；
- 宁可让约束显式（panic、报错），也不做隐式兜底。

在这三个前提下，框架选择了"脏标记 + 事务工作集 + 身份表"的组合，而不是 SQL 构建器或 ActiveRecord。

## 2. 核心不变量

### 2.1 一次事务里，同一行只有一个缓存记录

`dbctx.GetOrAddCachedRecord` 在事务内按 `(TableName(), Identity())` 解析记录：

- 命中时返回已有对象，**丢弃**这次新扫描出来的对象；
- 未命中时把它放进事务缓存并返回；
- 非事务查询（`SelectXxx`）没有 Transaction，直接返回扫描出来的对象，并把它标记为脱离事务（见 2.7）。

这对应 xdb 的两步：`Transaction.getCachedTRecord` 命中就直接用，未命中再 `addCachedTRecord`；生成的实体相当于 xdb 的 `TRecord`。

所以同一个事务里：

```go
a := UserRedEnvelopeTable.GetByUidActId(ctx, uid, actId)
b := UserRedEnvelopeTable.GetById(ctx, a.Id())
// a == b，是同一个指针
a.SetTodayCount(11)
b.SetTotal(42)
// 提交时只有一条 UPDATE: SET today_count=11, total=42 WHERE id=...
```

`GetById` 在事务里会先查身份表：命中就直接返回，**不再发 SELECT**，也就不会读到事务未 flush 的旧值。唯一索引查询（`GetByUidActId` 等）仍会先查询，因为身份表按主键索引；查回来后再解析成已有对象。

### 2.2 身份表的边界

- 触发时机是**扫描完成后、返回给业务前**；`QueryOne` / `QueryList` 都会走这一步。
- 命中判断只看 `(表名, 主键)`，**不看对象脏不脏**。
- 同一个指针重复登记只算一次，不会重复进 flush 列表。
- `Identity() == 0` 的对象（自增主键还没回填、还没 `Insert`）没有身份，不进身份表；`Insert` 回填主键后会再登记一次。
- 身份表只在事务内有效；跨事务/跨请求不共享。

这条保证替代了早期的"同一行两个对象就 panic"守卫：现在不是报错，而是把第二次扫描出来的对象解析成第一次的记录。

### 2.3 落库时机在事务提交前，不在 `SetXxx` 时

`SetXxx` / `Delete()` 只改内存；真正的 UPDATE / DELETE 由 `Procedure` 在业务回调返回后统一 flush。`Insert` 例外：它要拿回自增主键，当场执行。所以：

- 一次事务里所有修改合并成一个 `pgx.Batch` 发出，一次往返；任何一条失败（包括 UPDATE / DELETE 没命中任何行，`dbpool.ErrRowNotFound`）→ 整体回滚。
- 事务中途查库看到的是**库里的值**。框架不去兼容"改了再查"，而是把会读错的写法封死（见 2.7）：
  - 改了某些列、还没落库，再按这些列做条件查同一张表 → panic（`dbctx.CheckPredicate`）。SELECT 按库里的旧值过滤，会多行或漏行。改别的列不受影响；按主键查走身份表，也不受影响。
  - 同一事务里删了再插同一个唯一键 → INSERT 先于 DELETE 到达数据库，撞唯一索引，整个事务回滚。这是约定：要"重置"一行就改它的列，不要删了重建。
  - 业务手写的 SQL 同样只看到库里的值；手写 SQL 要读本事务改过的数据时，把它放到修改之前。

### 2.4 错误靠 panic 传播，事务边界统一收口

生成代码遇到 pgx 错误直接 panic；`QueryOne` 查不到返回 nil（`ErrNoRows` 不算错误，SQL 日志里也记 INFO），其他错误 panic。`Procedure` 的 defer 在业务返回错误、panic、flush 失败时恢复内存并回滚，panic 会**回滚后原样重抛**（要接住它必须包住整个 `Procedure` / `WithTx` 调用，放在回调里的 `recover` 接不到）。业务回调里不需要逐个检查 SQL 错误。

### 2.5 `GetXxx` 与 `SelectXxx` 互斥

| | 必须在事务里 | 用哪条连接 | 参与身份表解析 | 返回的实体 |
|---|---|---|---|---|
| `GetByXxx` / `ListByXxx` / `GetAll` / `Insert` | 是（否则 panic） | 事务 | 是 | 可以改 |
| `SelectByXxx` / `SelectListByXxx` / `SelectAll` | 否（是则 panic），且必须在 `dbpool.Query` 里 | 连接池 | 否 | 只读（脱离事务） |
| `dbpool.Query` | 否（是则报错） | 连接池 | 否 | — |

事务里从池里另取一条连接读数据，会读到事务未提交前的旧值——所以框架直接把这条路封死。

### 2.6 失败时恢复内存对象

- 对象第一次登记进事务时（查询返回前，或 `Insert` 发 SQL 前），事务调用 `SnapshotState()` 保存整条快照——生成的实体只有值字段，快照就是一次结构体拷贝。
- 业务返回错误、panic、flush 失败，或者 PostgreSQL 把 COMMIT 当成了 ROLLBACK（`pgx.ErrTxCommitRollback`，事务之前已经出错）时，`Procedure` 先恢复所有快照，再回滚 SQLTx，最后执行 AfterRollback 钩子。`Insert` 过的对象恢复成插入前的样子（主键 0、未加载），可以在新事务里重新插入。
- COMMIT 返回其他错误时结果不确定：**不回滚内存对象**，也不执行提交/回滚钩子；调用方应丢弃这批对象。

### 2.7 约定即约束

框架不为不支持的写法做兼容，而是让它们当场失败：

| 写法 | 结果 |
|---|---|
| 事务里再开事务（`Procedure` / `WithTx` 嵌套） | 返回 `ErrNestedTransaction`，内层不执行 |
| 改事务外查出来的实体，或者改已经结束的事务里的实体 | `SetXxx` / `Delete` panic：实体已脱离事务，改了也不会落库 |
| 改了查询条件列之后再按这些列查同一张表 | panic（见 2.3） |
| 事务外调用 `GetByXxx` / `Insert` / `dbctx.AfterCommit` / `dbctx.AfterRollback` | panic |
| 事务里调用 `SelectByXxx` / `dbpool.Query` | panic / 报错 |
| `Insert` 一个已经插入过或者查出来的对象 | panic |
| 删了再插同一个唯一键 | 数据库报唯一约束冲突，事务回滚 |

实体脱离事务的时机：事务外查询返回时；事务结束时（提交、回滚、提交结果不确定），事务里登记过的已加载实体全部脱离。新对象（`NewXxx()` 创建、还没插入或插入被回滚）不受影响。

生成器同样只接受框架支持的 schema（见第 4 节）。

## 3. 事务、锁与超时

- `pdb.xml` 的 `statementTimeoutMs` / `idleInTransactionSessionTimeoutMs` / `lockTimeoutMs` 拼进 BEGIN：`BEGIN; SET LOCAL statement_timeout = ...; ...`。不带参数的 Exec 走简单协议，一次往返发完，所以**每个事务都有这三个超时**，加不加锁都一样，也不多花往返。
- `transactionTimeoutMs` 是客户端预算：一次 `Procedure`（BEGIN 到 COMMIT 前）或一次 `dbpool.Query` 回调的总时长上限。pgx 默认在 ctx 到期时只断开本地连接、**不发 CancelRequest**——服务端的语句要靠 `statement_timeout` 停下来。
- `dbpool.Query` 没有事务，`SET LOCAL` 管不到；要给它服务端上限，用角色级设置 `ALTER ROLE app SET statement_timeout = '5s'`（经过 pgBouncer 事务池也生效；放在连接参数里会被 pgBouncer 拒绝）。
- `COMMIT` 用 `context.WithoutCancel(ctx)`：一旦发出就必须等到结果，避免"客户端不知道提交成功没有"导致重试重复发放。
- 咨询锁是**替代 `SELECT ... FOR UPDATE` 的行级串行手段**，在事务开头获取（见 3.1）。玩家请求用 try 版，GM/RPC 写请求用阻塞版（README）。单 bigint 和双 int32 是两个互不重叠的 key 空间。

**已知限制**：框架一次只挂一把锁，多把锁的获取顺序由业务自己保证；行的 UPDATE 顺序 = 对象注册顺序（首次查询顺序），一次事务锁多把锁时这个顺序会参与死锁判定。当前"一次事务一个 uid 一把锁"的模型下不存在这个问题。

### 3.1 Procedure 入口

`dbpool.Procedure(ctx, opts, fn)` 是事务的唯一入口，`WithTx(ctx, fn)` 是不加锁的简写：

```go
type ProcedureOptions struct {
    Lock    LockSpec      // 零值 = 不加锁
    Timeout time.Duration // <=0 表示只用 pdb.xml 的 transactionTimeoutMs
}

func Procedure(ctx context.Context, opts ProcedureOptions, fn func(context.Context) error) error
```

锁用构造器生成：

```go
Procedure(ctx, ProcedureOptions{Lock: TryAdvisoryLock(uid)}, fn)       // 单 key，try
Procedure(ctx, ProcedureOptions{Lock: AdvisoryLock(uid)}, fn)          // 单 key，阻塞
Procedure(ctx, ProcedureOptions{Lock: TryAdvisoryLock2(ns, id)}, fn)   // 双 int32，try
Procedure(ctx, ProcedureOptions{Lock: AdvisoryLock2(ns, id)}, fn)      // 双 int32，阻塞
```

- `Timeout` 与 `transactionTimeoutMs` 同时生效时取更早到期的那个；COMMIT 仍然用 `WithoutCancel`，不受这个预算影响。
- **不自动重试**：try 锁拿不到返回 `ErrAdvisoryLockNotAcquired`，由业务返回"稍后再试"或在上层重试；框架不在请求里空转。blocking 锁由 `lock_timeout` 限制等待，等超时同样返回 `ErrAdvisoryLockNotAcquired`——调用方只需要认这一个"忙"信号。`fn` 只执行一次。
- **不能嵌套**：ctx 里已经有事务时返回 `ErrNestedTransaction`。嵌套事务会从池里另拿一条连接：同一把锁自己等自己，同一行在两个事务里各有一个对象互相覆盖，并发高时连接池互等。

### 3.2 提交后 / 回滚后任务

`dbctx.AfterCommit(ctx, fn)` / `dbctx.AfterRollback(ctx, fn)` 把任务挂到当前事务：

- **AfterCommit** 只在 COMMIT 明确成功后执行；COMMIT 返回错误时结果不确定，两个钩子都不执行——可能已提交，不能发失败通知；也可能没提交，不能发成功通知。
- **AfterRollback** 在业务返回错误、panic、或 flush 失败导致回滚后执行；执行时内存对象已经恢复。
- **顺序**：都按注册顺序执行；同步执行在调用方 goroutine 上，慢钩子会拖慢请求。
- **只能在事务里注册**：没有事务时 panic——在没有提交点的地方注册"提交后"任务，一般是 ctx 传错了。
- **上下文**：钩子拿到的是调用方 ctx 去掉取消/超时、且不带事务的副本；保留请求 ID 等值。钩子里可以开新事务（`Procedure`），查库用 `dbpool.Query`。
- **panic**：单个钩子 panic 只记日志（slog），后续钩子继续执行；已经提交的数据不受影响。
- **用途**：发包、RPC、事件等外部副作用放 AfterCommit；失败通知放 AfterRollback。不要在钩子里做能失败并需要回滚的业务。

## 4. 生成代码的形状

| 生成物 | 说明 |
|---|---|
| `NewXxx()` | 新对象，还没有身份；`SetXxx` 之后 `Insert` |
| `GetById` / `SelectById` | 主键查询；`GetById` 在事务里先查身份表，命中不发 SQL |
| `DeleteById` | 先 `GetById`（缓存优先），找不到返回 false；找到后标记删除 |
| `GetByYyy` / `SelectByYyy` | **unique 索引**查询（`index unique="true"`） |
| `ListByYyy` / `SelectListByYyy` | **非 unique 索引**查询，返回 `[]*Xxx` |
| `List<Method>` / `SelectList<Method>`，unique 时 `Get<Method>` / `Select<Method>` | **部分索引**查询（`index where="..." method="..."`），参数是 `variable` 里的列，条件常量写死在 SQL 里 |
| `GetAll` / `SelectAll` | 全表，返回 `[]*Xxx` |
| `SetXxx` / `Xxx()` | 写入（值没变不置脏位；实体脱离事务时 panic）/ 读取 |
| `Delete()` / `IsDeleted()` | 标记删除 / 查询删除状态 |
| `Insert()` | `INSERT ... RETURNING <pk>`，只接受还没插入过的新对象 |
| `TableName()` / `Identity()` / `FlushStmt()` / `FlushDone()` / `DirtyColumns()` / `SnapshotState()` / `RestoreState()` / `Detach()` | 实现 `dbctx.Flusher`，供运行时使用 |

改 `unique` 会改变生成的 API 形态（单行 ↔ 列表）。列表查询返回的元素在事务内都会登记进身份表；再对同一行单独 `GetById` 会返回列表里的同一个指针，不会产生第二个对象。

删除是延迟的：`Delete()` / `DeleteById` 只标记，提交前由 flush 统一发 `DELETE`；同一个事务内 `GetById` / 唯一索引 / 列表查询都会过滤掉已删除的记录；事务回滚时对象恢复成未删除状态。

生成器只接受框架支持的 schema，其余直接报错：

- 主键必须是 `int64` 且 `autoIncrement="true"`（身份表、`Insert` 回填、`GetById` 缓存都按它实现）；
- 除主键外至少一列，最多 64 列（`dirty` 是 `uint64` 位掩码）；
- 列名是小写字母开头的 Go 标识符，不能是 Go 关键字（`type` 换成 `typ` / `kind`），不能和生成代码的内部名字冲突（`loaded`、`deleted`、`detached`、`dirty`、`t`、`o`、`q`、`ctx`、`query`，以及 `tableName`、`delete` 这类和实体方法同名的列）；
- 类型只能是 `int64` / `int32` / `int16` / `int8` / `string` / `bool` / `float32` / `float64`；
- 同一组列只能有一个索引，也不能和主键相同；一个 bean 只能映射一张表。
- 部分索引：`where` 是逗号分隔的 `变量=常量`（之间是 AND），只支持 bool 列（`true` / `false`）和整数列（按类型校验范围），不能用主键，同一列不能出现两次；必须写 `method`（大写字母开头的 Go 标识符），普通索引不能写 `method`；不写 `variable` 时索引建在主键上、查询没有参数，这时不能 unique；列加条件相同的部分索引不能重复，生成的方法名不能和别的查询重复。

部分索引的条件常量直接写进查询 SQL，不用 `$n` 参数：PostgreSQL 只在查询条件能推出索引条件时才用部分索引，参数化的 `worker_enabled = $1` 在通用计划里推不出来。bool 条件写成 `"col"` / `NOT "col"`，和库里存的形式一致。条件列和 `variable` 一样进 `CheckPredicate`。genddl 生成 `CREATE [UNIQUE] INDEX IF NOT EXISTS "idx_<表>_where_<method 的 snake_case>" ... WHERE ...`；已存在的索引用 `pg_get_expr(indpred)` 读出条件，去掉括号、引号和整数类型转换后比较，对不上报错，不重建。

## 5. 已知缺口

1. **嵌套 savepoint / 部分回滚没有实现**：只有事务级回滚，嵌套事务直接拒绝（`ErrNestedTransaction`）。
2. **咨询锁的 bigint key 空间只有一个**：`TryAdvisoryLock(uid)` 和别的业务用 bigint key 的锁会撞号。约定 bigint 形式只给 uid 用，其他业务锁用 `AdvisoryLock2(ns, id)`。（README 里"uid 超过 int32"的问题已经由单 bigint 形式解决。）
3. **多咨询锁场景下的加锁/更新顺序**：见第 3 节，当前模型下不适用。
4. **`dbpool.Query` 没有服务端超时**：只有客户端预算，服务端上限要靠角色级 `statement_timeout`（见第 3 节）。

## 6. 设计提案

- **Procedure 与 Transaction 的职责划分与命名**：[design-procedure-transaction.md](design-procedure-transaction.md)（提案，Phase 1-5 已实施）。实施时并入本文正文，并在变更记录里留痕。

## 7. 变更记录

### 2026-10-30 支持部分索引

- **改了什么**：`index` 新增 `where`、`method` 属性（`readxml.Index.Where` / `Method`，解析和校验在 `readxml.ParseWhere`）。genbeans 按 `method` 生成 `List<Method>` / `SelectList<Method>`（unique 时 `Get<Method>` / `Select<Method>`），内部查询函数 `queryWhere<Method>`，条件常量写进 SQL，条件列进 `CheckPredicate`；模板支持没有参数的查询。genddl 生成带 `WHERE` 的索引，内省时读 `pg_get_expr(indpred, indrelid)` 并按 `normalizePredicate` 比较条件。
- **为什么**：expservergo 重启时要按 `worker_enabled = true` 恢复 worker，只有少数行满足条件；全列索引既大又不能生成无参的查询方法，手写查询又绕开了生成器。
- **影响范围**：没有 `where` 的索引，生成代码和 DDL 都不变（重新生成本仓库 `ptable`，`git diff` 为空）；普通索引写 `method` 现在会报错。部分索引只支持 bool 和整数条件。
- **验证状态**：`go build/vet/test` 通过；新增 genbeans 部分索引生成和拒绝用例、genddl 的 DDL 生成和条件比较用例（比较用例的 PG 一侧是本地 PostgreSQL `pg_get_expr` 的实测输出）。在 expservergo 的本地库上用临时 schema 跑通：执行 genddl 输出后再跑一次只剩 `CREATE SCHEMA`，`EXPLAIN` 走部分索引。

### 2026-10-28 去掉 sensitive 列

- **改了什么**：删掉 `pdb.xml` 的 `sensitive` 属性（`readxml.Variable.Sensitive`）、生成器的 `sqlArg` / `SQLArgs` 和相关校验、`dbctx.Secret`，以及 SQL 日志里 `'***'` 的分支；生成代码直接传字段值。
- **为什么**：使用方不需要在 SQL 日志里隐去列值，这套机制只增加了复杂度。
- **影响范围**：SQL 日志里原来打成 `'***'` 的列现在打出原值；`pdb.xml` 里残留的 `sensitive` 属性会被 XML 解析忽略，不报错。传给数据库的值和运行时语义不变。
- **验证状态**：`go build/vet/test` 通过；重新生成了本仓库的 `ptable` 和 go_pg_test 的 `ptable`。

### 2026-10-28 约定改成约束；精简运行时和生成代码

- **改了什么**：
  - 超时：三个 `SET LOCAL` 拼进 BEGIN，所有事务生效（之前只有加锁的事务才下发）；`transactionTimeoutMs` 同时作为 `dbpool.Query` 回调的预算。
  - 锁：阻塞锁等到 `lock_timeout` 也返回 `ErrAdvisoryLockNotAcquired`（之前是一个原样的 PgError 55P03），和 try 锁拿不到是同一个"忙"信号。
  - 约束：嵌套 `Procedure` 返回 `ErrNestedTransaction`；事务外查出来的实体、事务结束后的实体脱离事务，`SetXxx` / `Delete` panic；改了查询条件列之后再按这些列查同一张表 panic（`dbctx.CheckPredicate`）；`dbctx.AfterCommit` / `AfterRollback` 只能在事务里调用（之前事务外立即执行）；`Insert` 拒绝已经插入过的对象；`pgx.ErrTxCommitRollback` 按回滚处理。
  - 生成器：只接受 int64 自增主键、合法且不冲突的列名、受支持的类型，索引不能重复，一个 bean 只映射一张表；`sensitive="true"` 的列在 SQL 日志里打成 `'***'`。
  - SQL 日志：不再把 SQL 当 `Sprintf` 格式串（`LIKE 'a%'`、取模不再乱码）；`nil` 打成 `NULL`；查不到记 INFO 而不是 ERROR。
  - 精简：删掉 Deprecated 别名、四个 `With*AdvisoryLock*` 薄封装和公开的 `WithTxPrepare`（`Procedure` 是唯一入口）；生成代码去掉 `orig*` 字段、`xxxState` 结构、`loadedXxx`、`ResetToLoaded`、`ResetNew`、`Update` 和每表一个的 `ErrXxxNotFound`（统一为 `dbpool.ErrRowNotFound`），拼 UPDATE 改由 `dbctx.UpdateStmt` 完成，每张表的生成代码减少约一半；`RegisterNew` 合并进 `GetOrAddCachedRecord`（插入前的快照就是回滚目标）；ctx 里 SQLTx 和 Transaction 合成一个值（`dbctx.HasSQLTx` 改名 `InTransaction`）；`dbpool.Transaction` 改为包内类型；锁从 5 种模式 + 接口简化成一个结构体，拿锁只发一条语句；生成器的中间结构从 `readxml` 挪回 `gen/bean`。
- **为什么**：这些问题原来都是"约定"：嵌套事务、事务外改实体、改了再查、ctx 传错都写得出来，而且静默出错（锁自己等自己、修改丢失、查询多行漏行、副作用在提交前发出）。不去兼容这些写法，而是让它们当场失败。精简掉的都是多轮迭代留下来、已经没有调用方的 API。
- **影响范围**：go_pg_test 用到的 API（`Procedure` / `WithTx` / `Query` / 四个锁构造器 / `dbctx.SQLTxFromCtx` / `dbctx.AfterCommit`，以及生成的查询、`SetXxx`、`Insert`）都不变，重新生成 `ptable` 即可；违反上述约束的业务代码会从静默出错变成 panic / 报错。
- **验证状态**：`go build` / `go vet` / `PDBGEN_REQUIRE_DB=1 go test ./...`（真实 PG，不跳过）通过；新增测试覆盖每一条约束、三个超时在加锁和不加锁的事务里都生效、`statement_timeout` 能取消不加锁事务里的慢语句、`Query` 预算、日志格式化和脱敏、生成器的 schema 校验。go_pg_test 重新生成后 build / vet / test 通过，`-tags=live` 的登录 + 红包联调通过。
- **未验证**：`-race`（本机没有 cgo）。

### 2026-10-22 生成 Delete / DeleteById

- **改了什么**：实体增加 `deleted` 状态和 `Delete()` / `IsDeleted()`；`FlushStmt` 在 deleted 时返回 `DELETE`；`FlushDone` 处理删除；`GetById` / `QueryOne` / `QueryList` 过滤已删除记录；生成 `DeleteBy<PK>`（主键为 `id` 时是 `DeleteById`），先 `GetBy<PK>`（缓存优先）再标记；`dbctx.Flusher` 增加 `IsDeleted()`。
- **为什么**：业务需要删除行，同时保持工作单元批量 flush、身份表和 memento 回滚语义。
- **影响范围**：删除延迟到提交前 flush；同一事务内查询看不到已删除行；回滚恢复对象；找不到返回 false；冷行 `DeleteById` 会多一次 `GetBy<PK>` 的 SELECT。
- **验证状态**：`go build/vet/test` 通过；`ptable/delete_test.go` 覆盖 `Delete` / `FlushStmt` / `FlushDone` / 快照恢复；真实 PG 集成测试覆盖提交、缺失、回滚、同事务隐藏、Insert 后删除。
- **未验证**：无。

### 2026-10-20 明确不自动重试

- **决定**：`Procedure` 不内置重试。try 锁拿不到返回 `ErrAdvisoryLockNotAcquired`，由业务返回"稍后再试"或在上层重试；blocking 锁由 `lock_timeout` 限制等待。PG serialization failure / deadlock 由调用方决定是否重跑。
- **为什么**：try 锁本来就是"玩家忙"的业务信号，请求内退避会占住 worker；自动重跑还要求业务幂等并定义每次尝试的钩子语义。
- **影响**：`ProcedureOptions` 不提供 Retry；文档里"重试待实现"的表述统一改成"不自动重试"。
- **验证状态**：无代码变化。

### 2026-10-20 增加事务级内存回滚（memento）

- **改了什么**：`dbctx.Flusher` 增加 `SnapshotState` / `RestoreState` / `ResetNew`；`dbctx.Transaction` 增加 `RegisterNew` / `Rollback`；`WithTxPrepare` 在错误、panic、Flush 失败时先恢复内存对象；生成的 `Insert` 先 `RegisterNew`。
- **为什么**：重试和共享缓存需要失败后内存状态与数据库一致；之前只有单对象 `ResetToLoaded`。
- **影响范围**：错误 / panic / Flush 失败路径恢复对象；COMMIT 结果不确定时不回滚内存、不执行钩子；成功路径不变。嵌套 savepoint / 部分回滚仍未实现。
- **验证状态**：`go build/vet/test` 通过；单元测试覆盖快照恢复、多对象、新建对象 `ResetNew`；真实 PG 集成测试覆盖加载后修改回滚、panic 回滚、Insert 回滚、AfterRollback 钩子看到恢复后的值、多对象回滚。
- **未验证**：嵌套 savepoint / 部分回滚；重试由调用方决定，不在框架内。

### 2026-10-20 增加 Procedure 统一入口

- **改了什么**：新增 `dbpool.Procedure(ctx, ProcedureOptions, fn)`、`ProcedureOptions`、`LockSpec` 和四个锁构造器；`WithTx` / 四个 `With*Lock*` 改为 `Procedure` 的薄封装；`ProcedureOptions.Timeout` 可以给单次 Procedure 加预算。
- **为什么**：把事务入口、锁、超时收敛到一个 xdb 风格的 `Procedure`；以后调整入口策略只需要改这一处。
- **影响范围**：现有入口行为不变；Phase 4 不重试，`fn` 仍然只执行一次；`Timeout` 与全局预算取更早；COMMIT 不受预算影响。
- **验证状态**：`go build/vet/test` 通过；真实 PG 集成测试覆盖 try 锁冲突、Timeout 体现在 ctx deadline、旧 `WithTryAdvisoryLock` 薄封装。
- **未验证**：阻塞锁的等待/超时行为依赖 `lock_timeout`，没有单独测试；自动重试不是目标。

### 2026-10-20 引入事务级缓存记录（身份表）

- **改了什么**：`dbctx.Transaction` 用 `GetCachedRecord` / `AddCachedRecord`（对应 xdb 的 `getCachedTRecord` / `addCachedTRecord`）替代 `Register`；新增 `dbctx.GetOrAddCachedRecord`，`QueryOne` / `QueryList` / 生成代码的 `Insert` 都走它；`dbpool.Transaction` 维护 `(表名, 主键) -> Flusher` 的缓存记录表。
- **为什么**：同一事务里同一行被两条查询路径各查一次会得到两个对象，提交时两条 UPDATE 各写各的绝对值，后写的静默覆盖先写的。之前用 panic 守卫；现在直接解析到同一个对象，既消除静默丢数据，也消除对业务写法的限制。命名对齐 xdb 的 `getCachedTRecord` / `addCachedTRecord`。
- **影响范围**：`GetByXxx` / `ListByXxx` / `GetAll` 在事务内返回规范对象；`GetById` 命中身份表时不再发 SELECT；唯一索引查询仍先 SELECT 再解析；`Identity()==0` 的对象不进表，`Insert` 回填后登记；非事务路径不变；`Update()` 仍绕过事务缓存。`Transaction.Register` 保留为 Deprecated 别名。
- **验证状态**：`go build/vet/test` 通过；单元测试覆盖命中、同指针、不同行、Identity=0、回填主键后登记、未命中；真实 PG 集成测试覆盖索引查询 + 主键查询返回同一指针、列表 + 单行查询返回同一指针、跨事务不共享。
- **未验证**：暂无。

### 2026-10-20 增加 AfterCommit / AfterRollback

- **改了什么**：`dbctx.Transaction` 增加 `AfterCommit` / `AfterRollback`；`dbpool.Transaction` 保存并在提交/回滚后按注册顺序执行；新增 `dbctx.AfterCommit` / `dbctx.AfterRollback` 注册入口；`WithTxPrepare` 在 COMMIT 成功、回滚完成后的对应时点执行钩子。
- **为什么**：发包、事件、RPC 等外部副作用不能在 COMMIT 之前发出；业务失败时要能发失败通知。参考 xdb 的 `ppostWhileCommit` / `ppostWhileRollback` 和 aotu_gs 的 `sendMsgWhileCommit`。
- **影响范围**：钩子同步执行；没有事务时立即执行；COMMIT 结果不确定时两个钩子都不执行；单个钩子 panic 只记日志，不影响后续钩子和事务结果。钩子 ctx 去掉 SQLTx/Transaction 和取消/超时，保留调用方值。
- **验证状态**：`go build/vet/test` 通过；单元测试覆盖事务外立即执行、事务内委托、注册顺序、只执行一次、panic 隔离；集成测试在真实 PG 上通过（提交后可见、回滚后不可见、panic 路径回滚、钩子 panic 不影响提交）。
- **未验证**：COMMIT 返回错误（结果不确定）时两个钩子都不跑——这条没有自动化测试，靠代码审查。

### 2026-10-20 命名统一：TX→SQLTx，Transaction

- **改了什么**：`dbctx.TX` → `dbctx.SQLTx`，`TxFromCtx` / `TxFromCtxOptional` / `WithTx` / `HasTx` 同步改名；`dbctx.UnitOfWork` / `dbpool.UnitOfWork` → `Transaction`；`NewUnitOfWork` → `NewTransaction`；文件 `dbpool/uow.go` → `dbpool/transaction.go`。旧名保留为 Deprecated 别名，方便外部代码过渡。生成模板同步改名并重新生成 `ptable/*.go`。
- **为什么**：`UnitOfWork` 这个名字不够直接；`Transaction` 与 xdb 的术语一致。SQL 事务和业务事务工作集是两层不同的东西，`SQLTx` 明确前者，`Transaction` 留给后者。
- **影响范围**：只改名，不改运行时语义、flush 时机和 panic 行为。`dbpool.WithTx` / `WithTxPrepare` 作为事务入口保留原名。
- **验证状态**：`go build ./...`、`go vet ./...`、`go test ./...` 通过（含真实 PG 集成测试）。重新生成 `ptable/*.go` 时顺带同步了模板中已有的表名 quote，属于之前模板与生成物的漂移。
- **未验证**：外部调用方是否使用旧名；旧名别名暂时保留。

### 2026-10-16 引入同一行重复检测

- **改了什么**：`dbctx.Flusher` 加 `TableName() string`；`Transaction` 加 `rows map[rowKey]dbctx.Flusher`，`Register` 在 `(表名, 主键)` 重复时 panic；生成模板加 `TableName()`，重新生成 `ptable/*.go`。
- **为什么**：同一行在一个事务里被两条查询路径各查一次会得到两个对象，提交时两条 UPDATE 各写各的绝对值，同一列被改两次时后写的静默覆盖先写的——这是框架里唯一会静默丢数据的场景。把它变成响的。
- **影响范围**：只影响事务内的查询路径（`GetXxx`）；`SelectXxx` / `Query` 非事务路径不登记、不检测。`Flusher` 是接口，加方法会波及所有实现——当前实现只有生成代码，已同步重新生成。
- **验证状态**：`dbpool/transaction_test.go` 用最小 Flusher 替身覆盖检测逻辑（重复行 panic、同指针不误报、不同行/不同表不误报、主键为 0 跳过）；`dbpool/duplicate_test.go` 对真实 PG 覆盖端到端行为（混合查询路径触发 panic 且事务回滚、只有一方脏也照样拦、跨事务不误报）。`go vet ./...` 干净。**注：重复行 panic 已在 2026-10-20 被身份表取代，见上一条变更记录。**
- **未验证**：`ListByYyy` 与后续单行查询组合时的检测行为（见已知缺口 4，只有设计推理没有测试）。

### 2026-10-16 放弃事务级身份表（被上面的方案取代）

- **背景**：先做的是"自动修复"版本——`dbctx.InstanceCache` 作为事务级身份表，单行查询包在 `dbctx.Resolve` 里，命中 `(表名, 主键)` 就丢弃新扫出来的对象、返回已有指针，并给 `Flusher` 加了 `Identity()`。
- **为什么撤掉**：它建立在"同一请求会查两次同一行"这个假设上，而按使用方的约定业务代码靠传参传递对象、不会回头再查。为不成立的场景维护身份表、查询包装层和反射判空不划算；而且它只是**修**，违反约定时悄无声息。移除后 `Flusher` 只保留 `TableName()` / `Identity()` 两个给检测用的方法，查询路径 `QueryOne` / `QueryList` 回到原样。
- **保留的部分**：`Identity()`（检测需要判断"是不是同一行"）和它的"主键为 0 表示还没回填"语义。
