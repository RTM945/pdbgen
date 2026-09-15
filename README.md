# pdbgen

从 `pdb.xml` 生成Go持久化代码（Select + 乐观锁Update，不含Delete），
以及一个只读的schema差异检查工具。

## 目录结构

```
schema/          解析pdb.xml，定义bean/table的数据结构
gen/             代码生成器：bean+table -> Go struct + Load/Update/Insert
gen/cmd/         生成器命令行入口
schemacheck/     只读对比数据库实际结构和xml定义的差异，只给建议SQL，绝不自动执行
schemacheck/cmd/ schemacheck命令行入口
example/         对pdb.xml跑一遍生成器产出的示例代码
verify/          用生成代码在真实PG上跑一遍Insert/Load/Update/乐观锁冲突场景，验证正确性
```

## 设计要点（对应之前讨论的架构决策）

**bean只描述Go类型，table只描述PG相关的东西**
`<bean>` 里的 `type` 全部是Go类型（`int64`/`string`/`time.Time`），不出现任何PG的东西；
主键、索引这些PG概念全部在 `<table>` 里声明。两者职责分离，bean可以复用在不需要DB的场景。

**并发控制用字段级乐观锁，不用 `SELECT ... FOR UPDATE`**
`Load` 不加锁，读的时候顺手记一份每个字段的初始值快照；`Update` 只针对真正被
`SetXxx` 改过的字段生成 `SET`，并用快照值做 `WHERE` 条件（CAS），一次操作合并成一条SQL。
这样"读快照 → 执行业务逻辑（哪怕中间夹了调AI接口这种慢操作）→ 写回"这几步之间，
除了最后写的一瞬间，全程不占用任何DB连接/锁——呼应之前反复强调的"事务里不能有网络IO"这条原则。

写冲突时返回 `ErrXxxConflict`，调用方应该重新 `Load` 最新数据、重跑业务逻辑后再 `Update` 一次，
这个重试循环建议封装在Repository/框架层，业务代码不需要感知。

**只有 Select 和 Update（按需Insert），没有 Delete**
符合"玩家数据不删除"这个业务前提。

**联合索引**
`<index variable="...">` 支持逗号分隔多个字段，如 `variable="id,token"`，顺序即索引的列顺序
（PG联合索引遵循"最左前缀"规则，顺序错了等于是另一个索引）。生成器会产出对应的
`LoadXxxByIdToken(ctx, db, id, token)` 这种多参数查询函数；`schemacheck` 也会连带校验
数据库里实际的索引列组成和顺序是否与xml一致，名字对得上不代表列真的一致。

**表结构变更不自动执行**
`schemacheck` 只读对比数据库实际结构和xml声明的差异，输出建议的 `ALTER`/`CREATE INDEX`
语句和详细原因，但不会连接后自动执行任何DDL——表结构变更应该由人看着建议、
自己判断影响面和时机后手动执行，不应该是生成器自动做的事。

## pdb.xml 根节点的连接配置

```xml
<pdb url="postgres://user:pass@host:5432/dbname?sslmode=disable"
     genOutput="./example"
     schema="public"
     poolMaxConns="100" poolMinConns="10"
     poolMaxConnLifetime="3600" poolMaxConnIdleTime="1800"
     poolHealthCheckPeriod="60"
     statementTimeoutMs="5000" idleInTransactionSessionTimeoutMs="5000"
     appName="lobby-svc">
```

**技术栈决定了哪些池参数是"真的"**

生成代码里的 `DBTX` 接口方法签名对应 `*sql.Row`/`sql.Result`，这是 `database/sql`
标准接口的类型，不是 pgx 原生连接池 `pgxpool.Pool` 的类型（`pgxpool` 返回的是
`*pgx.Row`/`pgconn.CommandTag`）。这意味着 `poolMinConns` 和 `poolHealthCheckPeriod`
这两个属性，在当前这套技术栈下**没有真正对应的实现**：

- `database/sql` 只有4个可调参数：`SetMaxOpenConns` / `SetMaxIdleConns` /
  `SetConnMaxLifetime` / `SetConnMaxIdleTime`，没有"强制保持N个连接常驻热身"的
  `MinConns`，也没有后台定期探活的 `HealthCheckPeriod`——这是 `pgxpool` 独有的能力。
