# Procedure 与 Transaction：职责划分与命名（设计提案）

> 状态：提案，Phase 1（命名迁移）、Phase 2（AfterCommit / AfterRollback）、Phase 3（身份表）、Phase 4（Procedure 入口）、Phase 5（memento 回滚）已实施；其余阶段未实施。
> 参考实现：`C:\Dev\xdb.jar.src1`（xdb）的 `Procedure` / `Transaction` / `Savepoint`。
> 实施时：同步修改 [design.md](design.md)，并在它的"变更记录"里留痕。

## 1. 问题

pdbgen 原来有两个层面的"事务"，但名字没有体现出来：

- `dbctx.TX`：数据库事务，执行 SQL（pgx.Tx 的包装）；
- `dbpool.UnitOfWork`：事务内的脏对象收集器和批量落库器。

`UnitOfWork` 这个名字不够直接。它实际承担的是"一次业务事务的工作集"——身份表、脏对象、回滚、提交后任务。xdb 把这一层直接叫 `Transaction`；pdbgen 沿用 `Transaction` 更自然，但需要给数据库事务让出名字。Phase 1 已完成改名：SQL 事务叫 `SQLTx`，业务事务工作集叫 `Transaction`。

## 2. 命名结论

- **Procedure**：一次业务事务（入口、锁、超时、重试、提交/回滚后任务）。对应 xdb 的 `Procedure`，对应今天 `dbpool.Procedure` 的入口 + 回调。
- **Transaction**：事务内工作集（原 `UnitOfWork`）：身份表、脏对象、savepoint/回滚、提交/回滚后任务。对应 xdb 的 `Transaction` + `Savepoint`。
- **SQLTx**：数据库事务（原 `dbctx.TX`）：执行 SQL、提交/回滚。对应 pgx.Tx。
- **Flusher**：可落库实体。对应 xdb 的 `TRecord`。
- **Persister**：异步落库器（未来）。对应 xdb 的 `TStorage.changed` + `Checkpoint`。

一句话：**Procedure 拥有一个 Transaction；Transaction 操作一个 SQLTx；SQLTx 管理一组 Flusher。**

### 2.1 为什么把 TX 改成 SQLTx

`Transaction` 和 `SQLTx` 是两种不同的东西：

- `SQLTx` 的生命周期由 `Begin/Commit/Rollback` 决定，关心的是 SQL 原子性；
- `Transaction` 的生命周期由 `Procedure` 决定，关心的是内存工作集的原子性、身份表、回滚和提交后任务。

如果继续用 `TX` 表示 SQL 事务、用 `Transaction` 表示工作集，两个名字在同一段代码里会持续混淆。把 SQL 类型显式命名为 `SQLTx`，把 `Transaction` 留给业务语义，读起来更直接。

### 2.2 迁移映射

| 旧名字 | 新名字 | 说明 |
|---|---|---|
| `dbctx.TX` | `dbctx.SQLTx` | 接口类型 |
| `dbctx.TxFromCtx` | `dbctx.SQLTxFromCtx` | 取 SQL 事务 |
| `dbctx.WithTx` | `dbctx.WithSQLTx` | 把 SQL 事务放进 ctx |
| `dbctx.HasTx` | `dbctx.HasSQLTx` | 是否在 SQL 事务内 |
| `dbctx.UnitOfWork` | `dbctx.Transaction` | 接口 |
| `dbctx.WithUnitOfWork` | `dbctx.WithTransaction` | 把工作集放进 ctx |
| `dbctx.Track` | `dbctx.GetOrAddCachedRecord` | 返回规范对象；`Track` 保留为 Deprecated 别名 |
| `dbpool.UnitOfWork` | `dbpool.Transaction` | 具体实现 |
| `dbpool.NewUnitOfWork` | `dbpool.NewTransaction` | 构造函数 |
| `dbpool/uow.go` | `dbpool/transaction.go` | 文件 |
| `dbpool/uow_test.go` | `dbpool/transaction_test.go` | 测试文件 |

迁移方式：先加新名字，旧名字保留为 `// Deprecated:` 别名；改运行时和生成模板；重新生成 `ptable/*.go`；确认无引用后删别名。这样每一步都能编译、测试。

Phase 1 已按这个方式执行：运行时、模板和 `ptable/*.go` 都已使用新名字，旧名保留为 Deprecated 别名。

## 3. 概念对照

### 3.1 xdb 的请求路径

