# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概览

pdbgen 是面向 PostgreSQL 的"代码生成 + 运行时"框架，目标场景是游戏服务端：把 `pdb.xml` 作为数据库表和 Go 结构体的唯一事实来源，生成建表/迁移 SQL 和一套带脏标记追踪、工作单元（Transaction）批量落库的 CRUD 代码。模块名 `pdbgen`，Go 1.27，唯一直接依赖是 `github.com/jackc/pgx/v5`。

运行时的语义承诺（事务、锁、缓存记录/身份表、欠账）记在 [docs/design.md](docs/design.md)：**改了运行时语义要同步改它**，并在它的"变更记录"里留痕。

## 常用命令

```bash
# 构建（两个生成器 + 运行时包）
go build ./...
go vet ./...

# 生成 Go 数据表代码：写入 genOutput 目录（相对 pdb.xml 所在的目录，默认 ./ptable）。
# 改了 pdb.xml、模板或者生成器都要重新生成（TestPtableIsUpToDate 会检查）
go run ./gen/bean -schema ./pdb.xml

# 生成 DDL：连接 pdb.xml 里的 url 读库里的真实结构，把 SQL 打到 stdout（不写文件、不执行）
go run ./gen/ddl -schema ./pdb.xml > ddl.sql

# 测试。dbpool 的集成测试连 pdb.xml 里那套 PG（见 README 的 docker 命令）：
# 连不上默认 skip；PDBGEN_REQUIRE_DB=1 时直接失败，确认改动真的在库上跑过时用它
go test ./...
PDBGEN_REQUIRE_DB=1 go test ./...

# 竞态检测需要 cgo 和 gcc（本机是 MSYS2 UCRT64，在 C:\msys64\ucrt64\bin）。改了并发相关的代码一定要跑
CGO_ENABLED=1 PDBGEN_REQUIRE_DB=1 go test -race ./...

# 基准（连库）：数字记在 design.md 1.2，改了 Procedure / flush / 日志的路径要重跑并更新
go test ./dbpool ./ptable -run '^$' -bench . -benchmem
```

两个生成器的 `-schema` 默认值是 `./pdb.xml`。`genOutput` 相对 pdb.xml 所在的目录，在哪个目录下运行都一样。不提交编出来的生成器（`.gitignore` 忽略 `*.exe`）：用 pdbgen 的项目在自己的 go.mod 里登记成工具，版本跟着 go.mod 走——`go get -tool pdbgen/gen/bean pdbgen/gen/ddl`，之后 `go tool bean -schema app/pdb.xml`、`go tool ddl -schema app/pdb.xml`。改了模板或运行时接口（`dbctx.Flusher` 等）要重新生成用到 pdbgen 的项目的 `ptable`，否则旧的生成代码编不过（go_pg_test 还在用它 `bin/` 下拷贝的旧 exe）。

源文件一律 LF：`.gitattributes` 是 `* text=auto eol=lf`，`core.autocrlf=true` 的机器上检出来也是 LF，重跑生成器不会让 `ptable` 显示为已修改，`gofmt -l` 也不会因为 CRLF 报文件。Windows 上用 Python 改文件时 `write_text` 要传 `newline="\n"`。

## 代码生成链路

```
pdb.xml ──readxml──┬─> gen/ddl  连库内省 → CREATE/ALTER DDL（包在一个事务里）打到 stdout，人工执行
                   └─> gen/bean 模板渲染 → ptable/<表名>.pdb.go（每个 table 一个文件）
```

