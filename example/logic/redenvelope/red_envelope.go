// Package redenvelope 是推荐的业务写法：业务结构体内嵌生成的 *ptable.RedEnvelope，
// 所有修改只调 SetXxx，落库、回滚、提交后发包都交给 dbpool.Procedure。
//
// 玩家请求在持有 uid 锁的 Procedure 里调用（user_red_envelope 归 uid 所有）：
//
//	err := dbpool.Procedure(ctx, dbpool.ProcedureOptions{Lock: dbpool.TryAdvisoryLock(uid)}, func(ctx context.Context) error {
//		e := redenvelope.Get(ctx, uid, actID, false, time.Now())
//		return e.Receive(dailyLimit)
//	})
//
// 只读的查询（GM 后台看数据）在 dbpool.Query 里用 readonly=true。
package redenvelope

import (
	"context"
	"errors"
	"time"

	"pdbgen/ptable"
)

// resetHour 是每天换日的钟点：5 点之前算前一天。
const resetHour = 5

// ErrDailyLimit 今天的次数用完了。返回它，Procedure 回滚，什么都不会改。
var ErrDailyLimit = errors.New("redenvelope: daily limit reached")

type RedEnvelope struct {
	*ptable.RedEnvelope
	readonly bool
}

// Get 取 uid 在活动 actID 里的红包记录，没有就新建一条（readonly 时只在内存里新建，不插入）。
// readonly=false 必须在事务里调用，readonly=true 必须在 dbpool.Query 里调用。
func Get(ctx context.Context, uid int64, actID int32, readonly bool, now time.Time) *RedEnvelope {
	var r *ptable.RedEnvelope
	if readonly {
		r = ptable.UserRedEnvelopeTable.SelectByUidActId(ctx, uid, actID)
	} else {
		r = ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid, actID)
	}
	if r == nil {
		r = ptable.NewRedEnvelope()
		r.SetUid(uid)
		r.SetActId(actID)
		r.SetLastRefreshAt(now.Unix())
		if !readonly {
			ptable.UserRedEnvelopeTable.Insert(ctx, r)
		}
	}
	e := &RedEnvelope{r, readonly}
	e.refresh(now)
	return e
}

// refresh 跨了游戏日就把今天的次数清零。只读的记录不改：改了也不会落库，还会 panic。
func (e *RedEnvelope) refresh(now time.Time) {
	if e.readonly || sameDay(time.Unix(e.LastRefreshAt(), 0), now) {
		return
	}
	e.SetTodayCount(0)
	e.SetLastRefreshAt(now.Unix())
}

// Receive 领一次。校验放在最前面：失败就返回错误，整个 Procedure 回滚。
func (e *RedEnvelope) Receive(dailyLimit int32) error {
	if e.TodayCount() >= dailyLimit {
		return ErrDailyLimit
	}
	e.SetTodayCount(e.TodayCount() + 1)
	e.SetTotal(e.Total() + 1)
	return nil
}

// sameDay 报告 a 和 b 是不是同一个游戏日：每天 resetHour 点换日，按 b 所在的时区算。
func sameDay(a, b time.Time) bool {
	shift := resetHour * time.Hour
	ay, am, ad := a.In(b.Location()).Add(-shift).Date()
	by, bm, bd := b.Add(-shift).Date()
	return ay == by && am == bm && ad == bd
}