```
Procedure.call()                         Procedure.java:213
  ├─ Transaction.begin + savepoint       Procedure.java:124
  ├─ TTable.get/add/remove
  │    ├─ 事务内 cachedTRecord（身份表）   Transaction.java:84
  │    ├─ Lockey.checkAndLock            Lockey.java:103
  │    └─ 修改字段 → Savepoint 注册 undo Log
  ├─ 成功 → Savepoint.commit 按顺序执行
  └─ 失败 → rollback 逆序恢复旧值         Savepoint.java:11-25
```

- `Transaction` 是线程级的：`Transaction.current()`，由 `Procedure.call()` 创建。
- `Procedure` 是业务层：子类实现 `process()`，框架负责 begin/commit/rollback/retry。
- xdb 没有独立的 SQL 事务类型；它的 `Transaction` 是内存事务。

### 3.2 pdbgen 的现状与目标

| 概念 | 现状（Phase 5 后） | 目标 |
|---|---|---|
| 业务事务 | `dbpool.Procedure`（锁 / 超时）+ 回调 | 增加重试 |
| 内存工作集 | `dbpool.Transaction`：身份表 + 脏对象 + memento 回滚 + 提交后任务 | 增加嵌套 savepoint（可选） |
| SQL 事务 | `dbctx.SQLTx` | 不变 |
| 实体 | 生成结构体 + `Flusher` | 不变 |

- `Procedure` 负责入口、锁和单次预算；`WithTxPrepare` 负责底层的 Begin、flush、commit/rollback。
- `Transaction.GetCachedRecord` / `AddCachedRecord` 解析身份表，`Flush` 在提交前批量发 UPDATE。
- 已经有身份表、提交后任务、统一入口和 memento 回滚；还没有嵌套 savepoint / 重试。

### 3.3 目标职责

| 能力 | Procedure | Transaction | SQLTx |
|---|---|---|---|
| 决定事务边界 | 是 | 否 | 被调用 |
| 获取锁 / 超时 / 重试 | 是 | 否 | 否 |
| 业务逻辑 | 调用 `fn` | 否 | 否 |
| 身份表 | 否 | 是 | 否 |
| 收集脏对象 | 否 | 是 | 否 |
| savepoint / 回滚内存状态 | 否 | 是 | 否 |
| 提交后/回滚后任务 | 触发 | 保存与执行 | 否 |
| 执行 SQL / 提交 | 否 | 否（只生成语句） | 是 |
| 拼 UPDATE / FlushDone | 否 | 协调 | 否 |

## 4. 目标语义

### 4.1 Procedure

- 负责：事务入口、锁策略（单/双 key、try/block）、超时、重试、调用业务函数、按结果提交或回滚。
- 不负责：收集脏对象、拼 SQL、维护身份表。
- **重试语义**：与 xdb 一致，遇到可重试错误（锁冲突、序列化失败）时整段重新执行。因此 `fn` 必须可重入，所有外部副作用（发包、RPC、事件）必须放到 `AfterCommit`。重试依赖内存回滚（Phase 5 已实现）；`Procedure` 目前还没有接入重试。
- **提交结果不确定**：COMMIT 发出后网络断开，不能当作成功，也不能重跑业务。保持现有 `context.WithoutCancel` 的处理方式；`AfterCommit` 只在 COMMIT 明确成功后运行。

### 4.2 Transaction

- 负责：
  - 身份表：`(table, identity) -> Flusher`，同一行返回同一对象；
  - 脏对象收集；
  - savepoint 栈与内存回滚；
  - 提交/回滚后任务队列；
  - 同步模式下协调 `Flush`。
- 不负责：SQL 事务的 begin/commit/rollback（那是 Procedure + SQLTx 的职责）。
- 生命周期：由 Procedure 创建，放进 ctx，随 Procedure 结束而销毁。

### 4.3 SQLTx

- 保持现状：`dbctx.SQLTx` 是带 SQL 日志的 pgx.Tx 包装。
- 不把 Transaction 和 SQLTx 合并：
  - Transaction 未来可能跨多个 SQLTx（write-behind 的批量落库）；
  - 只读路径可能只有 SQLTx、没有 Transaction（`dbpool.Query`）。

### 4.4 Flusher

- 保持现状：生成代码实现 `TableName` / `Identity` / `FlushStmt` / `FlushDone`。
- 未来异步化时再加 `CaptureDirty` / `ConfirmFlushed`，不改同步路径语义。

## 5. 生命周期