- [pdb.xml](pdb.xml) 同时是生成配置和表结构定义：`bean` 声明字段（Go 类型），`table` 把 bean 映射到表名、主键、索引。运行时参数（连接池、各类超时、`showSQL`、`appName`）也在这里。
- [readxml/](readxml/) 只负责读 `pdb.xml` 和命名转换；生成器的中间模型（`field` / `tableGen` / `queryMethod`）在 `gen/bean` 里。[readxml/parse.go](readxml/parse.go) 自己解析 XML（不用 `encoding/xml` 的 Unmarshal），每个元素的属性表就是允许的全部属性：不认识的元素和属性、写不对的值都报错，带 `文件:行号`，一次报全（design.md 第 4 节）。加属性就是在对应元素的属性表里加一项，结构体（[readxml/xml.go](readxml/xml.go)）同步加字段。
- [gen/ddl/main.go](gen/ddl/main.go) 只做增量：缺的表/列/索引才生成 DDL；已存在的列只做校验（类型不符、XML 要求 NOT NULL 但库里可空 → 直接报错），**不存在任何修改列类型或 DROP 的逻辑**，主键/索引定义对不上也是报错而不是重建。输出包在 `BEGIN; ... COMMIT;` 里。`checkNames` 先检查所有标识符不超过 63 字节、表/主键/索引在 schema 里不重名（超长的用 `<index name="...">` / `<primaryKey name="...">` 起短名字）。连库用 `QueryExecModeExec`，经过 pgBouncer 也能用。`TestGenerateAgainstDatabase` 在真实 PG 的临时 schema 里跑完整流程（建表、再跑无事可做、加列加索引、定义不符报错），测完删掉 schema。Go 类型映射：`int64→BIGINT`、`int32→INTEGER`、`int16/int8→SMALLINT`、`string→TEXT`、`bool→BOOLEAN`、`float32→REAL`、`float64→DOUBLE PRECISION`，非空列生成 `NOT NULL DEFAULT` 默认值（bool 默认 `false`，数值默认 `0`，文本默认 `''`）。
- [gen/bean/main.go](gen/bean/main.go) + [gen/bean/template.go](gen/bean/template.go) 生成 `ptable/<表名>.pdb.go`。模板是 Go 源码里的一个字符串常量，生成后经 `gofmt` 格式化。框架不支持的 schema 在这里直接报错（规则见 [docs/design.md](docs/design.md) 第 4 节），不要为了兼容某种 schema 去放宽它；所有表的错误一次报出来，带 `pdb.xml:行号`。[gen/bean/output.go](gen/bean/output.go) 负责写文件：内容没变不写；删除输出目录里不再需要的生成文件（第一行是 `header` 的 `.go` 文件，包括以前的 `<表名>.go`），不碰手写文件；用 go/build 的规则检查文件名不带平台后缀（`buildsEverywhere`）；用 go/parser 解析生成结果，查出不同的表生成了同一个包级名字（`checkDeclarations`）。生成代码导入的 dbctx 路径从 readxml 的导入路径推出来（`dbctxImport`），模块改名不用改模板。

生成代码对表结构的约定：

- 复合索引用逗号分隔写在 `index` 的 `variable` 上（`uid,actId`）。**unique 索引生成单行查询 `GetByXxx`/`SelectByXxx`，非 unique 索引生成列表查询 `ListByXxx`/`SelectListByXxx`**，主键固定是 `GetById`/`SelectById`。改 `unique` 会改变生成的 API 形态。
- 部分索引：`<index variable="uid" where="state=0, flag=true" method="PendingByUid"/>` 生成 `ListPendingByUid` / `SelectListPendingByUid`（unique 时 `Get`/`Select` 前缀），`variable` 可省（索引建在主键上，查询无参数）。`where` 只支持 bool 和整数列，常量写进 SQL 而不是 `$n` 参数（否则规划器用不上部分索引）。规则见 [docs/design.md](docs/design.md) 第 4 节。
- `<table lock="uid">`：每一行由 key 等于这一列的值的 bigint 咨询锁保护，生成的实体实现 `dbctx.Owned`，事务没持有这把锁时查出来的行只读（`SetXxx` / `Delete` panic）。归某个玩家的表都应该写，规则见 design.md 2.8。
- 主键必须是 `int64` 且 `autoIncrement="true"`：不出现在 INSERT 的字段列表里，靠 `RETURNING id` 回填，也不参与 UPDATE。`primaryKey` 的 `start` / `step` 生成 `START WITH` / `INCREMENT BY`；只影响 identity 首次创建，已存在的 identity 不会被自动修改。
- SQL 里的表名带 schema（`"public"."user_mail"`，`schema` 属性默认 `public`），和 DDL 建表的位置一致，不依赖连接的 search_path。`TableName()` 仍然是不带 schema 的表名（身份表的 key、日志和错误信息）。
- 表名只能是小写字母开头的 snake_case：它同时是 SQL 表名、文件名和 Go 名字的一部分。
- `ptable/` 下的 `<表名>.pdb.go` 全部是生成产物，不要手改；改结构要改 `pdb.xml`、必要时改模板，然后重新生成并提交。`ptable/delete_test.go` 是手写测试。

