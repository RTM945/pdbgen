# pdbgen
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
    CREATE TABLE IF NOT EXISTS user (
        id            BIGINT NOT NULL,
        name          TEXT NOT NULL,
        last_login_at TIMESTAMPTZ NOT NULL,
        created_at    TIMESTAMPTZ NOT NULL,
        token         TEXT NOT NULL
    );

    ALTER TABLE user
        ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (START WITH 1000),
        ADD CONSTRAINT pk_user PRIMARY KEY (id);

    CREATE UNIQUE INDEX index_token ON user (token) ;

    CREATE UNIQUE INDEX joint_index_id_token ON user (id,token);
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
        <index name="index_token" variable="token" unique="true"/>
        <index name="joint_index_id_token" variable="id,token" unique="true"/>
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
  *ptable.RedEnvelope // 这是数据库对象
}

// 不再用select for update
// 请求进来SELECT pg_try_advisory_xact_lock(hashtext(user), uid);
// 拿不到锁会立即返回 false
// GM用pg_advisory_xact_lock(hashtext(user), uid)
// 拿不到锁会等待
// 注册的情况下还没有uid, 尝试用 pg_try_advisory_xact_lock(hashtext(account), account_id)


// select id, uid, act_id, last_refresh_at, today_count, total from user_red_envelope where uid=$1, act_id=$2
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