```
Procedure(ctx, opts, fn)
  ├─ begin SQLTx
  ├─ txn := NewTransaction()
  │    ctx = WithSQLTx(ctx, sqlTx)
  │    ctx = WithTransaction(ctx, txn)
  ├─ prepare: SET LOCAL ...; acquire lock
  ├─ fn(ctx)
  │    ├─ GetByXxx → QueryOne → GetOrAddCachedRecord
  │    │    └─ txn.GetCachedRecord / txn.AddCachedRecord
  │    ├─ SetXxx → dirty |= ...；必要时记录回滚快照
  │    └─ AfterCommit(fn) / AfterRollback(fn)
  ├─ 成功:
  │    ├─ txn.Flush(ctx)          // 同步模式：Batch UPDATE
  │    ├─ SQLTx.Commit(WithoutCancel)
  │    └─ 执行 AfterCommit 队列
  └─ 失败 / panic:
       ├─ txn.Rollback()         // 恢复内存对象
       ├─ SQLTx.Rollback()
       └─ 执行 AfterRollback 队列
```

顺序约束：

- `Flush` 必须在 `SQLTx.Commit` 之前；`Flush` 失败 → 整体回滚。
- `AfterCommit` 只在 COMMIT 明确成功后执行。任务失败不能回滚已经提交的数据，只能记录日志；是否向调用方返回错误由业务决定。
- `AfterRollback` 在 SQLTx 回滚之后执行，不能假设数据库里还有未回滚的数据。
- panic 传播保持现状：先回滚，再原样重抛。

## 6. 回滚：整体回滚，不做字段级 undo log

### 6.1 业务代码如何使用回滚

参考 aotu_gs 的 [RedPackRainRole.java](../master/aotu_gs/src/ares/logic/activity/redpackrain/RedPackRainRole.java)：

- `getRedPackRainRole` 在加载时就调 `tryClear()` / `refresh()` 修改实体；
- `click()` 先做完全部校验，最后才 `setTodaycount(+1)`；
- 中途 `distributeAllAward` 可能已经改了道具、货币等多张表；
- 任何一步失败都 `return false`，由 [MsgRedpackrain.java](../master/aotu_gs/src/ares/logic/activity/redpackrain/MsgRedpackrain.java) 的返回值成为 `Procedure.process()` 的返回值，整段回滚；
- 成功路径的发包用 `sendMsgWhileCommit`（[CommonUtil.java](../master/aotu_common/src/ares/logic/util/CommonUtil.java)），提交后才发。

业务代码从头到尾没有"撤销某个字段"的需求，也不应该依赖部分回滚。它需要的是：**一次 Procedure 成功就全部生效，失败就全部不生效。**

### 6.2 那 xdb 为什么按字段记 Log

因为它的对象在缓存里长期存活：

- 事务失败后，缓存不能留下半改状态；
- 嵌套 Procedure 需要 savepoint 做部分回滚；
- 锁冲突重试时，不能带着上一次的脏状态重跑。

字段级 Log 是**实现整体回滚的手段**，不是业务 API。它让"恢复整条记录"只写回实际改过的字段，省内存；但业务从不使用这个粒度。

### 6.3 pdbgen 的选择：整体回滚 + memento

- 需要内存回滚时，用 memento：`Track` 时给实体拍整条快照（字段值 + orig + dirty + id + loaded），回滚时整条恢复。
- 不修改 `SetXxx` 签名，不在实体里塞 Transaction 指针，不给 setter 加 `ctx`。
- 嵌套 savepoint 如果需要，按层保存快照；没有嵌套需求就只保留 Procedure 级快照。
- 字段级 undo log 明确列为**非目标**。

### 6.4 当前阶段是否真的需要内存回滚

| 场景 | 失败后内存对象会怎样 | 是否需要 memento |
|---|---|---|
| 当前模型：对象随请求创建、随请求丢弃 | 对象不可达，DB 事务已回滚 | 不需要 |
| 共享缓存 / write-behind | 缓存里的对象会留下半改状态 | 必需 |
| Procedure 重试 | 重跑可能拿到同一个缓存对象 | 必需 |

Phase 5 已实现 memento；它是后续重试和共享缓存的基础。当前没有重试/缓存时，DB 回滚 + 丢弃对象也够用，但 memento 让失败后的内存状态与数据库保持一致，避免把脏对象带到下一次使用。

### 6.5 必须处理的边界：Insert 后回滚

生成代码的 `Insert` 是在 SQL 成功、主键回填后才 `Track` 的。如果按 Track 时快照，回滚后对象仍然带着 `id` 和 `loaded=true`，但库里没有这行。处理方式：