## 运行时架构

### 脏标记 + 事务工作集（[dbctx/](dbctx/)、[dbpool/transaction.go](dbpool/transaction.go)）

生成的结构体是"当前值 + dirty 位掩码 + 三个状态位（loaded / deleted / detached）"（见 [ptable/user_red_envelope.go](ptable/user_red_envelope.go)）：

- `SetXxx` 值没变就不置脏位，变了就 `dirty |= 1<<n`；实体脱离事务时 panic，写了 `lock` 的表在本事务没持有这一行的锁（`foreign`）时也 panic。getter 只读当前值。生成器按 bean 字段顺序给除主键外的列分配位。
- `dbctx.Flusher` 是运行时看到的实体：`TableName()` / `Identity()` 给出行的身份（`Identity` 为 0 表示主键未回填）；`FlushStmt()` 在未 `loaded` 时返回空，`deleted` 时返回 `DELETE`，否则用 `dbctx.UpdateStmt` 拼**只含脏列的 UPDATE**；`FlushDone()` 清脏位；`SnapshotState()` / `RestoreState()` 就是整条结构体拷贝；`DirtyColumns()` 给 `CheckPredicate` 用；`Detach()` 让实体脱离事务。
- 查询路径（`dbctx.QueryOne` / `QueryList`）在返回前调 `dbctx.GetOrAddCachedRecord`：事务里解析成当前事务的缓存记录，事务外标记为脱离事务。`Insert` 也走它：发 SQL 前登记一次（拍"插入前"的快照，回滚就恢复成它），主键回填后再登记一次（进身份表）。
- **同一事务里一行只有一个缓存记录**：按 `(TableName(), Identity())` 解析，命中就返回已有对象（xdb 的 `getCachedTRecord` / `addCachedTRecord`）。`GetById` 先查身份表，命中不发 SELECT；索引查询先查库再解析，查之前用 `dbctx.CheckPredicate` 确认条件列没有未落库的修改。细节见 [docs/design.md](docs/design.md)。
- 事务按登记顺序收集实体，提交前把所有修改合并成一个 `pgx.Batch` 一次往返发出；UPDATE / DELETE 没命中任何行返回 `dbpool.ErrRowNotFound`。任何一条失败都整体回滚。业务代码只管 `SetXxx`，落库在提交前统一处理。
- 删除：`Delete()` 标记删除；`DeleteById` 先 `GetById`（缓存优先）再标记，找不到返回 false；查询过滤已删除的行；提交前 flush 成 `DELETE`。
- 事务失败（错误 / panic / flush 失败 / `pgx.ErrTxCommitRollback`）时恢复所有快照，再回滚 SQLTx，最后跑 AfterRollback。COMMIT 结果不确定时不回滚内存，也不跑钩子。无论哪种结局，事务结束后登记过的已加载实体都脱离事务。
- 外部副作用（发包、RPC、事件）用 `dbctx.AfterCommit(ctx, fn)`，失败通知用 `dbctx.AfterRollback(ctx, fn)`，只能在事务里注册。钩子同步执行、按注册顺序；钩子 ctx 不带事务和取消/超时。

### 约定即约束：用错就 panic / 报错

