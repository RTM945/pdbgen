package redenvelope

import (
	"context"
	"pdbgen/example/ptable"
	"time"
)

type RedEnvelope struct {
	*ptable.RedEnvelope
}

func Get(ctx context.Context, uid int64, actId int32) *RedEnvelope {
	// 这里会select for update
	redEnvelope := ptable.UserRedEnvelope.GetByUidActId(ctx, uid, actId)
	if redEnvelope == nil {
		redEnvelope = ptable.NewRedEnvelope()
		redEnvelope.SetUid(uid)
		redEnvelope.SetActId(actId)
		redEnvelope.SetTodayCount(0)
		redEnvelope.SetTotal(0)
		ptable.UserRedEnvelope.Insert(ctx, redEnvelope)
	}
	ret := &RedEnvelope{redEnvelope}
	ret.refresh()
	return ret
}

func (e *RedEnvelope) refresh() {
	if !timeutil.IsSameDay(time.Now, e.LastRefreshAt(), 5) {
		// 跨天刷新次数
		e.SetTodayCount(0)
		e.SetLastRefreshAt(time.Now())
	}
}

func (e *RedEnvelope) Online(session *Session) {
	// 红点
	// session.Send 其实是将消息放入上下文，最后统一转成一个结构体下发
	session.Send(&SRedPoint{
		Typ:    RedEnvelope,
		Action: ADD,
	})

	session.Send(&SRedEnvelope{
		ActId:         e.ActId(),
		LastRefreshAt: e.LastRefreshAt(),
		TodayCount:    e.TodayCount(),
	})
}

func (e *RedEnvelope) Receive(session *Session) {
	if e.RedEnvelope.TodayCount() >= conf.RedEnvelopeDailyLimit {
		message.SendMsgNotify(e.Uid(), 180604, null)
		return
	}
	// 奖励道具
	result := ResourceManager.AddItem(e.Uid(), conf.RedEnvelope.item, FROM_RedEnvelope_ADD)
	if !result.IsSuccess() {
		message.SendMsgNotify(e.Uid(), 180605, null)
		return
	}
	CommonPanelManager.SendMessage(e.Uid(), result.getAllAddResources())
	e.SetTodayCount(e.RedEnvelope.TodayCount() + 1)
	e.SetTotal(e.RedEnvelope.Total() + 1)
	e.Online(session)
}
