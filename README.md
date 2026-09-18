# pdbgen

从 `pdb.xml` 生成Go持久化代码（Select + 悲观锁Update，不含Delete），
以及一个只读的schema差异检查工具。

## 目录结构

```
schema/          解析pdb.xml，定义bean/table的数据结构；ColumnName等命名规则也在这里统一维护
gen/             代码生成器：bean+table -> Go struct + Load/Update/Insert
gen/cmd/         生成器命令行入口
schemacheck/     只读对比数据库实际结构和xml定义的差异，只给建议SQL，绝不自动执行
schemacheck/cmd/ schemacheck命令行入口
ddl/             生成建表DDL；仅当表在数据库里不存在时才允许自动执行
ddl/cmd/         ddl命令行入口
dbpool/          从pdb.xml的连接配置构建*pgxpool.Pool
example/         对pdb.xml跑一遍生成器产出的示例代码
verify/          用生成代码在真实PG上验证 FOR UPDATE 悲观锁的阻塞行为、ResetToLoaded、联合索引查询
verify_pool/     验证连接池配置（MinConns预热/statement_timeout等）真实生效
```

## 设计要点（对应之前讨论的架构决策）

**bean只描述Go类型，table只描述PG相关的东西**
`<bean>` 里的 `type` 全部是Go类型（`int64`/`string`/`time.Time`），不出现任何PG的东西；
主键、索引这些PG概念全部在 `<table>` 里声明。两者职责分离，bean可以复用在不需要DB的场景。

**并发控制用 `SELECT ... FOR UPDATE` 悲观锁**

`Load`（包括按索引Load）都会对读到的这一行加 `FOR UPDATE`，把整行锁住直到事务
结束；`Update` 只针对真正被 `SetXxx` 改过的字段生成 `SET`，不再需要旧值比对
——因为从Load到Update/Commit之间这一行一直被锁着，不存在别的请求能在中间插进来
改动同一行的可能。

**这个锁只在"显式事务"内才会一直持有，这是最容易踩的坑**：如果直接把
`*pgxpool.Pool` 传给 `Load`（而不是先 `pool.Begin(ctx)` 拿到 `pgx.Tx` 再传进去），
`FOR UPDATE` 在这条SELECT语句执行完就立刻释放了，后面的Update完全没有被保护，
等于白锁。正确用法：

```go
tx, _ := pool.Begin(ctx)
defer tx.Rollback(ctx) // Commit成功后这里是空操作，可以放心defer
obj, _ := model.LoadUser(ctx, tx, id)
obj.SetToken(newToken)
_ = obj.Update(ctx, tx)
tx.Commit(ctx)
```

这两种行为都在真实PG上验证过：正确用法下，并发的第二个事务Load同一行会被
阻塞、等第一个事务Commit后才能继续（阻塞时长和第一个事务持有锁的时间吻合，
不是碰巧）；而不开事务直接用pool的错误用法下，第二次Load几乎不用等待就能
拿到数据，具体证明了"锁没有跨语句生效"这个警告不是空谈。

**这套模式的代价（回到了很早讨论过的"日本手游那套架构"的取舍）**：`Load`到
`Update`/`Commit`之间不能再夹带慢操作（比如调用AI接口），因为这段时间行锁一直
占着——之前乐观锁方案刻意规避的这个问题，现在又变成需要业务开发自己注意的
边界：涉及外部慢调用的场景（AI对话之类），不要把它们塞进这个Load/Update包裹的
事务区间里，应该像很早之前讨论过的那样拆成"外部调用（不持锁）+短事务落库
（持锁）"两段。

**`orig` 快照字段保留了下来，但用途变了：不再用于CAS比对，是给以后引入本地
缓存做"失败回滚"用的**