- `Insert` 在发 SQL 前把实体登记为"新建"（identity=0、loaded=false 的初始状态）；
- 回滚时对"新建"实体做 `ResetNew`：清 `id`、`loaded`、`dirty`，并从 Transaction 身份表移除；
- 对象如果随后被丢弃，这一步只是保险；如果对象在缓存/重试中被复用，它是必需的。

### 6.6 业务约束

- 校验失败就返回错误或让 panic 传播，**不要在 Procedure 内部 `recover` 事务错误后继续用同一批对象**。xdb 的注释也明确写着"不能捕获 XError 及其子类"。
- 对外副作用（发包、RPC、事件）走 `AfterCommit`；失败通知走 `AfterRollback`。
- 不要用 `ResetToLoaded` 在业务里做局部撤销——那是绕过 Transaction 的写法。

## 7. API 形状（提案）

### 7.1 Procedure 入口

```go
type ProcedureOptions struct {
    Lock    LockSpec      // 零值 = 不加锁
    Timeout time.Duration // <=0 表示只用 transactionTimeoutMs
}

func Procedure(ctx context.Context, opts ProcedureOptions, fn func(context.Context) error) error

// 锁构造器
TryAdvisoryLock(key int64) LockSpec
AdvisoryLock(key int64) LockSpec
TryAdvisoryLock2(ns, id int32) LockSpec
AdvisoryLock2(ns, id int32) LockSpec
```

- 底层复用 `WithTxPrepare`。
- `WithTx` / `WithTryAdvisoryLock*` / `WithAdvisoryLock*` 保留为薄封装，避免一次性改所有调用点。
- **Retry 推迟到 Phase 5**：重试必须先把内存状态回滚干净，所以和 memento 一起做；Phase 4 的 `fn` 只执行一次。
- 如果保留嵌套语义，嵌套 `Procedure` 自动创建 savepoint；不需要嵌套时，Phase 5 只做 Procedure 级回滚，savepoint API 延后。

### 7.2 Transaction 接口

```go
type Transaction interface {
    // GetCachedRecord 返回事务里已经缓存的记录（xdb: getCachedTRecord）。
    GetCachedRecord(tableName string, id int64) (Flusher, bool)

    // AddCachedRecord 把记录放进事务缓存并返回规范记录
    // （xdb: addCachedTRecord）。同一行已经有记录时返回已有记录。
    AddCachedRecord(obj Flusher) Flusher

    // RegisterNew 在 Insert 之前登记新建实体，回滚时恢复为未加载状态。
    RegisterNew(obj Flusher)

    // Rollback 恢复本次事务内所有缓存记录的快照。
    Rollback()

    AfterCommit(fn func(context.Context))
    AfterRollback(fn func(context.Context))
}
```

`Savepoint` / `RollbackTo` 仍未实现；Phase 5 只做 Procedure 级整体回滚。

`GetOrAddCachedRecord` 是 `getCachedTRecord` + `addCachedTRecord` 的组合入口；
`GetCachedRecordAs` 是缓存优先查询的泛型入口（生成代码的 `GetById` 用它）：

```go
func GetOrAddCachedRecord[T Flusher](ctx context.Context, obj T) T
func GetCachedRecordAs[T Flusher](ctx context.Context, tableName string, id int64) (T, bool)
```

`QueryOne` / `QueryList` 用返回值替换扫描出来的对象。这样"同一行一个对象"从 panic 守卫变成身份表保证。

### 7.3 生成代码变化

- `QueryOne` / `QueryList`：`obj = dbctx.GetOrAddCachedRecord(ctx, obj)`。
- `GetById`：先 `dbctx.GetCachedRecordAs[*Xxx]`，命中直接返回，不发 SELECT；未命中才走原查询。
- 唯一索引查询（`GetByYyy`）仍先查库，再靠 `GetOrAddCachedRecord` 解析成已有对象。
- `SnapshotState()` / `RestoreState()` / `ResetNew()` 已生成；第一次登记时保存快照，回滚时恢复。
- `Insert` 在发 SQL 前 `RegisterNew`（保存插入前状态），成功后回填主键并更新身份表；回滚时 `ResetNew`。
- `FlushStmt` / `FlushDone` 暂不变。

## 8. 分阶段落地

