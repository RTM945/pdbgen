# pdbgen 设计说明

这份文档记录 pdbgen 的**运行时语义和设计决策**——那些光看代码看不出来、但写业务代码时必须遵守的东西。和 `CLAUDE.md` 的分工：`CLAUDE.md` 讲怎么改这个仓库（命令、生成链路、注意点），这里讲框架对使用者的承诺。

改动运行时的语义时要同时改这里，并在下面的"变更记录"里留痕。

---

## 1. 定位

pdbgen 是面向游戏服务端的 PostgreSQL 数据访问框架：`pdb.xml` 定义表结构，生成器产出建表/迁移 SQL 和一套 Go 代码。它不做通用 ORM 的权衡——约束条件很明确：

- 一次事务服务一个请求，事务入口挂咨询锁，同一把锁的请求天然串行；
- 表都是窄表，一次请求涉及的改动少；
- 宁可让约束显式（panic、报错），也不做隐式兜底。

在这三个前提下，框架选择了"脏标记 + 工作单元 + 重复行检测"的组合，而不是 SQL 构建器或 ActiveRecord。

## 2. 核心不变量

### 2.1 一次请求里，同一行只查一次、只有一个对象

这是**使用约定**，不是框架替你保证的事。框架的作用是：违反了会立刻炸，不会静默写坏数据。

违反的后果本来是这样的：

```go
// 同一个事务内，两条路径查到同一行
a := UserRedEnvelopeTable.GetByUidActId(ctx, uid, actId)  // 对象 A
b := UserRedEnvelopeTable.GetById(ctx, a.Id())             // 对象 B，同一行
a.SetTodayCount(11)
b.SetTotal(42)
```

A 和 B 是两个互不相干的 Go 对象，各带各的脏位和 orig 快照。提交时 Transaction 会发出两条 UPDATE：`SET today_count = 11` 和 `SET total = 42`。写的是绝对值，同一列被两处改过时后执行的那条赢，先改的**静默消失**——没有日志、没有报错。这是框架里唯一会静默丢数据的失败模式。

**Why 用检测而不是自动合并**：自动合并需要框架维护"一行一对象"的身份表（把第二次查出来的对象丢弃、返回第一次的指针）。但业务代码本来就会把第一次查出的对象传参使用，不会回头再查——为不存在的场景维护一张事务级身份表和一层查询包装不划算。所以框架只做守卫：**检测到同一行有两个对象就直接 panic**，请求失败、事务回滚。

**How to apply**：正常写就不会碰到。真碰到了，报错信息会给出表名、主键和改法：复用第一次查出来的对象。

### 2.2 重复行检测的触发点和严格程度

`Transaction.Register` 在**每个对象被登记时**检查 `(表名, 主键)` 是否已经出现过：

- 触发时机是**注册时**（也就是第二次查询返回时），不是提交时。不用等到 flush 去比较脏位，早炸早发现。
- 判断只看"是不是同一行"，**不看对象脏不脏**。即使只有一个对象被改过、两条 UPDATE 实际上不冲突，也会 panic。
- 同一个对象（同一个指针）重复登记不算重复行——`QueryOne` 和 `QueryList` 都会登记，业务代码也可能重复注册同一个指针。
- 主键为 0 的对象（自增主键还没回填、还没 `Insert`）没有身份，不参与检测。
- 检测只在事务内有意义：非事务查询（`SelectXxx` / `Query`）没有 Transaction，不会登记也不会检测。

严格程度的取舍：宽松版（只在两边都脏且脏位重叠时才炸）需要框架判断"两条 UPDATE 会不会真的冲突"，复杂且仍然拦不住全部情况；现在这样用一条 `(表名, 主键)` 就覆盖了全部场景，代价是极少数无害写法被拦——拦下的报错会告诉你改法。

### 2.3 落库时机在事务提交前，不在 `SetXxx` 时