`ResetToLoaded()` 方法可以把对象的字段还原回Load成功时（或者上一次Update成功时）
的快照值，并清空dirty标记——用途是：如果Update失败、或者事务因为别的原因回滚了，
内存里的对象可能已经被`SetXxx`改得和数据库实际内容不一样了。以后如果引入本地
缓存、把这个对象继续放回缓存供下次读取复用，绝不能让缓存里存着"改了一半、DB
其实没写进去"的脏状态——出错时调用`ResetToLoaded()`，就能把对象安全恢复成
"确定和DB一致"的版本。当前架构里还没有本地缓存，这个方法先备着，已经在
真实PG上验证过它能正确复原字段值并清空dirty（验证方式：改一个字段、调用
`ResetToLoaded()`、确认字段变回原值，并且之后调用`Update`是no-op、不会真的
执行任何SQL）。

**只有 Select 和 Update（按需Insert），没有 Delete**
符合"玩家数据不删除"这个业务前提。

**联合索引**
`<index variable="...">` 支持逗号分隔多个字段，如 `variable="id,token"`，顺序即索引的列顺序
（PG联合索引遵循"最左前缀"规则，顺序错了等于是另一个索引）。生成器会产出对应的
`LoadXxxByIdToken(ctx, db, id, token)` 这种多参数查询函数（同样是 `FOR UPDATE`）；
`schemacheck` 也会连带校验数据库里实际的索引列组成和顺序是否与xml一致，名字对得上
不代表列真的一致。

**建表：只在表不存在时自动执行，已存在的表一律不碰**
`ddl` 包按XML生成 `CREATE TABLE`+主键+索引，和 `schemacheck` 的"只读建议"是互补
关系，不是矛盾——区别在于风险：一张表完全不存在，建错了大不了删掉重来，不涉及
数据丢失，所以这一步允许自动执行；但凡表已经存在（哪怕结构和xml对不上），
`ddl.Bootstrap` 一律跳过、绝不做任何 `ALTER`，改动还是走 `schemacheck` 的建议
+人工手动执行这条路。三个场景都在真实PG上验证过：表不存在时`-apply`能正确建表
（并且用`schemacheck`交叉验证过和xml定义完全一致）；表已存在且有数据时，
再次`-apply`会完全跳过，数据分毫未动。

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

**技术栈：原生 pgxpool（不是 database/sql）**

生成代码里的 `DBTX` 接口方法是pgx风格（`Exec`/`QueryRow`，没有Context后缀——pgx
要求ctx必须显式作为第一个参数），`Update` 里判断冲突用的是 `pgconn.CommandTag.
RowsAffected()`（注意这个方法**不返回error**，和 `database/sql` 的 `sql.Result.
RowsAffected() (int64, error)` 签名不一样）。`*pgxpool.Pool` 和 `pgx.Tx` 都实现了
这个 `DBTX` 接口，可以直接传给 `Load`/`Update`/`Insert`。

选原生pgxpool而不是 `database/sql`+驱动，换来的是 `poolMinConns` 和
`poolHealthCheckPeriod` 这两个参数的**真实语义**（这两点之前用 `database/sql`
时是没有真正实现的，只能近似）：

- `poolMinConns`：pgxpool会在后台主动把连接数补到这个下限，哪怕服务刚启动、
  一次查询还没发生，池子里也已经有这么多条连接在等着——已经在真实PG上验证过：
  `Open()` 之后不发起任何查询，`pool.Stat().TotalConns()` 立刻就有10条（配置的
  `poolMinConns=10`），这是 `database/sql` 用 `SetMaxIdleConns` 模拟不出来的效果
  （那个只是"空闲时最多留几个不关"的上限，不会主动预热）。
- `poolHealthCheckPeriod`：pgxpool会按这个周期后台主动探活、清掉不健康的连接，
  不再是"等下次用到才发现连接坏了"的惰性检测。

`dbpool.Open()` 直接把这几个属性映射到 `pgxpool.Config` 的对应字段
（`MaxConns`/`MinConns`/`MaxConnLifetime`/`MaxConnIdleTime`/`HealthCheckPeriod`），
和你贴的那版 `newPool` 函数是同一件事，只是参数来源从硬编码改成了从pdb.xml读。

**新增了三个原始XML里没有、但建议加上的属性**

