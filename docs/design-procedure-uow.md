# Procedure 与 UnitOfWork：职责划分与命名（设计提案）

> 状态：提案，未实施。
> 参考实现：`C:\Dev\xdb.jar.src1`（xdb）的 `Procedure` / `Transaction` / `Savepoint`。
> 实施时：同步修改 [design.md](design.md)，并在它的"变更记录"里留痕。

## 1. 问题

pdbgen 目前把"业务事务"和"变更集合"混在一个概念里：

- `dbpool.WithTxPrepare(ctx, prepare, fn)` 是事务入口，`fn` 是业务逻辑；
- `dbpool.UnitOfWork` 是事务内的脏对象收集器和批量落库器。

xdb 把这两件事拆开了：

- `Procedure` 是业务事务单元：`process()`、锁、重试、提交/回滚任务；
- `Transaction` + `Savepoint` + `Log` 才是变更跟踪、身份表、回滚日志。

所以 **Procedure 并不等价于 UnitOfWork**。Procedure 对应的是 `WithTxPrepare` 的入口和回调，UoW 对应的是 xdb 的 `Transaction`。把两者都叫 UoW 会让"业务事务"和"变更集合"混为一谈；把 UoW 改名叫 Procedure 又会丢掉变更跟踪的语义。

## 2. 结论

采用**组合命名**，不合并：

| 名字 | 职责 | xdb 对应 | pdbgen 现状 |
|---|---|---|---|
| `Procedure` | 一次业务事务：入口、锁、超时、重试、提交/回滚后任务 | `Procedure` | `WithTxPrepare` + 回调 |
| `UnitOfWork` | 事务内工作集：身份表、脏对象、savepoint/回滚 | `Transaction` + `Savepoint` | `dbpool.UnitOfWork` |
| `TX` | 数据库事务：SQL 原子性、提交结果 | 无（嵌入式存储） | `dbctx.TX` / `pgx.Tx` |
| `Flusher` | 可落库实体 | `TRecord` | 生成结构体 |
| `Persister` | 异步落库器（未来） | `TStorage.changed` + `Checkpoint` | 暂无 |

一句话：**Procedure 拥有一个 UoW；UoW 操作一个 TX；TX 管理一组 Flusher。**

不把 `dbctx.TX` 改名成 `Transaction`：pdbgen 里已经有一个"Transaction"语义的数据库事务，再引入一个同名的内存事务会持续误导。

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
- `UnitOfWork` 的职责在 xdb 里被拆成三块：身份表在 `Transaction.cachedTRecord`，脏对象在 `TStorage.changed`，回滚日志在 `Savepoint`。

### 3.2 pdbgen 的现状

- `WithTxPrepare` 同时承担事务入口、UoW 创建、flush、commit/rollback。
- `UnitOfWork.Register` 收集对象，`Flush` 在提交前批量发 UPDATE。
- 没有身份表（重复行直接 panic）、没有 savepoint、没有提交后任务。

### 3.3 目标职责

| 能力 | Procedure | UnitOfWork | TX |
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
- **重试语义**：与 xdb 一致，遇到可重试错误（锁冲突、序列化失败）时整段重新执行。因此 `fn` 必须可重入，所有外部副作用（发包、RPC、事件）必须放到 `AfterCommit`。
- **提交结果不确定**：COMMIT 发出后网络断开，不能当作成功，也不能重跑业务。保持现有 `context.WithoutCancel` 的处理方式；`AfterCommit` 只在 COMMIT 明确成功后运行。

### 4.2 UnitOfWork

- 负责：
  - 身份表：`(table, identity) -> Flusher`，同一行返回同一对象；
  - 脏对象收集；
  - savepoint 栈与内存回滚；
  - 提交/回滚后任务队列；
  - 同步模式下协调 `Flush`。
- 不负责：SQL 事务的 begin/commit/rollback（那是 Procedure + TX 的职责）。
- 生命周期：由 Procedure 创建，放进 ctx，随 Procedure 结束而销毁。

### 4.3 TX

- 保持现状：`dbctx.TX` 是带 SQL 日志的 pgx.Tx 包装。
- 不把 UoW 和 TX 合并：
  - UoW 未来可能跨多个 TX（write-behind 的批量落库）；
  - 只读路径可能只有 TX、没有 UoW（`dbpool.Query`）。

### 4.4 Flusher

- 保持现状：生成代码实现 `TableName` / `Identity` / `FlushStmt` / `FlushDone`。
- 未来异步化时再加 `CaptureDirty` / `ConfirmFlushed`，不改同步路径语义。

## 5. 生命周期