框架不为不支持的写法做兼容，而是让它们当场失败（完整列表见 [docs/design.md](docs/design.md) 2.7）。改代码时保持这个方向：新发现的坑优先加约束，而不是加兼容机制。

- `GetByXxx` / `ListByXxx` / `GetAll` / `Insert` 需要 ctx 里有事务，否则 panic；`SelectByXxx` / `SelectAll` 只能在 `dbpool.Query` 里、事务外调用，否则 panic；`dbpool.Query` 在事务里调用返回错误（事务改了数据、Query 却从池里另一条连接读，会读到旧值）。
- 嵌套 `Procedure` 返回 `ErrNestedTransaction`；改脱离事务的实体 panic；改了查询条件列再按它查同一张表 panic；`Insert` 一个已经插入过的对象 panic。
- 写业务逻辑时先确定这段代码在不在事务里，再选方法；同一事务里先查后改。

**回调里 panic 是错误传播方式，事务边界把它变成返回值**（同 goxdb 的 `abort`）：生成代码的查询和 `Insert` 遇到 SQL 错误时经 `dbctx.fail` 以 `*abort` panic 结束，`Procedure` / `Query` 接住它（`dbctx.AbortError`），回滚后把错误返回给调用方。回调里不需要逐个检查 SQL 错误，调用方用 `errors.Is` / `errors.As` 分类（23505、`ErrTimeout`、`context.Canceled`）。业务自己的 panic 和误用的 panic 不是 abort，回滚后原样重抛。新增会执行 SQL 的生成代码，出错时要走 `dbctx` 的辅助函数（`QueryOne` / `QueryList` / `InsertReturning`），不要在生成代码里直接 `panic(err)`。

### 事务与咨询锁（[dbpool/procedure.go](dbpool/procedure.go)）

`dbpool.Procedure(ctx, ProcedureOptions{Lock, Timeout}, fn)` 是事务的唯一入口（`WithTx` 是不加锁的简写）：拒绝嵌套 → 取当前连接池（没打开返回 `ErrNotOpen`）→ 加预算（`context.WithTimeoutCause(..., ErrTimeout)`）→ `BEGIN` 带上三个 `SET LOCAL` → 创建事务放进 ctx → 拿咨询锁 → 执行 `fn` → flush → COMMIT，提交成功后执行 AfterCommit。结构和 goxdb 的 `once()` 相同：正常路径在函数体里走完，`ended` 置位；defer 只处理没走到结尾的情况（abort、panic、`runtime.Goexit`），一律回滚（恢复内存 → ROLLBACK → AfterRollback）。`d.failed` 给没提交的结果计数，并给预算到期造成的错误套上 `ErrTimeout`。

- `statement_timeout` / `idle_in_transaction_session_timeout` / `lock_timeout` 拼进 BEGIN（不带参数的 Exec 走简单协议，多条语句一次往返），所有事务都生效。
- `transactionTimeoutMs` 是客户端预算，同时管 `Procedure` 和 `dbpool.Query` 回调，超过返回 `ErrTimeout`（调用方自己取消、到期时不带它，见 `overBudget`）。pgx 在 ctx 到期时只断开连接、不发 CancelRequest，服务端靠 `statement_timeout` 停下；`Query` 没有事务，服务端上限要用角色级 `statement_timeout`。COMMIT 用 `context.WithoutCancel(ctx)`：一旦发出去就必须等到结果，避免"不知道到底提交没有"导致重试重复发放；但最多等 `commitTimeoutMs`（默认 30s），等不到返回 `ErrCommitUnknown`，不回滚内存也不跑钩子。
- 锁用 `ProcedureOptions.Lock` 指定：`TryAdvisoryLock(key)` / `TryAdvisoryLock2(ns, id)` 拿不到返回 `ErrAdvisoryLockNotAcquired`；`AdvisoryLock(key)` / `AdvisoryLock2(ns, id)` 会阻塞等待（受 `lock_timeout` 限制，等超时同样返回 `ErrAdvisoryLockNotAcquired`）。try 锁失败是正常的"玩家忙"信号，业务直接返回稍后再试，不要在请求内退避重试；框架也不自动重试。key 的语义框架不关心，bigint 形式约定只给 uid 用。
- 这套锁是用来替代 `SELECT ... FOR UPDATE` 做玩家级串行的（README 有说明：玩家请求用 try 版，GM/RPC 写请求用阻塞版，只读请求走非事务的 `Query`）。
- 连接池是进程级的：`dbpool.Open(ctx, Config)` / `dbpool.Init(ctx, schema)`（= `Open(ctx, SchemaConfig(schema))`）打开，打开后的参数在 `db` 里只读，当前的 `db` 存在 `active`（atomic）。已经打开时再开返回 `ErrAlreadyOpen`。`Config` 的零值字段用 pgx 的默认值，只覆盖写了的。`Close` 必须排在服务优雅关闭**之后**，否则会把在途请求打死在半路；之后可以再 `Open`（测试里的 `reopen` 就是这样换配置的）。`DefaultQueryExecMode` 是 `pgx.QueryExecModeExec`（extended protocol、不缓存 prepared statement），配合 pgBouncer 的 transaction pooling。
- `dbpool.ReadStats()`：Procedure 按结果的计数（`procedureCounters`）和 pgxpool 的统计（design.md 3.3）。