- `statementTimeoutMs` / `idleInTransactionSessionTimeoutMs`：分别对应PG的
  `statement_timeout` 和 `idle_in_transaction_session_timeout`。pgxpool这边是通过
  `AfterConnect` 钩子，在每条物理连接刚建立时执行一次 `SET`（和 `database/sql`+
  `lib/pq` 那版走连接串 `options` 参数的实现方式不同，效果一样：只要连接不重建，
  这个会话级超时就一直生效）。前者防止一条异常慢/写错的查询长期占着连接；后者专门
  防"开了事务、代码bug导致忘记提交/回滚"这种情况——事务挂着不结束，它占的行锁和
  连接会一直不释放（呼应之前讨论过的"多表联动操作，事务包裹多条UPDATE"那部分，
  一旦这种事务因为bug没走到COMMIT/ROLLBACK，这两个超时就是最后的安全网）。
- `appName`：对应PG的 `application_name`，会出现在 `pg_stat_activity` 和日志里。
  当前架构下有多个独立服务共享同一个数据库（lobby服务、AI对话服务），排查问题
  时能一眼看出某个慢查询/某个占着连接不放的会话是哪个服务开的，不用瞎猜。
- `schema`：`schemacheck` 原来硬编码查 `public` schema，现在从这里读，不填默认
  `public`，行为不变，只是不再写死。

以上全部在真实PG上验证过：`poolMinConns=10` 时未发起任何查询就已经预热出连接、
`application_name` 能在 `pg_stat_activity` 里看到、`statement_timeout` 能在设定
时间精确掐断长查询、`idle_in_transaction_session_timeout` 能杀掉忘记提交的空事务。

**`gen` 现在是一站式命令：代码生成 + 建表 + 结构检查**

`go run ./gen/cmd -xml pdb.xml` 一次做三件事：生成Go代码（不需要数据库，永远先跑）；
建表（`ddl.Bootstrap`，只对不存在的表生效）；结构检查（`schemacheck.Check`，只读报告）。
三个包现在统一用 `*pgxpool.Pool`（`schemacheck` 之前是独立的 `database/sql`+`lib/pq`，
这次为了能在一次调用里共用同一个连接池而迁移过去了，`lib/pq` 已经从项目里彻底移除）。
如果只想生成代码、不想碰数据库（比如CI环境里没有DB），加 `-db=false`——这条路径已经
在数据库完全停机的情况下验证过，代码生成不受影响。`ddl/cmd` 和 `schemacheck/cmd` 依然
保留作为独立命令，需要单独跑其中一步时还能用。

## 依赖安装

```bash
go get github.com/jackc/pgx/v5/pgxpool
```

当前 `go.mod` 里有几条 `replace`（把 `gopkg.in/*` 和 `golang.org/x/*` 重定向到
GitHub镜像）——那是这次在沙盒环境里验证代码时，因为网络白名单不包含
`gopkg.in`/`golang.org` 才加的临时绕行方案。**你自己的开发机器如果能正常访问
`proxy.golang.org`，这几条 `replace` 可以直接从 `go.mod` 里删掉**，`go mod tidy`
会自动处理好真正的依赖版本。

## 用法

### 一站式：生成代码 + 建表 + 结构检查

```bash
go run ./gen/cmd -xml pdb.xml
```

输出大致长这样（表不存在时会建表，已存在时会跳过并报告差异）：

```
== 生成Go代码 ==
  已生成 example/model_users_gen.go

== 建表（仅对不存在的表生效，已存在的表不会被改动）==
  [已创建] users        # 或者：[跳过] users 已存在，未做任何改动

== 结构检查（仅报告差异，不会自动执行任何修改）==
  数据库结构与 pdb.xml 一致，没有发现差异。
```

只想生成代码、不碰数据库（比如CI环境没有DB连接）：

```bash
go run ./gen/cmd -xml pdb.xml -db=false
```

`-out`/`-pkg`/`-dsn` 都可以覆盖对应的xml属性，具体见 `-h`。

