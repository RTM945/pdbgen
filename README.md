# pdbgen

面向游戏服务端的 PostgreSQL 数据访问框架：`pdb.xml` 定义表结构，生成器产出建表 / 迁移 SQL 和带脏标记的 Go 代码，运行时在一个事务里把修改合并成一次往返落库。

- 一次请求一个事务，事务开头拿玩家的 uid 咨询锁，同一个玩家的请求一个接一个执行；
- 业务只调 `SetXxx`，提交前只把改过的列合并成一个 Batch 发出；
- 同一个事务里同一行只有一个对象，失败时内存里的对象恢复成进事务时的样子；
- `AfterCommit` / `AfterRollback` 在结果确定之后执行，发包不会早于提交；
- 用错了当场失败：事务外改实体、不持锁改别的玩家的行、嵌套事务、`pdb.xml` 写错的属性。

```go
schema, err := readxml.LoadSchema("pdb.xml")
if err != nil {
    return err
}
if err := dbpool.Init(ctx, schema); err != nil { // 或者 dbpool.Open(ctx, dbpool.Config{...})
    return err
}
defer dbpool.Close() // 排在服务优雅关闭之后

// 玩家请求：拿 uid 的 try 锁，上一个请求还没结束就立即返回 ErrAdvisoryLockNotAcquired
err = dbpool.Procedure(ctx, dbpool.ProcedureOptions{Lock: dbpool.TryAdvisoryLock(uid)}, func(ctx context.Context) error {
    e := ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid, actID)
    if e == nil {
        e = ptable.NewRedEnvelope()
        e.SetUid(uid)
        e.SetActId(actID)
        ptable.UserRedEnvelopeTable.Insert(ctx, e)
    }
    if e.TodayCount() >= dailyLimit {
        return ErrDailyLimit // 返回 error 或 panic 都会回滚，什么都不会改
    }
    e.SetTodayCount(e.TodayCount() + 1) // 提交前只 UPDATE 这一列
    dbctx.AfterCommit(ctx, func(context.Context) { session.Send(reply) })
    return nil
})
// err 是 nil、业务自己的错误、SQL 错误（唯一键冲突用 errors.As 取 *pgconn.PgError）、
// dbpool.ErrAdvisoryLockNotAcquired（玩家忙）、dbpool.ErrTimeout 或 dbpool.ErrCommitUnknown

// 事务外的只读查询
err = dbpool.Query(ctx, func(ctx context.Context) error {
    mails = ptable.UserMailTable.SelectListByUidConfId(ctx, uid, confID)
    return nil
})
```

更完整的业务写法见 [example/logic/redenvelope](example/logic/redenvelope/red_envelope.go)。

## pdb.xml

```xml
<?xml version="1.0" encoding="UTF-8"?>
<pdb url="postgres://app:app@127.0.0.1:6432/gamedb?sslmode=disable"
     genOutput="./ptable" schema="public"
     poolMaxConns="100" statementTimeoutMs="5000" lockTimeoutMs="1000" transactionTimeoutMs="5000"
     showSQL="true" appName="lobby-svc">

    <bean name="RedEnvelope">
        <variable name="id" type="int64"/> 自增主键
        <variable name="uid" type="int64"/> 玩家
        <variable name="actId" type="int32"/> 活动
        <variable name="todayCount" type="int32"/> 今天领了几次
    </bean>

    <!-- lock="uid"：每一行归 uid 那把咨询锁管，不持有它的事务只能读 -->
    <table name="user_red_envelope" bean="RedEnvelope" lock="uid">
        <primaryKey variable="id" autoIncrement="true"/>
        <index variable="uid,actId" unique="true"/> 生成 GetByUidActId / SelectByUidActId
    </table>
</pdb>
```

属性写错、元素写错都会报错并指出行号。全部属性和生成规则见 [docs/design.md](docs/design.md) 第 4 节。

## 生成

```bash
go run ./gen/bean -schema ./pdb.xml           # 生成 ptable/<表名>.pdb.go，删掉不再需要的生成文件
go run ./gen/ddl -schema ./pdb.xml > ddl.sql  # 对照库里的结构生成增量 DDL（包在一个事务里），检查后手工执行
```

在用 pdbgen 的项目里，把两个生成器登记成工具，版本跟着 go.mod 走：

```bash
go get -tool pdbgen/gen/bean pdbgen/gen/ddl
go tool bean -schema app/test/pdb.xml
go tool ddl -schema app/test/pdb.xml > ddl.sql
```

## 本地 PostgreSQL + PgBouncer

```bash
docker pull postgres:14.24-alpine3.23
docker pull edoburu/pgbouncer
docker run --name my-postgres -e POSTGRES_PASSWORD=mysecretpassword -d postgres:14.24-alpine3.23
docker exec my-postgres psql -U postgres -c "CREATE USER app WITH PASSWORD 'app';"
docker exec my-postgres psql -U postgres -c "CREATE DATABASE gamedb OWNER app;"
# dbpool.Query 没有事务，SET LOCAL 管不到它，服务端的语句超时用角色级设置
docker exec my-postgres psql -U postgres -c "ALTER ROLE app SET statement_timeout = '5s';"
docker network create pgnet
docker network connect pgnet my-postgres
```

用 app 登录建表，表的所有者是 app，应用就不会报权限问题：

```bash
docker exec -it my-postgres psql -U app -d gamedb
```

让第三方 SQL 工具也能经过 PgBouncer 登录。读 `pg_shadow` 需要超级用户，所以用 postgres 登录 gamedb 执行：

```sql
CREATE SCHEMA IF NOT EXISTS pgbouncer;
CREATE OR REPLACE FUNCTION pgbouncer.get_auth(p_usename text)
RETURNS TABLE(usename name, passwd text) AS $$
  SELECT usename, passwd FROM pg_catalog.pg_shadow WHERE usename = p_usename;
$$ LANGUAGE sql SECURITY DEFINER;

GRANT USAGE ON SCHEMA pgbouncer TO app;
GRANT EXECUTE ON FUNCTION pgbouncer.get_auth(text) TO app;
```

```bash
docker run -d \
  --name my-pgbouncer \
  --network pgnet \
  -p 6432:6432 \
  -e DATABASE_URL="postgres://app:app@my-postgres:5432/gamedb" \
  -e LISTEN_PORT=6432 \
  -e AUTH_TYPE=scram-sha-256 \
  -e AUTH_USER=app \
  -e AUTH_QUERY="SELECT * FROM pgbouncer.get_auth(\$1)" \
  -e POOL_MODE=transaction \
  -e DEFAULT_POOL_SIZE=20 \
  -e MAX_CLIENT_CONN=100 \
  edoburu/pgbouncer:latest
```

`go test ./...` 连上面这套库跑集成测试，连不上时跳过；`PDBGEN_REQUIRE_DB=1 go test ./...` 连不上直接失败。

语义、技术选型、生成器规则、已知缺口和变更记录见 [docs/design.md](docs/design.md)；怎么改这个仓库见 [CLAUDE.md](CLAUDE.md)。