### SQL 日志（[dbpool/logger.go](dbpool/logger.go)）

`showSQL="true"` 时用 `loggingSQLTx` 包一层，每条语句记一条 message 为 `sql` 的日志（成功 Info / 失败 Error，查不到不算失败），flush 的 Batch 合并成一条 `sql batch`；`$1` 占位符会被替换成真实字面量（`nil` 是 `NULL`），方便直接粘到 psql。`QueryRow` 拖到 `Scan` 时才记，因为错误要到 `Scan` 才拿得到。先用 `Enabled` 看级别会不会输出，不会就不格式化（`TestLogSkipsDisabledLevel` 守着）；日志带调用方的 ctx。日志落点用 `SetLogger` 换，任何时候都能调（`sqlLog` 是 atomic），不设就用调用时的 `slog.Default()`；钩子 panic 的日志也走它。

## 约定

- **atomic 只用在确实有无锁并发访问的地方**（同 goxdb）。现有的三处：`active`（每次 Procedure / Query 都读，只有 Open / Close 写）、`sqlLog`（`SetLogger` 随时写，每条日志都读）、`procedureCounters`（各个请求 goroutine 并发累加）。能由锁或单个 goroutine 保护的字段用普通字段：atomic 访问对 `-race` 不可见。新增 atomic 要在字段注释里写明谁在不持锁的情况下访问它。
- **打开之后的配置只读**：要换配置就 `Close` 再 `Open`，不要去改 `db` 的字段。
- 用 Go 1.27 的写法（写 Go 代码前先跑 modern-go-guidelines 的 `list`）。

## 已知缺口

- 只有事务级回滚；嵌套事务直接拒绝，savepoint / 部分回滚没有实现。框架不自动重试。
- 咨询锁的 bigint key 空间只有一个，约定只给 uid 用；其他业务锁用双 int32 形式。
- `dbpool.Query` 只有客户端预算，服务端上限要靠角色级 `statement_timeout`。
- 没写 `lock` 的表，行归哪把锁只靠约定：持有 A 的锁去改 B 的行不拦（design.md 2.8、第 5 节）。

## 参考实现

[example/logic/redenvelope/red_envelope.go](example/logic/redenvelope/red_envelope.go) 展示了推荐的业务写法：业务结构体内嵌 `*ptable.Xxx` 加一个 `readonly` 标志，`Get` 按 readonly 分派 `GetByXxx` / `SelectByXxx`，查不到时构造新实体并 `Insert`，之后所有修改只调 `SetXxx`；校验放在最前面，失败就返回错误让整个 Procedure 回滚。它有不连库的单元测试（跨天刷新、次数上限），业务逻辑可以照这个样子用 `NewXxx()` 建的对象测。
