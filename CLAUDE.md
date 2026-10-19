# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概览

pdbgen 是面向 PostgreSQL 的"代码生成 + 运行时"框架，目标场景是游戏服务端：把 `pdb.xml` 作为数据库表和 Go 结构体的唯一事实来源，生成建表/迁移 SQL 和一套带脏标记追踪、工作单元（Transaction）批量落库的 CRUD 代码。模块名 `pdbgen`，Go 1.27，唯一直接依赖是 `github.com/jackc/pgx/v5`。

运行时的语义承诺（事务、锁、重复行检测、欠账）记在 [docs/design.md](docs/design.md)：**改了运行时语义要同步改它**，并在它的"变更记录"里留痕。

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

# 测试（只有一个测试，需要 pdb.xml 里那套 PG 真的在跑，见 README 的 docker 命令）
go test ./...
go test ./dbpool -run TestPG -v
```

两个生成器的 `-schema` 默认值是 `./pdb.xml`，`genOutput` 是相对**当前工作目录**解析的，所以要在仓库根目录执行。已编译的 `gen/bean/genbeans.exe`、`gen/ddl/genddl.exe` 是提交进仓库的产物，README 里"cd 到生成器目录再 build"的写法只在显式传 `-schema` 时成立。

`ptable/*.go` 被提交进版本库，而生成器写的是 LF、仓库配置是 `core.autocrlf=true`（检出成 CRLF），所以重跑生成器会让这几个文件在 `git status` 里显示为已修改，实际内容逐字节相同（`git diff` 为空）。判断是否真有变化要先看 `git diff`，不要被 status 误导。运行时包自身（`dbctx`/`dbpool`）没有这种问题。

## 代码生成链路

```
pdb.xml ──readxml──┬─> gen/ddl  连库内省 → CREATE/ALTER DDL 打到 stdout，人工执行
                   └─> gen/bean 模板渲染 → ptable/*.go（每个 table 一个文件）
```

- [pdb.xml](pdb.xml) 同时是生成配置和表结构定义：`bean` 声明字段（Go 类型），`table` 把 bean 映射到表名、主键、索引。运行时参数（连接池、各类超时、`showSQL`、`appName`）也在这里。
- [readxml/xml.go](readxml/xml.go) 解析 XML，并提供命名转换与 `Field`/`TableGen`/`QueryMethod` 这些生成器的中间模型。改 XML 结构时 schema struct 要同步。
- [gen/ddl/main.go](gen/ddl/main.go) 只做增量：缺的表/列/索引才生成 DDL；已存在的列只做校验（类型不符、XML 要求 NOT NULL 但库里可空 → 直接报错），**不存在任何修改列类型或 DROP 的逻辑**，主键/索引定义对不上也是报错而不是重建。Go 类型映射：`int64→BIGINT`、`int32→INTEGER`、`int16/int8→SMALLINT`、`string→TEXT`，非空列生成 `NOT NULL DEFAULT` 默认值。
- [gen/bean/main.go](gen/bean/main.go) + [gen/bean/template.go](gen/bean/template.go) 生成 `ptable/<表名>.go`。模板是 Go 源码里的一个字符串常量，生成后经 `gofmt` 格式化。

生成代码对表结构的约定：

- 复合索引用逗号分隔写在 `index` 的 `variable` 上（`uid,actId`）。**unique 索引生成单行查询 `GetByXxx`/`SelectByXxx`，非 unique 索引生成列表查询 `ListByXxx`/`SelectListByXxx`**，主键固定是 `GetById`/`SelectById`。改 `unique` 会改变生成的 API 形态。
- 自增主键（`autoIncrement`）不出现在 INSERT 的字段列表里，靠 `RETURNING id` 回填；主键所有情况都不参与 UPDATE。
- `ptable/` 下的文件全部是生成产物，不要手改；改结构要改 `pdb.xml`、必要时改模板，然后重新生成并提交。

## 运行时架构

### 脏标记 + 事务工作集（[dbctx/](dbctx/)、[dbpool/transaction.go](dbpool/transaction.go)）

生成的结构体不是一个普通 ORM 实体，而是"当前值 + orig 快照 + dirty 位掩码"三件套（见 [ptable/user_red_envelope.go](ptable/user_red_envelope.go)）：

- `SetXxx` 值没变就提前返回（不置脏位），变了就 `dirty |= 1<<n`；getter 只读当前值。生成器按 bean 字段顺序分配位，`UpdateFields` 决定哪些字段可以脏。
- `TableName()` / `Identity()` / `FlushStmt()` / `FlushDone(rows)` 四个方法实现 `dbctx.Flusher`：`FlushStmt` 在 `dirty == 0` 或未 `loaded` 时返回空 query，否则按位拼出**只含脏列的 UPDATE**；`FlushDone` 回写 orig 快照、清 dirty，`rows == 0` 返回 `ErrXxxNotFound`；前两个给出行的身份（`Identity` 返回 0 表示主键未回填）。`ResetToLoaded()` 把实体回滚到快照。
- 查询路径（`dbctx.QueryOne` / `QueryList`）在返回前调 `dbctx.Track` 把实体登记进当前 ctx 的 Transaction；`Insert` 也登记自己。
- **同一事务里一行只允许有一个对象**：`Transaction.Register` 按 `(TableName(), Identity())` 检测，撞上就 panic（"后写的会静默覆盖先写的"）。它挡的是"同一行查两次 → 两个对象 → 两条 UPDATE 各写各的"这个唯一会静默丢数据的场景；检测在注册时触发、不看对象脏不脏，非事务路径不登记也不检测。这是使用约定 + 守卫，不是自动合并——业务代码应该复用第一次查出来的对象。细节见 [docs/design.md](docs/design.md)。
- `dbpool.Transaction` 按注册顺序、以指针去重收集实体，`Flush` 把所有脏 UPDATE 合并成一个 `pgx.Batch` 一次往返发出，再按顺序执行 `FlushDone`。任何一条失败即返回错误，由 `WithTx` 的 defer 回滚。所以业务代码只管 `SetXxx`，落库时机和合并都在提交前统一处理。
- `Update` 是绕过 Transaction 的立即落库入口，生成代码内部不调用它。
- 外部副作用（发包、RPC、事件）用 `dbctx.AfterCommit(ctx, fn)`，失败通知用 `dbctx.AfterRollback(ctx, fn)`。钩子同步执行、按注册顺序；`AfterCommit` 只在 COMMIT 明确成功后跑，COMMIT 返回错误时两个都不跑；钩子 ctx 不带事务和取消/超时。

### ctx 纪律：GetBy 与 SelectBy 互斥，用错就 panic

- `ptable.XxxTable.GetByXxx(ctx, ...)` 需要 ctx 里有事务，取 `dbctx.SQLTxFromCtx`；**不在事务里调用会 panic**（nil interface 断言）。
- `ptable.XxxTable.SelectByXxx(ctx, ...)` 需要 ctx 里没有事务，走 `dbctx.QuerierFromCtx`；**在事务里调用会主动 panic**（"SelectByXXX cannot be called inside transaction"）。
- `dbpool.Query` 同样拒绝在事务里调用，原因写在注释里：事务改了数据、Query 却从池里另一条连接读，会读到旧值。
- 这是设计上的强制约束，不是防御性代码。写业务逻辑时先确定这段代码在不在事务里，再选方法。

**panic 是这套框架的错误传播方式**：生成代码遇到 `pgx` 错误直接 panic，`WithTx` 的 defer `recover` 后先 `Rollback` 再原样 `panic(r)`。所以回调里不需要逐个检查 SQL 错误——要么正常返回，要么一路炸出去由上层 recover。

### 事务与咨询锁（[dbpool/dbpool.go](dbpool/dbpool.go)）

`WithTxPrepare(ctx, prepare, fn)` 是所有事务入口的公共实现：解析连接 → `Begin` → 把 tx 包装成带 SQL 日志的 `dbctx.SQLTx` 放进 ctx（`WithSQLTx`）→ 创建 Transaction 放进 ctx（`WithTransaction`）→ 执行 `prepare` 钩子 → 执行 `fn` → defer 里按"panic 回滚 / 有错回滚 / flush / commit"的顺序收尾，提交成功后执行 AfterCommit、回滚后执行 AfterRollback。

- 事务总预算由 `transactionTimeoutMs` 控制（`statement_timeout` 只管单条语句，PG 17 之前没有事务级 GUC），COMMIT 用 `context.WithoutCancel(ctx)`：一旦发出去就必须等到结果，避免"不知道到底提交没有"导致重试重复发放。
- `prepare` 钩子里用 `SET LOCAL` 下发 `statement_timeout`、`idle_in_transaction_session_timeout`、`lock_timeout`，然后拿咨询锁。
- 四个锁入口：`WithTryAdvisoryLock(key)` / `WithTryAdvisoryLock2(ns, id)` 拿不到返回 `ErrAdvisoryLockNotAcquired`；`WithAdvisoryLock` / `WithAdvisoryLock2` 会阻塞等待。单 bigint 与双 int32 的内置签名差异由 `lockArgs` 抽象（`lockKey` / `lockKey2`）处理，key 的语义框架不关心。
- 这套锁是用来替代 `SELECT ... FOR UPDATE` 做玩家级串行的（README 有说明：玩家请求用 try 版，GM/RPC 写请求用阻塞版，只读请求走非事务的 `Query`）。
- 连接池是包级变量，`Init` 每进程调一次；`Close` 必须排在服务优雅关闭**之后**，否则会把在途请求打死在半路。`DefaultQueryExecMode` 被设成 `pgx.QueryExecModeExec`（简单协议），配合 pgBouncer 的 transaction pooling。

### SQL 日志（[dbpool/logger.go](dbpool/logger.go)）

`showSQL="true"` 时用 `loggingDB` 包一层，每条语句记一条 message 为 `sql` 的日志（成功 Info / 失败 Error），Batch 合并成一条 `sql batch`；`$1` 占位符会被替换成真实字面量，方便直接粘到 psql。`QueryRow` 拖到 `Scan` 时才记（只调不扫的话语句根本没发出去）。日志落点用 `SetLogger` 换，不设就用调用时的 `slog.Default()`。

## 已知缺口（README todo）

- 咨询锁的 `hashtext` 拼接在 uid 超过 int32 时会出错，预想改成"默认用 uid、其他业务锁用枚举值"。
- 一次请求涉及多表修改时，`ResetToLoaded` 只回滚单个实体，还没有 ctx 级别的回滚日志。

## 参考实现

[example/logic/redenvelope/red_envelope.go](example/logic/redenvelope/red_envelope.go) 展示了推荐的业务写法：业务结构体内嵌 `*ptable.Xxx` 加一个 `readonly` 标志，`Get` 按 readonly 分派 `GetByXxx` / `SelectByXxx`，查不到时构造新实体并 `Insert`，之后所有修改只调 `SetXxx`。
