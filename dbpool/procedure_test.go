package dbpool

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestProcedureTryLockConflict 第一个 Procedure 持有阻塞锁时，
// 第二个 Procedure 的 try 锁应该立刻返回 ErrAdvisoryLockNotAcquired。
func TestProcedureTryLockConflict(t *testing.T) {
	setupTestDB(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	key := time.Now().UnixNano()
	started := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)

	go func() {
		firstDone <- Procedure(ctx, ProcedureOptions{Lock: AdvisoryLock(key)}, func(ctx context.Context) error {
			close(started)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()

	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("第一个 Procedure 没能启动")
	}

	var secondCalled bool
	err := Procedure(ctx, ProcedureOptions{Lock: TryAdvisoryLock(key)}, func(context.Context) error {
		secondCalled = true
		return nil
	})

	if secondCalled {
		close(release)
		<-firstDone
		t.Fatal("拿不到锁时业务函数不应该执行")
	}
	if !errors.Is(err, ErrAdvisoryLockNotAcquired) {
		close(release)
		<-firstDone
		t.Fatalf("期望 ErrAdvisoryLockNotAcquired，实际: %v", err)
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("第一个 Procedure 失败: %v", err)
	}
}

// TestProcedureAppliesTimeout ProcedureOptions.Timeout 应该体现在传给业务的 ctx 上。
func TestProcedureAppliesTimeout(t *testing.T) {
	setupTestDB(t)

	var gotDeadline bool
	err := Procedure(t.Context(), ProcedureOptions{Timeout: time.Second}, func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		gotDeadline = ok && time.Until(deadline) > 0 && time.Until(deadline) <= time.Second
		return nil
	})
	if err != nil {
		t.Fatalf("Procedure 失败: %v", err)
	}
	if !gotDeadline {
		t.Fatal("ProcedureOptions.Timeout 应该体现在 ctx 的 deadline 上")
	}
}

// TestBlockingLockTimeoutIsNotAcquired 阻塞锁等到 lock_timeout 还没拿到，返回的和 try 锁一样是
// ErrAdvisoryLockNotAcquired——调用方只需要认一个"忙"信号。
func TestBlockingLockTimeoutIsNotAcquired(t *testing.T) {
	setupTestDB(t)

	old := beginSQL
	beginSQL = buildBeginSQL(0, 0, 100)
	t.Cleanup(func() { beginSQL = old })

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	key := time.Now().UnixNano()
	started := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- Procedure(ctx, ProcedureOptions{Lock: AdvisoryLock(key)}, func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	err := Procedure(ctx, ProcedureOptions{Lock: AdvisoryLock(key)}, func(context.Context) error { return nil })
	close(release)
	if !errors.Is(err, ErrAdvisoryLockNotAcquired) {
		t.Fatalf("阻塞锁等超时应该返回 ErrAdvisoryLockNotAcquired，实际: %v", err)
	}
	if err := <-firstDone; err != nil {
		t.Fatalf("第一个 Procedure 失败: %v", err)
	}
}
