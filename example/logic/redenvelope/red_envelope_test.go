package redenvelope

import (
	"errors"
	"testing"
	"time"

	"pdbgen/ptable"
)

func TestSameDay(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)
	at := func(day, hour int) time.Time { return time.Date(2026, 11, day, hour, 0, 0, 0, loc) }
	for _, tc := range []struct {
		a, b time.Time
		want bool
	}{
		{at(1, 6), at(1, 23), true},
		{at(1, 4), at(1, 6), false}, // 5 点换日
		{at(1, 23), at(2, 4), true}, // 第二天 5 点之前还算前一天
		{at(1, 23), at(2, 5), false},
	} {
		if got := sameDay(tc.a, tc.b); got != tc.want {
			t.Errorf("sameDay(%v, %v) = %v，期望 %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestRefreshAndReceive 同一个游戏日里次数累加，跨天清零；达到上限返回 ErrDailyLimit。
// 用 NewRedEnvelope 建的新对象不需要事务，可以直接测业务逻辑。
func TestRefreshAndReceive(t *testing.T) {
	day1 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.Local)
	r := ptable.NewRedEnvelope()
	r.SetLastRefreshAt(day1.Unix())
	e := &RedEnvelope{RedEnvelope: r}

	e.refresh(day1.Add(time.Hour))
	for range 2 {
		if err := e.Receive(2); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Receive(2); !errors.Is(err, ErrDailyLimit) {
		t.Fatalf("超过上限应该返回 ErrDailyLimit，实际: %v", err)
	}

	e.refresh(day1.Add(24 * time.Hour))
	if e.TodayCount() != 0 || e.Total() != 2 {
		t.Fatalf("跨天应该清零今天的次数、保留总数，实际 today=%d total=%d", e.TodayCount(), e.Total())
	}
}