| 阶段 | 内容 | 验收 |
|---|---|---|
| 0 | 安全网：跑通现有测试，补齐事务/回滚/重复行用例；代码不动 | `go test ./...` 全绿；无 PG 时 skip |
| 1 | **已完成**。命名迁移：`TX→SQLTx`、`UnitOfWork→Transaction`；保留 Deprecated 别名；改模板并重新生成 `ptable/*.go` | `go build/vet/test` 全绿；行为无变化 |
| 2 | **已完成**。AfterCommit / AfterRollback；不碰生成实体 | 提交后可见、回滚不触发、panic 路径、hook panic 不影响其他 hook |
| 3 | **已完成**。Transaction 身份表（`GetCachedRecord` / `AddCachedRecord`）；`GetOrAddCachedRecord` 返回规范对象；`GetById` 缓存优先；移除重复行 panic | 单元测试 + 真实 PG 的"同事务两次查询返回同一指针、第二次 GetById 不发 SELECT"测试 |
| 4 | **已完成**。`Procedure` 入口（锁/超时）；先不做重试 | 锁参数和超时行为与现有 `With*Lock*` 一致 |
| 5 | **已完成**。整事务 memento 回滚 + `ResetNew`；嵌套 savepoint 仍未实现 | 多对象修改回滚测试；Insert 后回滚测试；AfterRollback 看到恢复后的值 |
| 6 | （可选）Persister / write-behind，与 Procedure 解耦 | 独立设计，不在本文范围 |

每个阶段：

- `go build ./...`、`go vet ./...`、`go test ./...`；
- 更新 [design.md](design.md) 并在"变更记录"里留痕；
- 涉及生成代码的，重新生成 `ptable/*.go` 并提交（注意 CRLF 导致的假修改）。

## 9. 对现有语义的影响

1. **重复行解析**：身份表命中后直接返回已有对象，不再 panic；design.md 2.1 / 2.2 已经改写并留了变更记录。事务级身份表同时承担去重和 read-your-writes，后续内存回滚也会以它为边界。
2. **Flush 时机**：Phase 1-5 不变，仍在 COMMIT 前同步 Flush；Phase 6 才讨论异步。
3. **错误传播**：保持 panic 传播；Procedure 只捕获可重试错误，其他错误原样抛出。
4. **重试与副作用**：重试尚未接入；接入后 `fn` 可能被执行多次，所有对外副作用必须走 `AfterCommit`，否则重试会重复。
5. **锁顺序**：Procedure 仍然只挂一把锁；多锁场景由业务保证顺序，与 design.md 第 3 节一致。
6. **身份表的作用域**：Transaction 是事务级的，不跨事务；跨请求共享对象属于 write-behind/会话工作集，不在本提案范围内。
7. **业务失败语义**：校验不通过返回错误，由 Procedure 整体回滚；业务不要在事务内 `recover` 后继续。参考 `RedPackRainRole.click()`：校验前置，失败直接返回 false。

## 10. 非目标与风险

- 不把 `SQLTx` 改名成 `Transaction`，避免与内存事务混淆。
- 不把 Transaction 和 Procedure 合并成一个类型：生命周期和职责都不同。
- `Procedure` 在 Go 里是函数 + options，不是类；不要为了名字造一个需要继承的结构。
- memento 方案在"大量实体 + 多层 savepoint"时是 O(n×m)，需要压测；如果开销不可接受，先限制嵌套层数，再考虑更细的记账机制。
- 失败策略：Phase 1-5 仍是"错误回滚、向上抛"；write-behind 的 fail-stop/降级策略在 Phase 6 单独设计。

## 11. 待决问题

1. `Procedure` 的锁参数是统一成 `LockSpec`，还是保留现有四个 `With*Lock*` 函数作为入口？
2. 重试范围：只重试 `ErrAdvisoryLockNotAcquired`，还是也包括 PG 的 serialization failure / deadlock？后者需要业务回调严格幂等。
3. 嵌套 savepoint 是否要做？Phase 5 已实现 Procedure 级整体回滚；参考业务实现没有用到部分回滚，`Savepoint` / `RollbackTo` 继续延后。
4. ~~`QueryList` 遇到重复行时，是返回同一指针两次，还是去重？~~ 已决定：与 xdb 一致，返回同一指针；`docs/design.md` 已注明列表元素可能重复。
5. Transaction 是否向业务暴露 `Savepoint` / `RollbackTo`，还是只允许 Procedure 内部使用？
6. `AfterCommit` 任务 panic 时：记录日志并继续执行后续任务，还是中断？建议记录日志、继续执行所有任务，最后把 panic 汇总给调用方。
