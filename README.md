# pdbgen
```
<?xml version="1.0" encoding="UTF-8"?>
<pdb url="postgres://app:app@127.0.0.1:6432/gamedb?sslmode=disable"
     genOutput="./ptable"
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
        last_login_at BIGINT NOT NULL,
        created_at    BIGINT NOT NULL,
        token         TEXT NOT NULL
    );

    ALTER TABLE user
        ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (START WITH 1000),
        ADD CONSTRAINT pk_user PRIMARY KEY (id);

    CREATE INDEX index_token ON users (token);

    CREATE INDEX joint_index_id_token ON users (id,token);
    -->
    <bean name="user">
        <variable name="id" type="int64"/>
        <variable name="name" type="string"/>
        <variable name="lastLoginAt" type="int64"/>
        <variable name="createdAt" type="int64"/>
        <variable name="token" type="string"/>
    </bean>
    <table name="user" bean="user">
        <primaryKey name="pk_user" variable="id" autoIncrement="true" start="1000"/>
        <index name="index_token" variable="token" unique="false"/>
        <index name="joint_index_id_token" variable="id,token" unique="false"/>
    </table>


    <bean name="RedEnvelope">
        <variable name="id" type="int64"/> 自增
        <variable name="uid" type="int64"/> user id
        <variable name="actId" type="int32"/> 活动id
        <variable name="lastRefreshAt" type="int64"/> 上一次刷新的时间
        <variable name="todayCount" type="int32"/> 今天领了几次
        <variable name="total" type="int32"/> 总共领了几次
    </bean>

    <table name="user_red_envelope" bean="RedEnvelope">
        <primaryKey name="pk_user_red_envelope_id" variable="id" autoIncrement="true"/>
        <index name="joint_index_user_red_envelope_uid_act_id" variable="uid,actId" unique="true"/>
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
  *ptable.RedEnvelope // 这是数据库对象
  readonly bool
}

// 不再用select for update
// 玩家请求进来SELECT pg_try_advisory_xact_lock(hashtext(user), uid);
// 拿不到锁会立即返回 false
// GM和RPC的写请求用pg_advisory_xact_lock(hashtext(user), uid)
// 拿不到锁会等待
// 注册的情况下还没有uid, 尝试用 pg_try_advisory_xact_lock(hashtext(account), account_id)

// GM和RPC的只读请求需要明确标注readonly
// 我怀念java可以用注解在上层方法体上区分要不要用事务，是try_lock还是直接lock，再用反射aop加获取事务或者conn的代码

// select id, uid, act_id, last_refresh_at, today_count, total from user_red_envelope where uid=$1, act_id=$2
// 如果没有记录
// insert into user_red_envelope (uid, act_id, today_cnt, total) values ($1, $2, 0, 0) RETURNING id, uid, act_id, last_refresh_at, today_cnt, total;

func Get(ctx context.Context, uid int64, actId int32, readonly bool) *RedEnvelope {
	var redEnvelope *ptable.RedEnvelope
	if readonly {
		redEnvelope = ptable.UserRedEnvelopeTable.SelectByUidActId(ctx, uid, actId)
	} else {
		redEnvelope = ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid, actId)
	}
	if redEnvelope == nil {
		redEnvelope = ptable.NewRedEnvelope()
		redEnvelope.SetUid(uid)
		redEnvelope.SetActId(actId)
		redEnvelope.SetTodayCount(0)
		redEnvelope.SetTotal(0)
		redEnvelope.SetLastRefreshAt(0)
		if !readonly {
			ptable.UserRedEnvelopeTable.Insert(ctx, redEnvelope)
		}
	}
	ret := &RedEnvelope{redEnvelope, readonly}
	ret.refresh()
	return ret
}

func (this *RedEnvelope) refresh() {
    if e.readonly {
		return
	}
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

# todo 
<del>目前只能读写一行，需要多行的代码生成和业务逻辑支持</del>  

对于ResetToLoaded，因为在一次请求中可能涉及到多个表的改变，可能需要在ctx中记log，要回滚时log中的对象按顺序全部回滚  

咨询锁的函数签名是单bigint和两个int，hashtext('user', uid)是用两个int拼起来的，在uid超过int范围时就会出错。预想修改成默认的user锁直接用uid，而其他业务锁用代码生成枚举值。


#pg
```
docker pull postgres:14.24-alpine3.23
docker pull edoburu/pgbouncer
docker run --name my-postgres -e POSTGRES_PASSWORD=mysecretpassword -d postgres:14.24-alpine3.23
docker exec my-postgres psql -U postgres -c "CREATE USER app WITH PASSWORD 'app';"
docker exec my-postgres psql -U postgres -c "CREATE DATABASE gamedb OWNER app;"
docker network create pgnet
docker network connect pgnet my-postgres