`SetXxx` 只改内存 + 置脏位；真正的 UPDATE 由 `WithTxPrepare` 在业务回调返回后统一 flush（`Transaction.Flush`）。所以：

- 事务中途查库看到的是**库里的值**，不是本事务刚改的值；
- 一次事务里所有脏对象合并成一个 `pgx.Batch` 发出，一次往返；
- 任何一条失败 → 整体回滚。

### 2.4 错误靠 panic 传播，事务边界统一收口

生成代码遇到 pgx 错误直接 panic；`QueryOne` 查不到返回 nil（`ErrNoRows` 不算错误），其他错误 panic。`WithTxPrepare` 的 defer 按"panic 回滚 / 有错回滚 / flush / commit"的顺序收尾，panic 会**回滚后原样重抛**（要接住它必须包住整个 `WithTx` 调用，放在回调里的 `recover` 接不到）。业务回调里不需要逐个检查 SQL 错误。

### 2.5 `GetXxx` 与 `SelectXxx` 互斥

| | 必须在事务里 | 用哪条连接 | 参与重复行检测 |
|---|---|---|---|
| `GetByXxx` / `GetById` | 是（否则 panic） | 事务 | 是 |
| `SelectByXxx` / `SelectById` / `SelectAll` | 否（是则 panic） | 连接池 | 否 |
| `dbpool.Query` | 否（是则报错） | 连接池 | 否 |

事务里从池里另取一条连接读数据，会读到事务未提交前的旧值——所以框架直接把这条路封死。

## 3. 事务、锁与超时

- 事务总预算 `transactionTimeoutMs`，`SET LOCAL` 下发 `statement_timeout` / `idle_in_transaction_session_timeout` / `lock_timeout`。
- `COMMIT` 用 `context.WithoutCancel(ctx)`：一旦发出就必须等到结果，避免"客户端不知道提交成功没有"导致重试重复发放。
- 咨询锁是**替代 `SELECT ... FOR UPDATE` 的行级串行手段**，在 `prepare` 钩子里、事务内获取：
  - `WithTryAdvisoryLock(key)` / `WithTryAdvisoryLock2(ns, id)`：拿不到返回 `ErrAdvisoryLockNotAcquired`；
  - `WithAdvisoryLock` / `WithAdvisoryLock2`：阻塞等待。
  - 玩家请求用 try 版，GM/RPC 写请求用阻塞版（README）。

**已知限制**：框架一次只挂一把锁，多把锁的获取顺序由业务自己保证；行的 UPDATE 顺序 = 对象注册顺序（首次查询顺序），一次事务锁多把锁时这个顺序会参与死锁判定。当前"一次事务一个 uid 一把锁"的模型下不存在这个问题。

### 3.1 提交后 / 回滚后任务

`dbctx.AfterCommit(ctx, fn)` / `dbctx.AfterRollback(ctx, fn)` 把任务挂到当前事务：

- **AfterCommit** 只在 COMMIT 明确成功后执行；COMMIT 返回错误时结果不确定，两个钩子都不执行——可能已提交，不能发失败通知；也可能没提交，不能发成功通知。
- **AfterRollback** 在业务返回错误、panic、或 Flush 失败导致回滚后执行。
- **顺序**：都按注册顺序执行；同步执行在调用方 goroutine 上，慢钩子会拖慢请求。
- **没有事务时**：直接执行（没有提交点可等）。
- **上下文**：钩子拿到的是调用方 ctx 去掉取消/超时、且不带 SQLTx/Transaction 的副本；保留请求 ID 等值。钩子要查库用 `dbpool.Query`，不要用 `GetByXxx`（事务方法）拿事务 ctx。
- **panic**：单个钩子 panic 只记日志（slog），后续钩子继续执行；已经提交的数据不受影响。
- **用途**：发包、RPC、事件等外部副作用放 AfterCommit；失败通知放 AfterRollback。不要在钩子里做能失败并需要回滚的业务。

## 4. 生成代码的形状

