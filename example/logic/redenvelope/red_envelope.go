package redenvelope

import (
	"context"
	"pdbgen/ptable"
	"time"
)

type RedEnvelope struct {
	*ptable.RedEnvelope
}

func Get(ctx context.Context, uid int64, actId int32) *RedEnvelope {
	redEnvelope := ptable.UserRedEnvelopeTable.GetByUidActId(ctx, uid, actId)
	if redEnvelope == nil {
		redEnvelope = ptable.NewRedEnvelope()
		redEnvelope.SetUid(uid)
		redEnvelope.SetActId(actId)
		redEnvelope.SetTodayCount(0)
		redEnvelope.SetTotal(0)
		ptable.UserRedEnvelopeTable.Insert(ctx, redEnvelope)
	}
	ret := &RedEnvelope{redEnvelope}
	ret.refresh()
	return ret
}

func (e *RedEnvelope) refresh() {
	//if !timeutil.IsSameDay(time.Now, e.LastRefreshAt(), 5) {
	// 跨天刷新次数
	e.SetTodayCount(0)
	e.SetLastRefreshAt(time.Now())
	//}
}
