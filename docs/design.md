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
- 非事务查询（`SelectXxx` / `Query`）没有 Transaction，直接返回扫描出来的对象。

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
- `QueryList` 里如果同一个 `(表名, 主键)` 出现多次，返回的切片里会是同一个指针（列表元素可能重复）。

这条保证替代了早期的"同一行两个对象就 panic"守卫：现在不是报错，而是把第二次扫描出来的对象解析成第一次的记录。`Update()` 绕过事务缓存，仍然直接使用传入的对象。

### 2.3 落库时机在事务提交前，不在 `SetXxx` 时

`SetXxx` 只改内存 + 置脏位；真正的 UPDATE 由 `WithTxPrepare` 在业务回调返回后统一 flush（`Transaction.Flush`）。所以：

- 事务中途查库看到的是**库里的值**，不是本事务刚改的值；
- 一次事务里所有脏对象合并成一个 `pgx.Batch` 发出，一次往返；
- 任何一条失败 → 整体回滚。

### 2.4 错误靠 panic 传播，事务边界统一收口

生成代码遇到 pgx 错误直接 panic；`QueryOne` 查不到返回 nil（`ErrNoRows` 不算错误），其他错误 panic。`WithTxPrepare` 的 defer 按"panic 恢复内存+回滚 / 有错恢复内存+回滚 / flush 失败恢复内存+回滚 / commit"的顺序收尾，panic 会**回滚后原样重抛**（要接住它必须包住整个 `WithTx` 调用，放在回调里的 `recover` 接不到）。业务回调里不需要逐个检查 SQL 错误。

### 2.5 `GetXxx` 与 `SelectXxx` 互斥

| | 必须在事务里 | 用哪条连接 | 参与身份表解析 |
|---|---|---|---|
| `GetByXxx` / `GetById` | 是（否则 panic） | 事务 | 是 |
| `SelectByXxx` / `SelectById` / `SelectAll` | 否（是则 panic） | 连接池 | 否 |
| `dbpool.Query` | 否（是则报错） | 连接池 | 否 |

事务里从池里另取一条连接读数据，会读到事务未提交前的旧值——所以框架直接把这条路封死。

### 2.6 失败时恢复内存对象

- 对象第一次进入事务身份表时，`Transaction` 调用生成的 `SnapshotState()` 保存整条快照（字段值 + orig + dirty + id + loaded）。
- 业务返回错误、panic、或 Flush 失败时，`WithTxPrepare` 先调用 `Transaction.Rollback()` 恢复所有快照，再回滚 SQLTx，最后执行 AfterRollback 钩子。
- `Insert` 在发 SQL 前调用 `dbctx.RegisterNew`；回滚时这些对象走 `ResetNew`：清主键（自增时）、`loaded=false`、`dirty=0`，并从身份表移除。
- COMMIT 返回错误时结果不确定：**不回滚内存对象**，也不执行提交/回滚钩子；调用方应丢弃这批对象。

## 3. 事务、锁与超时

- 事务总预算 `transactionTimeoutMs`，`SET LOCAL` 下发 `statement_timeout` / `idle_in_transaction_session_timeout` / `lock_timeout`。
- `COMMIT` 用 `context.WithoutCancel(ctx)`：一旦发出就必须等到结果，避免"客户端不知道提交成功没有"导致重试重复发放。
- 咨询锁是**替代 `SELECT ... FOR UPDATE` 的行级串行手段**，在 `prepare` 钩子里、事务内获取：
  - `WithTryAdvisoryLock(key)` / `WithTryAdvisoryLock2(ns, id)`：拿不到返回 `ErrAdvisoryLockNotAcquired`；
  - `WithAdvisoryLock` / `WithAdvisoryLock2`：阻塞等待。
  - 玩家请求用 try 版，GM/RPC 写请求用阻塞版（README）。

**已知限制**：框架一次只挂一把锁，多把锁的获取顺序由业务自己保证；行的 UPDATE 顺序 = 对象注册顺序（首次查询顺序），一次事务锁多把锁时这个顺序会参与死锁判定。当前"一次事务一个 uid 一把锁"的模型下不存在这个问题。

### 3.1 Procedure 入口

`dbpool.Procedure(ctx, opts, fn)` 是所有事务入口的统一形式：

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
- Phase 4 不做重试：`fn` 只执行一次。重试需要内存回滚（memento），等 Phase 5 再加。
- `WithTx` / `WithTryAdvisoryLock*` / `WithAdvisoryLock*` 保留为薄封装，底层都走 `Procedure`；`WithTxPrepare` 是 `Procedure` 的底层实现，需要自定义 `prepare` 钩子时直接用它。

### 3.2 提交后 / 回滚后任务

`dbctx.AfterCommit(ctx, fn)` / `dbctx.AfterRollback(ctx, fn)` 把任务挂到当前事务：

- **AfterCommit** 只在 COMMIT 明确成功后执行；COMMIT 返回错误时结果不确定，两个钩子都不执行——可能已提交，不能发失败通知；也可能没提交，不能发成功通知。
- **AfterRollback** 在业务返回错误、panic、或 Flush 失败导致回滚后执行；执行时内存对象已经由 `Transaction.Rollback` 恢复。
- **顺序**：都按注册顺序执行；同步执行在调用方 goroutine 上，慢钩子会拖慢请求。
- **没有事务时**：直接执行（没有提交点可等）。
- **上下文**：钩子拿到的是调用方 ctx 去掉取消/超时、且不带 SQLTx/Transaction 的副本；保留请求 ID 等值。钩子要查库用 `dbpool.Query`，不要用 `GetByXxx`（事务方法）拿事务 ctx。
- **panic**：单个钩子 panic 只记日志（slog），后续钩子继续执行；已经提交的数据不受影响。
- **用途**：发包、RPC、事件等外部副作用放 AfterCommit；失败通知放 AfterRollback。不要在钩子里做能失败并需要回滚的业务。