-- 用app user登录创建表 所有权属于app 这样应用就不会报权限问题
docker exec -it my-postgres psql -U app -d gamedb

-- 解决第三方sql工具无法连接pgBouncer的问题
CREATE SCHEMA IF NOT EXISTS pgbouncer;
CREATE OR REPLACE FUNCTION pgbouncer.get_auth(p_usename text)
RETURNS TABLE(usename name, passwd text) AS $$
  SELECT usename, passwd FROM pg_catalog.pg_shadow WHERE usename = p_usename;
$$ LANGUAGE sql SECURITY DEFINER;

GRANT USAGE ON SCHEMA pgbouncer TO app;
GRANT EXECUTE ON FUNCTION pgbouncer.get_auth(text) TO app;


-- Code generated by pdb ddl generator; DO NOT EDIT.

CREATE SCHEMA IF NOT EXISTS "public";

-- ============================================================
-- TABLE: public.user
-- ============================================================

CREATE TABLE IF NOT EXISTS "public"."user" (
        "id" BIGINT NOT NULL,
        "name" TEXT NOT NULL DEFAULT '',
        "last_login_at" BIGINT NOT NULL DEFAULT 0,
        "created_at" BIGINT NOT NULL DEFAULT 0,
        "token" TEXT NOT NULL DEFAULT ''
);

ALTER TABLE "public"."user"
        ALTER COLUMN "id" ADD GENERATED ALWAYS AS IDENTITY (START WITH 1000),
        ADD CONSTRAINT "pk_user_id" PRIMARY KEY ("id");


-- ============================================================
-- TABLE: public.user_red_envelope
-- ============================================================

CREATE TABLE IF NOT EXISTS "public"."user_red_envelope" (
        "id" BIGINT NOT NULL,
        "uid" BIGINT NOT NULL DEFAULT 0,
        "act_id" INTEGER NOT NULL DEFAULT 0,
        "last_refresh_at" BIGINT NOT NULL DEFAULT 0,
        "today_count" INTEGER NOT NULL DEFAULT 0,
        "total" INTEGER NOT NULL DEFAULT 0
);

ALTER TABLE "public"."user_red_envelope"
        ALTER COLUMN "id" ADD GENERATED ALWAYS AS IDENTITY,
        ADD CONSTRAINT "pk_user_red_envelope_id" PRIMARY KEY ("id");

CREATE UNIQUE INDEX IF NOT EXISTS "idx_user_red_envelope_uid_act_id" ON "public"."user_red_envelope" ("uid", "act_id");


-- ============================================================
-- TABLE: public.user_mail
-- ============================================================

CREATE TABLE IF NOT EXISTS "public"."user_mail" (
        "id" BIGINT NOT NULL,
        "uid" BIGINT NOT NULL DEFAULT 0,
        "conf_id" INTEGER NOT NULL DEFAULT 0,
        "state" INTEGER NOT NULL DEFAULT 0,
        "receive_at" BIGINT NOT NULL DEFAULT 0,
        "params" TEXT NOT NULL DEFAULT '',
        "award_list" TEXT NOT NULL DEFAULT ''
);

ALTER TABLE "public"."user_mail"
        ALTER COLUMN "id" ADD GENERATED ALWAYS AS IDENTITY,
        ADD CONSTRAINT "pk_user_mail_id" PRIMARY KEY ("id");

CREATE INDEX IF NOT EXISTS "idx_user_mail_uid_conf_id" ON "public"."user_mail" ("uid", "conf_id");


-- ============================================================
-- TABLE: public.user_gm_mail
-- ============================================================

CREATE TABLE IF NOT EXISTS "public"."user_gm_mail" (
        "id" BIGINT NOT NULL,
        "params" TEXT NOT NULL DEFAULT '',
        "award_list" TEXT NOT NULL DEFAULT '',
        "start_at" BIGINT NOT NULL DEFAULT 0,
        "end_at" BIGINT NOT NULL DEFAULT 0,
        "channel" TEXT NOT NULL DEFAULT '',
        "user_created_at" BIGINT NOT NULL DEFAULT 0,
        "condition" TEXT NOT NULL DEFAULT ''
);

ALTER TABLE "public"."user_gm_mail"
        ALTER COLUMN "id" ADD GENERATED ALWAYS AS IDENTITY,
        ADD CONSTRAINT "pk_user_gm_mail_id" PRIMARY KEY ("id");


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

# build 
cd ../ddl/
go build -o genddl.exe

cd ..\bean\        
go build -o genbeans.exe