| 生成物 | 说明 |
|---|---|
| `NewXxx()` | 空对象，没有身份 |
| `GetById` / `SelectById` | 主键查询 |
| `GetByYyy` / `SelectByYyy` | **unique 索引**查询（`index unique="true"`） |
| `ListByYyy` / `SelectListByYyy` | **非 unique 索引**查询，返回 `[]*Xxx` |
| `GetAll` / `SelectAll` | 全表，返回 `[]*Xxx` |
| `SetXxx` / `Xxx()` | 写入（值没变不置脏位）/ 读取 |
| `TableName()` | 表名常量，实现 `dbctx.Flusher`，供重复行检测用 |
| `Identity()` | 主键值，0 表示还没回填（还没 `Insert`），不参与检测 |
| `FlushStmt()` / `FlushDone()` | 实现 `dbctx.Flusher`，只拼脏列 |
| `ResetToLoaded()` | 回到 last-loaded 快照（单对象回滚，不是事务回滚） |
| `Insert()` | `INSERT ... RETURNING <pk>`，回填主键、登记 Transaction |
| `Update()` | 绕过 Transaction 的立即落库入口，生成代码内部不用 |

改 `unique` 会改变生成的 API 形态（单行 ↔ 列表）。列表查询返回的每个元素都是新对象，所以要格外注意 2.1：**列表里已经有的行，不要再单独 `GetById` 查一次**——那正是检测会拦下的情况。

## 5. 已知缺口

1. **`ResetToLoaded` 只回滚单个对象**：一次请求改多张表时没有 ctx 级的回滚日志，要回滚得自己按逆序处理（README todo）。
2. **咨询锁 key 的哈希问题**：`hashtext('user', uid)` 用两个 int 拼，uid 超过 int32 会出错；预想改成默认直接用 uid、其他业务锁用生成代码的枚举值（README todo）。
3. **多咨询锁场景下的加锁/更新顺序**：见第 3 节，当前模型下不适用。
4. **`ListByYyy` 的元素与后续单行查询的重复检测**：列表里的对象登记后，再对同一行单独 `GetById` 会 panic——这是设计使然，但列表 + 逐条处理的写法（比如分页邮件列表里逐封取）需要改成复用列表里的对象。

## 6. 设计提案

- **Procedure 与 Transaction 的职责划分与命名**：[design-procedure-transaction.md](design-procedure-transaction.md)（提案，未实施）。实施时并入本文正文，并在变更记录里留痕。

## 7. 变更记录

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
- **验证状态**：`dbpool/transaction_test.go` 用最小 Flusher 替身覆盖检测逻辑（重复行 panic、同指针不误报、不同行/不同表不误报、主键为 0 跳过）；`dbpool/duplicate_test.go` 对真实 PG 覆盖端到端行为（混合查询路径触发 panic 且事务回滚、只有一方脏也照样拦、跨事务不误报）。`go vet ./...` 干净。
- **未验证**：`ListByYyy` 与后续单行查询组合时的检测行为（见已知缺口 4，只有设计推理没有测试）。

### 2026-10-16 放弃事务级身份表（被上面的方案取代）

- **背景**：先做的是"自动修复"版本——`dbctx.InstanceCache` 作为事务级身份表，单行查询包在 `dbctx.Resolve` 里，命中 `(表名, 主键)` 就丢弃新扫出来的对象、返回已有指针，并给 `Flusher` 加了 `Identity()`。
- **为什么撤掉**：它建立在"同一请求会查两次同一行"这个假设上，而按使用方的约定业务代码靠传参传递对象、不会回头再查。为不成立的场景维护身份表、查询包装层和反射判空不划算；而且它只是**修**，违反约定时悄无声息。移除后 `Flusher` 只保留 `TableName()` / `Identity()` 两个给检测用的方法，查询路径 `QueryOne` / `QueryList` 回到原样。
- **保留的部分**：`Identity()`（检测需要判断"是不是同一行"）和它的"主键为 0 表示还没回填"语义。