```
Procedure(ctx, opts, fn)
  ├─ begin TX
  ├─ UoW := NewUnitOfWork()
  │    ctx = WithTx(ctx, tx)
  │    ctx = WithUnitOfWork(ctx, UoW)
  ├─ prepare: SET LOCAL ...; acquire lock
  ├─ fn(ctx)
  │    ├─ GetByXxx → QueryOne → Track → UoW.Register
  │    ├─ SetXxx → dirty |= ...；必要时记录回滚快照
  │    └─ AfterCommit(fn) / AfterRollback(fn)
  ├─ 成功:
  │    ├─ UoW.Flush(ctx)          // 同步模式：Batch UPDATE
  │    ├─ TX.Commit(WithoutCancel)
  │    └─ 执行 AfterCommit 队列
  └─ 失败 / panic:
       ├─ UoW.Rollback()         // 恢复内存对象
       ├─ TX.Rollback()
       └─ 执行 AfterRollback 队列
```

顺序约束：

- `Flush` 必须在 `TX.Commit` 之前；`Flush` 失败 → 整体回滚。
- `AfterCommit` 只在 COMMIT 明确成功后执行。任务失败不能回滚已经提交的数据，只能记录日志；是否向调用方返回错误由业务决定。
- `AfterRollback` 在 TX 回滚之后执行，不能假设数据库里还有未回滚的数据。
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
- 不修改 `SetXxx` 签名，不在实体里塞 UoW 指针，不给 setter 加 `ctx`。
- 嵌套 savepoint 如果需要，按层保存快照；没有嵌套需求就只保留 Procedure 级快照。
- 字段级 undo log 明确列为**非目标**。

### 6.4 当前阶段是否真的需要内存回滚

| 场景 | 失败后内存对象会怎样 | 是否需要 memento |
|---|---|---|
| 当前模型：对象随请求创建、随请求丢弃 | 对象不可达，DB 事务已回滚 | 不需要 |
| 共享缓存 / write-behind | 缓存里的对象会留下半改状态 | 必需 |
| Procedure 重试 | 重跑可能拿到同一个缓存对象 | 必需 |

所以 Phase 2 的 memento 是为"缓存/重试"准备的；没有这两者时，DB 回滚 + 丢弃对象就够了。不要为了回滚提前引入字段级日志。

### 6.5 必须处理的边界：Insert 后回滚

生成代码的 `Insert` 是在 SQL 成功、主键回填后才 `Track` 的。如果按 Track 时快照，回滚后对象仍然带着 `id` 和 `loaded=true`，但库里没有这行。处理方式：

- `Insert` 在发 SQL 前把实体登记为"新建"（identity=0、loaded=false 的初始状态）；
- 回滚时对"新建"实体做 `ResetNew`：清 `id`、`loaded`、`dirty`，并从 UoW 身份表移除；
- 对象如果随后被丢弃，这一步只是保险；如果对象在缓存/重试中被复用，它是必需的。

### 6.6 业务约束

- 校验失败就返回错误或让 panic 传播，**不要在 Procedure 内部 `recover` 事务错误后继续用同一批对象**。xdb 的注释也明确写着"不能捕获 XError 及其子类"。
- 对外副作用（发包、RPC、事件）走 `AfterCommit`；失败通知走 `AfterRollback`。
- 不要用 `ResetToLoaded` 在业务里做局部撤销——那是绕过 UoW 的写法。

## 7. API 形状（提案）

### 7.1 Procedure 入口

```go
type ProcedureOptions struct {
    Lock    LockSpec      // 单 key / 双 key / try / block
    Retry   RetryPolicy   // 可重试错误、次数、退避
    Timeout time.Duration
}

func Procedure(ctx context.Context, opts ProcedureOptions, fn func(context.Context) error) error
```

- 底层复用 `WithTxPrepare`。
- `WithTx` / `WithTryAdvisoryLock*` / `WithAdvisoryLock*` 保留为薄封装，避免一次性改所有调用点。
- 如果保留嵌套语义，嵌套 `Procedure` 自动创建 savepoint；不需要嵌套时，Phase 2 只做 Procedure 级回滚，savepoint API 延后。

### 7.2 UnitOfWork 接口

```go
type UnitOfWork interface {
    // Register 登记实体并返回规范对象。
    // 命中身份表时返回已有对象；否则登记 obj 并返回 obj。
    Register(obj Flusher) Flusher

    // RegisterNew 在 Insert 之前登记新建实体，回滚时恢复为未加载状态。
    RegisterNew(obj Flusher)

    Savepoint() int
    RollbackTo(sp int)

    AfterCommit(fn func(context.Context))
    AfterRollback(fn func(context.Context))
}
```

`Track` 改成泛型并返回规范对象：