- `dbpool.Open()` 用 `SetMaxIdleConns(poolMinConns)` 去近似 `poolMinConns`，
  但这只是"空闲时最多留几个不关"的上限，不是"启动时就保证有N个连接热着"的下限，
  流量低谷把连接关光之后不会自动补回来，等下一次请求来了才按需重建。
- `poolHealthCheckPeriod` 目前**没有被使用**——`database/sql` 做的是惰性检测
  （查询失败才发现连接坏了、换一个新连接重试），不是主动定期探活。在"乐观锁+短事务"
  这套架构下，连接本来就在被高频复用，惰性检测的实际效果和定期探活差不了太多，
  可以先不纠结这个参数。

如果你确实需要 `MinConns`/`HealthCheckPeriod` 这种更精确的语义，说明你需要的是原生
`pgxpool`，但那意味着要把 `DBTX` 接口和生成代码里的返回类型换成 pgx 对应的类型，
是牵动生成器模板的改动，不建议只为了这两个参数去做这次切换。

**新增了三个原始XML里没有、但建议加上的属性**

- `statementTimeoutMs` / `idleInTransactionSessionTimeoutMs`：分别对应PG的
  `statement_timeout` 和 `idle_in_transaction_session_timeout`，通过连接串的
  `options` 参数下发，等价于连接后执行 `SET`。前者防止一条异常慢/写错的查询
  长期占着连接；后者专门防"开了事务、中间抛了异常或者代码bug导致忘记提交/
  回滚"这种情况——事务挂着不结束的话，它占的行锁和连接会一直不释放，是个真实
  存在的风险点（呼应之前讨论过的"多表联动操作，事务包裹多条UPDATE"那部分，
  一旦这种事务因为bug没走到COMMIT/ROLLBACK，这两个超时就是最后的安全网）。
- `appName`：对应PG的 `application_name`，会出现在 `pg_stat_activity` 和日志里。
  当前架构下有多个独立服务共享同一个数据库（lobby服务、AI对话服务），排查问题
  时能一眼看出某个慢查询/某个占着连接不放的会话是哪个服务开的，不用瞎猜。
- `schema`：`schemacheck` 原来硬编码查 `public` schema，现在从这里读，不填默认
  `public`，行为不变，只是不再写死。

以上三个属性在 `dbpool.Open()` 里都已经接好并在真实PG上验证过：`statement_timeout`
能在设定时间精确掐断长查询，`idle_in_transaction_session_timeout` 能杀掉忘记提交
的空事务，`application_name` 能在 `pg_stat_activity` 里看到。



### 1. 生成Go代码

```bash
go run ./gen/cmd -xml pdb.xml -out ./example -pkg model
```

### 2. 检查数据库结构和xml是否一致

```bash
go run ./schemacheck/cmd -xml pdb.xml \
  -dsn "postgres://user:pass@host/db?sslmode=disable"
```

输出示例：

```
发现 1 处差异（以下建议SQL仅供参考，不会被自动执行，请人工确认后手动执行）：

[缺少索引] table=users: 索引 "index_token"（列 token）在数据库里不存在
    建议SQL(需人工确认后手动执行): CREATE INDEX index_token ON users (token);
```

### 3. 业务代码怎么用生成出来的东西

```go
// 读
u, err := model.LoadUserByToken(ctx, db, requestToken)

// 改
u.SetToken(newToken)
u.SetLastLoginAt(time.Now())

// 写（合并成一条SQL，CAS比对，冲突返回 ErrUserConflict）
err = u.Update(ctx, db)
if errors.Is(err, model.ErrUserConflict) {
// 期间数据被别的请求改过，重新Load后重试
}
```

业务代码全程不写SQL，也不需要知道背后是乐观锁还是别的什么机制。

## 当前版本的已知局限（下一步可以扩展的方向）

- 并发策略目前是"全字段统一走乐观锁"，还没有像之前讨论的那样按字段区分
  `atomic_incr`（原子加减，如货币）/ `optimistic`（乐观锁比对）/ `readonly`（只读配置），
  这个可以在 `<variable>` 上加 `strategy` 属性，generator按策略生成不同的Update片段
- `schemacheck` 目前按表名严格匹配 `public` schema，多schema场景需要扩展
- 复合主键、外键约束这些暂未支持，当前场景（单列自增主键）够用，后续按需加