### 单独跑某一步（仍然保留，按需使用）

```bash
go run ./ddl/cmd -xml pdb.xml          # 只打印DDL，不连数据库
go run ./ddl/cmd -xml pdb.xml -apply   # 只做建表这一步（仅对不存在的表生效）
go run ./schemacheck/cmd -xml pdb.xml  # 只做结构检查这一步
```

### 业务代码怎么用生成出来的东西

```go
pool, err := dbpool.Open(ctx, pdb) // *pgxpool.Pool，实现了DBTX

// 必须显式开事务，Load的 FOR UPDATE 锁才会一直持有到Commit为止
tx, err := pool.Begin(ctx)
defer tx.Rollback(ctx) // Commit成功后这里是空操作，可以放心defer

// 读（这一步已经锁住了这一行）
u, err := model.LoadUserByToken(ctx, tx, requestToken)

// 改（纯内存操作，不碰DB）
u.SetToken(newToken)
u.SetLastLoginAt(time.Now())

// 写 + 提交（这里才真正释放锁）
err = u.Update(ctx, tx)
tx.Commit(ctx)
```

业务代码全程不写SQL。**注意 Load 到 Update/Commit 之间不要再夹带外部慢调用**
（比如调用AI接口）——这段时间行锁一直占着，慢操作会让锁被占用的时间跟着变长，
拖慢所有等着改同一行的其他请求。涉及外部慢调用的场景，应该拆成"外部调用（不
持锁，不在这个事务里）+ 短事务落库（持锁）"两段。

## 当前版本的已知局限（下一步可以扩展的方向）

- 并发策略目前是"全表统一走 `FOR UPDATE` 悲观锁"，还没有像之前讨论的那样按字段区分
  `atomic_incr`（原子加减，如货币，可以不用整行锁，直接用原子UPDATE）/`lock`（悲观锁，
  当前默认）/`readonly`（只读配置），这个可以在 `<variable>` 上加 `strategy` 属性，
  generator按策略生成不同的Update片段
- 复合主键、外键约束这些暂未支持，当前场景（单列自增主键）够用，后续按需加  