```go
func Track[T Flusher](ctx context.Context, obj T) T
```

`QueryOne` / `QueryList` 用返回值替换扫描出来的对象。这样"同一行一个对象"从 panic 守卫变成身份表保证。

### 7.3 生成代码变化

- `QueryOne` / `QueryList`：`obj = dbctx.Track(ctx, obj)`。
- 新增 `SnapshotState()` / `RestoreState()` / `ResetNew()`。
- `Insert` 在发 SQL 前 `TrackNew`（identity=0、loaded=false），成功后回填主键并更新身份表；回滚时 `ResetNew`。
- `FlushStmt` / `FlushDone` 暂不变。

## 8. 分阶段落地

| 阶段 | 内容 | 验收 |
|---|---|---|
| 0 | 本文档 + `design.md` 措辞统一；代码不动 | 文档评审通过 |
| 1 | UoW 身份表；`Track` 返回规范对象；重复行 panic 保留为兜底 | 现有测试 + "同事务两次查询返回同一指针"测试 |
| 2 | 整事务 memento 回滚 + `ResetNew`；嵌套 savepoint 可选 | 多表修改回滚测试；Insert 后回滚测试 |
| 3 | `Procedure` 入口（锁/超时/重试/AfterCommit/AfterRollback） | 锁冲突重试测试；提交后任务时序测试 |
| 4 | （可选）Persister / write-behind，与 Procedure 解耦 | 独立设计，不在本文范围 |

每个阶段：

- `go build ./...`、`go vet ./...`、`go test ./...`；
- 更新 [design.md](design.md) 并在"变更记录"里留痕；
- 涉及生成代码的，重新生成 `ptable/*.go` 并提交（注意 CRLF 导致的假修改）。

## 9. 对现有语义的影响

1. **重复行检测**：身份表命中后不会再出现两个对象，`Register` 的 panic 从"主要守卫"退化为"兜底"。这与 design.md 2.1 的取舍不同——当时的结论是"为假想场景维护身份表不划算"；引入事务级身份表后，它同时承担 read-your-writes、去重和回滚，成本开始有回报。需要在 design.md 里改写 2.1 并留变更记录。
2. **Flush 时机**：Phase 1-3 不变，仍在 COMMIT 前同步 Flush；Phase 4 才讨论异步。
3. **错误传播**：保持 panic 传播；Procedure 只捕获可重试错误，其他错误原样抛出。
4. **重试与副作用**：`fn` 可能被执行多次；所有对外副作用必须走 `AfterCommit`，否则重试会重复。
5. **锁顺序**：Procedure 仍然只挂一把锁；多锁场景由业务保证顺序，与 design.md 第 3 节一致。
6. **身份表的作用域**：UoW 是事务级的，不跨事务；跨请求共享对象属于 write-behind/会话工作集，不在本提案范围内。
7. **业务失败语义**：校验不通过返回错误，由 Procedure 整体回滚；业务不要在事务内 `recover` 后继续。参考 `RedPackRainRole.click()`：校验前置，失败直接返回 false。

## 10. 非目标与风险

- 不把 `dbctx.TX` 改名成 `Transaction`，避免歧义。
- 不把 UoW 和 Procedure 合并成一个类型：生命周期和职责都不同。
- `Procedure` 在 Go 里是函数 + options，不是类；不要为了名字造一个需要继承的结构。
- memento 方案在"大量实体 + 多层 savepoint"时是 O(n×m)，需要压测；如果开销不可接受，先限制嵌套层数，再考虑更细的记账机制。
- 失败策略：Phase 1-3 仍是"错误回滚、向上抛"；write-behind 的 fail-stop/降级策略在 Phase 4 单独设计。

## 11. 待决问题

1. `Procedure` 的锁参数是统一成 `LockSpec`，还是保留现有四个 `With*Lock*` 函数作为入口？
2. 重试范围：只重试 `ErrAdvisoryLockNotAcquired`，还是也包括 PG 的 serialization failure / deadlock？后者需要业务回调严格幂等。
3. 嵌套 savepoint 是否纳入 Phase 2？参考业务实现没有用到部分回滚；如果不需要，Phase 2 只保留 Procedure 级 memento，`Savepoint` / `RollbackTo` 可以延后。
4. `QueryList` 遇到重复行时，是返回同一指针两次，还是去重？xdb 返回同一个 `TRecord`；建议返回同一指针并在文档里说明。
5. UoW 是否向业务暴露 `Savepoint` / `RollbackTo`，还是只允许 Procedure 内部使用？
6. `AfterCommit` 任务 panic 时：记录日志并继续执行后续任务，还是中断？建议记录日志、继续执行所有任务，最后把 panic 汇总给调用方。