## 4. 生成代码的形状

| 生成物 | 说明 |
|---|---|
| `NewXxx()` | 空对象，没有身份 |
| `GetById` / `SelectById` | 主键查询；`GetById` 在事务里先查身份表，命中不发 SQL |
| `GetByYyy` / `SelectByYyy` | **unique 索引**查询（`index unique="true"`） |
| `ListByYyy` / `SelectListByYyy` | **非 unique 索引**查询，返回 `[]*Xxx` |
| `GetAll` / `SelectAll` | 全表，返回 `[]*Xxx` |
| `SetXxx` / `Xxx()` | 写入（值没变不置脏位）/ 读取 |
| `TableName()` | 表名常量，实现 `dbctx.Flusher`，供身份表解析用 |
| `Identity()` | 主键值，0 表示还没回填（还没 `Insert`），不参与身份表解析 |
| `FlushStmt()` / `FlushDone()` | 实现 `dbctx.Flusher`，只拼脏列 |
| `SnapshotState()` / `RestoreState()` | 整条实体快照；事务失败时由 `Transaction.Rollback` 恢复 |
| `ResetNew()` | `Insert` 回滚后恢复成未加载状态（自增主键清零） |
| `ResetToLoaded()` | 手动回到 last-loaded 快照；事务级回滚用 `Transaction.Rollback` |
| `Insert()` | `INSERT ... RETURNING <pk>`；先 `RegisterNew`，回填主键后 `GetOrAddCachedRecord` |
| `Update()` | 绕过 Transaction 的立即落库入口，生成代码内部不用 |

改 `unique` 会改变生成的 API 形态（单行 ↔ 列表）。列表查询返回的元素在事务内都会登记进身份表；再对同一行单独 `GetById` 会返回列表里的同一个指针，不会产生第二个对象。

## 5. 已知缺口

1. **嵌套 savepoint / 部分回滚还没实现**：现在只有事务级 memento 回滚（`Transaction.Rollback`）；`ResetToLoaded` 保留给业务手动局部撤销。
2. **咨询锁 key 的哈希问题**：`hashtext('user', uid)` 用两个 int 拼，uid 超过 int32 会出错；预想改成默认直接用 uid、其他业务锁用生成代码的枚举值（README todo）。
3. **多咨询锁场景下的加锁/更新顺序**：见第 3 节，当前模型下不适用。
4. **`QueryList` 的重复指针**：同一事务里如果列表查询返回了同一行多次，切片里的元素会是同一个指针；业务如果假定列表元素互不相同，需要自己去重。

## 6. 设计提案

- **Procedure 与 Transaction 的职责划分与命名**：[design-procedure-transaction.md](design-procedure-transaction.md)（提案，Phase 1-5 已实施）。实施时并入本文正文，并在变更记录里留痕。

## 7. 变更记录

### 2026-10-20 增加事务级内存回滚（memento）

- **改了什么**：`dbctx.Flusher` 增加 `SnapshotState` / `RestoreState` / `ResetNew`；`dbctx.Transaction` 增加 `RegisterNew` / `Rollback`；`WithTxPrepare` 在错误、panic、Flush 失败时先恢复内存对象；生成的 `Insert` 先 `RegisterNew`。
- **为什么**：重试和共享缓存需要失败后内存状态与数据库一致；之前只有单对象 `ResetToLoaded`。
- **影响范围**：错误 / panic / Flush 失败路径恢复对象；COMMIT 结果不确定时不回滚内存、不执行钩子；成功路径不变。嵌套 savepoint / 部分回滚仍未实现。
- **验证状态**：`go build/vet/test` 通过；单元测试覆盖快照恢复、多对象、新建对象 `ResetNew`；真实 PG 集成测试覆盖加载后修改回滚、panic 回滚、Insert 回滚、AfterRollback 钩子看到恢复后的值、多对象回滚。
- **未验证**：嵌套 savepoint / 部分回滚；重试尚未开启。

### 2026-10-20 增加 Procedure 统一入口

- **改了什么**：新增 `dbpool.Procedure(ctx, ProcedureOptions, fn)`、`ProcedureOptions`、`LockSpec` 和四个锁构造器；`WithTx` / 四个 `With*Lock*` 改为 `Procedure` 的薄封装；`ProcedureOptions.Timeout` 可以给单次 Procedure 加预算。
- **为什么**：把事务入口、锁、超时收敛到一个 xdb 风格的 `Procedure`；以后加重试时只需要改这一处。
- **影响范围**：现有入口行为不变；Phase 4 不重试，`fn` 仍然只执行一次；`Timeout` 与全局预算取更早；COMMIT 不受预算影响。
- **验证状态**：`go build/vet/test` 通过；真实 PG 集成测试覆盖 try 锁冲突、Timeout 体现在 ctx deadline、旧 `WithTryAdvisoryLock` 薄封装。
- **未验证**：阻塞锁的等待/超时行为依赖 `lock_timeout`，没有单独测试；重试未实现。

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