```
<pdb url="postgres://app:app@127.0.0.1:5432/gamedb?sslmode=disable"
     genOutput="./example"
     schema="public"
     poolMaxConns="100" poolMinConns="10"
     poolMaxConnLifetime="3600" poolMaxConnIdleTime="1800"
     poolHealthCheckPeriod="60"
     statementTimeoutMs="5000" idleInTransactionSessionTimeoutMs="5000"
     appName="lobby-svc">
	 
	<!--
    CREATE TABLE IF NOT EXISTS users (
        id            BIGINT NOT NULL,
        name          TEXT NOT NULL,
        last_login_at TIMESTAMPTZ NOT NULL,
        created_at    TIMESTAMPTZ NOT NULL,
        token         TEXT NOT NULL
    );

    ALTER TABLE users
        ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (START WITH 1000),
        ADD CONSTRAINT pk_users PRIMARY KEY (id);

    CREATE INDEX index_token ON users (token);

    CREATE INDEX joint_index_id_token ON users (id,token);
    -->
    <bean name="User">
        <variable name="id" type="int64"/>
        <variable name="name" type="string"/>
        <variable name="lastLoginAt" type="time.Time"/>
        <variable name="createdAt" type="time.Time"/>
        <variable name="token" type="string"/>
    </bean>
    <table name="user" bean="User">
        <primaryKey name="pk_user_id" variable="id" autoIncrement="true" start="1000"/>
        <index name="index_token" variable="token" unique="false"/>
        <index name="joint_index_id_token" variable="id,token" unique="false"/>
    </table>
	
	
	<bean name="RedEnvelope">
		<variable name="id" type="int64"/> 自增
		<variable name="uid" type="int64"/> user id
		<variable name="actId" type="int32"/> 活动id
		<variable name="lastRefreshAt" type="int64"/> 上一次刷新的时间
		<variable name="todayCNT" type="int32"/> 今天领了几次
		<variable name="total" type="int32"/> 总共领了几次
	</bean>

	<table name="user_red_envelope" bean="RedEnvelope">
		<primaryKey name="pk_red_envelope_id" variable="id" autoIncrement="true"/>
		<index name="joint_index_uid_act_id" variable="uid,actId" unique="true"/>
	</table>
</pdb>
// 在process之上应该有开事务，没有error和panic的情况下会自动update和提交
func ProcessRedEnvelope(session *Session, req *CRedEnvelope) {
	redenvelope.Get(session.UID, req.ActID).Online(session)
}

func ProcessRedEnvelopeReceive(session *Session, req *CRedEnvelopeReceive) {
	redenvelope.Get(session.UID, req.ActID).Receive(session)
}

// 业务程序员只应该关心如下的代码，有error和panic会自动回滚
type RedEnvelope struct {
	uid int64
	actId int32
	redEnvelope *pbean.RedEnvelope // 这是数据库对象
}

// 不再用select for update
// 请求进来SELECT pg_try_advisory_xact_lock(hashtext(user), uid);
// 拿不到锁会立即返回 false
// GM用pg_advisory_xact_lock(hashtext(user), uid)
// 拿不到锁会等待
// 注册的情况下还没有uid, 尝试用 pg_try_advisory_xact_lock(hashtext(account), account_id)


// select id, uid, act_id, last_refresh_at, today_cnt, total from user_red_envelope where uid=$1, act_id=$2
// 如果没有记录
// insert into user_red_envelope (uid, act_id, today_cnt, total) values ($1, $2, 0, 0) RETURNING id, uid, act_id, last_refresh_at, today_cnt, total;

func Get(uid int64, actId int32) *RedEnvelope {
	// 这里会select for update
	redEnvelope := ptable.UserRedEnvelope.LoadByUidActId(uid, actId)
	if redEnvelope == nil {
		redEnvelope = pbean.NewRedEnvelope()
		redEnvelope.SetUid(uid)
		redEnvelope.SetActId(actId)
		redEnvelope.SetTodayCount(0)
		redEnvelope.SetTotal(0)
		ptable.UserRedEnvelope.Insert(redEnvelope)
	}
	ret := &RedEnvelope{
	    uid: uid,
	    actId: actId,
		redEnvelope: redEnvelope,
	}
	ret.refresh()
	return ret
}

func (this *RedEnvelope) refresh() {
	if !timeutil.IsSameDay(time.Now, this.RedEnvelope.GetLastRefreshAt(), 5) {
		// 跨天刷新次数
		this.redEnvelope.SetTodayCount(0)
		this.redEnvelope.SetLastRefreshAt(time.Now())
	}
}

func (this *RedEnvelope) Online(session *Session) {
	// 红点
	// session.Send 其实是将消息放入上下文，最后统一转成一个结构体下发
	session.Send(&SRedPoint{
		Typ: RedEnvelope,
		Action: ADD,
	})
	
	session.Send(&SRedEnvelope{
		ActId: this.actId,
		LastRefreshAt: this.redEnvelope.GetLastRefreshAt(),
		TodayCount: this.redEnvelope.GetTodayCount(),
	})
}

func (this *RedEnvelope) Receive(session *Session) {
	if this.RedEnvelope.GetTodayCount() >= conf.RedEnvelopeDailyLimit {
		message.SendMsgNotify(this.uid, 180604, null);
		return
	}
	// 奖励道具
	result := ResourceManager.AddItem(this.uid, conf.RedEnvelope.item, FROM_RedEnvelope_ADD)
	if !result.IsSuccess() {
		message.SendMsgNotify(this.uid, 180605, null);
		return
	}
	CommonPanelManager.SendMessage(this.uid, result.getAllAddResources());
	this.redEnvelope.SetTodayCount(this.RedEnvelope.GetTodayCount() + 1)
	this.redEnvelope.SetTotal(this.RedEnvelope.GetTotal() + 1)
	online(session)
}
```