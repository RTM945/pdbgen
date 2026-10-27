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

# 生成 DDL：连接 pdb.xml 里的 url 读库里的真实结构，把 SQL 打到 stdout（不写文件、不执行）
cd gen/ddl && go build -o genddl.exe && cd ../..
./gen/ddl/genddl.exe -schema ./pdb.xml > ddl.sql

# 生成 Go 数据表代码：写入 pdb.xml 的 genOutput 目录（默认 ./ptable）
cd gen/bean && go build -o genbeans.exe && cd ../..
./gen/bean/genbeans.exe -schema ./pdb.xml

# 测试。dbpool 的集成测试连 pdb.xml 里那套 PG（见 README 的 docker 命令）：
# 连不上默认 skip；PDBGEN_REQUIRE_DB=1 时直接失败，确认改动真的在库上跑过时用它
go test ./...
PDBGEN_REQUIRE_DB=1 go test ./...
```

两个生成器的 `-schema` 默认值是 `./pdb.xml`，`genOutput` 是相对**当前工作目录**解析的，所以要在仓库根目录执行。已编译的 `gen/bean/genbeans.exe`、`gen/ddl/genddl.exe` 是提交进仓库的产物，README 里"cd 到生成器目录再 build"的写法只在显式传 `-schema` 时成立。改了模板或运行时接口（`dbctx.Flusher` 等）要重新编译 `genbeans.exe`，并重新生成用到 pdbgen 的项目的 `ptable`（go_pg_test 的 `bin/genbeans.exe` 是它的一份拷贝），否则旧的生成代码编不过。

`ptable/*.go` 被提交进版本库，而生成器写的是 LF、仓库配置是 `core.autocrlf=true`（检出成 CRLF），所以重跑生成器会让这几个文件在 `git status` 里显示为已修改，实际内容逐字节相同（`git diff` 为空）。判断是否真有变化要先看 `git diff`，不要被 status 误导。运行时包自身（`dbctx`/`dbpool`）没有这种问题。

## 代码生成链路

```
pdb.xml ──readxml──┬─> gen/ddl  连库内省 → CREATE/ALTER DDL 打到 stdout，人工执行
                   └─> gen/bean 模板渲染 → ptable/*.go（每个 table 一个文件）
```

- [pdb.xml](pdb.xml) 同时是生成配置和表结构定义：`bean` 声明字段（Go 类型），`table` 把 bean 映射到表名、主键、索引。运行时参数（连接池、各类超时、`showSQL`、`appName`）也在这里。
- [readxml/xml.go](readxml/xml.go) 只负责解析 XML 和命名转换；生成器的中间模型（`field` / `tableGen` / `queryMethod`）在 `gen/bean` 里。改 XML 结构时 schema struct 要同步。
- [gen/ddl/main.go](gen/ddl/main.go) 只做增量：缺的表/列/索引才生成 DDL；已存在的列只做校验（类型不符、XML 要求 NOT NULL 但库里可空 → 直接报错），**不存在任何修改列类型或 DROP 的逻辑**，主键/索引定义对不上也是报错而不是重建。Go 类型映射：`int64→BIGINT`、`int32→INTEGER`、`int16/int8→SMALLINT`、`string→TEXT`、`bool→BOOLEAN`、`float32→REAL`、`float64→DOUBLE PRECISION`，非空列生成 `NOT NULL DEFAULT` 默认值（bool 默认 `false`，数值默认 `0`，文本默认 `''`）。
- [gen/bean/main.go](gen/bean/main.go) + [gen/bean/template.go](gen/bean/template.go) 生成 `ptable/<表名>.go`。模板是 Go 源码里的一个字符串常量，生成后经 `gofmt` 格式化。框架不支持的 schema 在这里直接报错（规则见 [docs/design.md](docs/design.md) 第 4 节），不要为了兼容某种 schema 去放宽它。

生成代码对表结构的约定：

- 复合索引用逗号分隔写在 `index` 的 `variable` 上（`uid,actId`）。**unique 索引生成单行查询 `GetByXxx`/`SelectByXxx`，非 unique 索引生成列表查询 `ListByXxx`/`SelectListByXxx`**，主键固定是 `GetById`/`SelectById`。改 `unique` 会改变生成的 API 形态。
- 主键必须是 `int64` 且 `autoIncrement="true"`：不出现在 INSERT 的字段列表里，靠 `RETURNING id` 回填，也不参与 UPDATE。`primaryKey` 的 `start` / `step` 生成 `START WITH` / `INCREMENT BY`；只影响 identity 首次创建，已存在的 identity 不会被自动修改。
- `variable` 上的 `sensitive="true"`（只能是非主键的 string 列）让生成代码把参数包成 `dbctx.Secret`，SQL 日志里只打成 `'***'`。
- `ptable/` 下的 `<表名>.go` 全部是生成产物，不要手改；改结构要改 `pdb.xml`、必要时改模板，然后重新生成并提交。`ptable/delete_test.go` 是手写测试。

## 运行时架构

### 脏标记 + 事务工作集（[dbctx/](dbctx/)、[dbpool/transaction.go](dbpool/transaction.go)）

生成的结构体是"当前值 + dirty 位掩码 + 三个状态位（loaded / deleted / detached）"（见 [ptable/user_red_envelope.go](ptable/user_red_envelope.go)）：

- `SetXxx` 值没变就不置脏位，变了就 `dirty |= 1<<n`；实体脱离事务时 panic。getter 只读当前值。生成器按 bean 字段顺序给除主键外的列分配位。
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

**panic 是这套框架的错误传播方式**：生成代码遇到 `pgx` 错误直接 panic，`Procedure` 的 defer `recover` 后先回滚再原样 `panic(r)`。所以回调里不需要逐个检查 SQL 错误——要么正常返回，要么一路炸出去由上层 recover。

### 事务与咨询锁（[dbpool/procedure.go](dbpool/procedure.go)）

`dbpool.Procedure(ctx, ProcedureOptions{Lock, Timeout}, fn)` 是事务的唯一入口（`WithTx` 是不加锁的简写）：拒绝嵌套 → 加预算 → `BEGIN` 带上三个 `SET LOCAL` → 创建事务放进 ctx → 拿咨询锁 → 执行 `fn` → defer 里 flush、COMMIT 或回滚，提交成功后执行 AfterCommit、回滚后执行 AfterRollback。

- `statement_timeout` / `idle_in_transaction_session_timeout` / `lock_timeout` 拼进 BEGIN（不带参数的 Exec 走简单协议，多条语句一次往返），所有事务都生效。
- `transactionTimeoutMs` 是客户端预算，同时管 `Procedure` 和 `dbpool.Query` 回调。pgx 在 ctx 到期时只断开连接、不发 CancelRequest，服务端靠 `statement_timeout` 停下；`Query` 没有事务，服务端上限要用角色级 `statement_timeout`。COMMIT 用 `context.WithoutCancel(ctx)`：一旦发出去就必须等到结果，避免"不知道到底提交没有"导致重试重复发放。
- 锁用 `ProcedureOptions.Lock` 指定：`TryAdvisoryLock(key)` / `TryAdvisoryLock2(ns, id)` 拿不到返回 `ErrAdvisoryLockNotAcquired`；`AdvisoryLock(key)` / `AdvisoryLock2(ns, id)` 会阻塞等待（受 `lock_timeout` 限制）。try 锁失败是正常的"玩家忙"信号，业务直接返回稍后再试，不要在请求内退避重试；框架也不自动重试。key 的语义框架不关心，bigint 形式约定只给 uid 用。
- 这套锁是用来替代 `SELECT ... FOR UPDATE` 做玩家级串行的（README 有说明：玩家请求用 try 版，GM/RPC 写请求用阻塞版，只读请求走非事务的 `Query`）。
- 连接池是包级变量，`Init` 每进程调一次；`Close` 必须排在服务优雅关闭**之后**，否则会把在途请求打死在半路。`DefaultQueryExecMode` 是 `pgx.QueryExecModeExec`（extended protocol、不缓存 prepared statement），配合 pgBouncer 的 transaction pooling。

### SQL 日志（[dbpool/logger.go](dbpool/logger.go)）

`showSQL="true"` 时用 `loggingSQLTx` 包一层，每条语句记一条 message 为 `sql` 的日志（成功 Info / 失败 Error，查不到不算失败），flush 的 Batch 合并成一条 `sql batch`；`$1` 占位符会被替换成真实字面量（`nil` 是 `NULL`，`dbctx.Secret` 是 `'***'`），方便直接粘到 psql。`QueryRow` 拖到 `Scan` 时才记，因为错误要到 `Scan` 才拿得到。日志落点用 `SetLogger` 换，不设就用调用时的 `slog.Default()`。

## 已知缺口

- 只有事务级回滚；嵌套事务直接拒绝，savepoint / 部分回滚没有实现。框架不自动重试。
- 咨询锁的 bigint key 空间只有一个，约定只给 uid 用；其他业务锁用双 int32 形式。
- `dbpool.Query` 只有客户端预算，服务端上限要靠角色级 `statement_timeout`。

## 参考实现

[example/logic/redenvelope/red_envelope.go](example/logic/redenvelope/red_envelope.go) 展示了推荐的业务写法：业务结构体内嵌 `*ptable.Xxx` 加一个 `readonly` 标志，`Get` 按 readonly 分派 `GetByXxx` / `SelectByXxx`，查不到时构造新实体并 `Insert`，之后所有修改只调 `SetXxx`。
