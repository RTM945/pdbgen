package dbpool

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"pdbgen/dbctx"
	"pdbgen/ptable"
)

// 这个文件验证 pdb.xml 的 lock（docs/design.md 2.8）：user_mail 声明了 lock="uid"，
// 每一行由 key 等于 uid 的咨询锁保护，修改要持有它。

// seedMail 给 uid 插一封 state=1 的邮件。插入不需要锁：新建的行没有别的写者。
func seedMail(t *testing.T, ctx context.Context, uid int64) int64 {
	t.Helper()
	var id int64
	if err := WithTx(ctx, func(ctx context.Context) error {
		m := ptable.NewMail()
		m.SetUid(uid)
		m.SetState(1)
		ptable.UserMailTable.Insert(ctx, m)
		id = m.Id()
		return nil
	}); err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}
	return id
}

func cleanupMail(t *testing.T, ctx context.Context, uids ...int64) {
	t.Helper()
	if err := WithTx(ctx, func(ctx context.Context) error {
		_, err := dbctx.SQLTxFromCtx(ctx).Exec(ctx, `DELETE FROM user_mail WHERE uid = ANY($1)`, uids)
		return err
	}); err != nil {
		t.Errorf("清理 user_mail 失败: %v", err)
	}
}

func mailState(t *testing.T, ctx context.Context, id int64) int32 {
	t.Helper()
	var m *ptable.Mail
	if err := Query(ctx, func(ctx context.Context) error {
		m = ptable.UserMailTable.SelectById(ctx, id)
		return nil
	}); err != nil || m == nil {
		t.Fatalf("查询邮件 %d 失败: m=%v err=%v", id, m, err)
	}
	return m.State()
}

// TestOwnedRowNeedsItsLock 持有 A 的锁去改 B 的行：读照常，改的那一行当场 panic，事务回滚；
// 持有 B 的锁（try 或阻塞）就能改。
func TestOwnedRowNeedsItsLock(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uidA := time.Now().UnixNano() + 100
	uidB := uidA + 1
	defer cleanupMail(t, ctx, uidA, uidB)
	mailB := seedMail(t, ctx, uidB)

	msg := catchPanic(t, func() {
		_ = Procedure(ctx, ProcedureOptions{Lock: AdvisoryLock(uidA)}, func(ctx context.Context) error {
			m := ptable.UserMailTable.GetById(ctx, mailB)
			if m.State() != 1 {
				t.Error("读别人的行不受影响")
			}
			m.SetState(2)
			return nil
		})
	})
	if want := fmt.Sprintf("由 uid=%d 的锁保护，当前事务没有持有这把锁", uidB); !strings.Contains(msg, want) {
		t.Fatalf("改别人的行应该 panic（%s），实际: %q", want, msg)
	}
	if got := mailState(t, ctx, mailB); got != 1 {
		t.Fatalf("panic 之后不应该落库，实际 state=%d", got)
	}

	for _, lock := range []LockSpec{TryAdvisoryLock(uidB), AdvisoryLock(uidB)} {
		if err := Procedure(ctx, ProcedureOptions{Lock: lock}, func(ctx context.Context) error {
			m := ptable.UserMailTable.GetById(ctx, mailB)
			m.SetState(m.State() + 1)
			return nil
		}); err != nil {
			t.Fatalf("持有 B 的锁应该能改 B 的行: %v", err)
		}
	}
	if got := mailState(t, ctx, mailB); got != 3 {
		t.Fatalf("两次修改都应该落库，期望 state=3，实际 %d", got)
	}
}

// TestOwnedRowWithoutLock 不加锁的事务（比如 GM 工具忘了加锁）和双 int32 的锁：查出来的行只读，
// 改、删都 panic；列表查询出来的行也一样。本事务插入的新行可以改。
func TestOwnedRowWithoutLock(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uid := time.Now().UnixNano() + 200
	defer cleanupMail(t, ctx, uid)
	id := seedMail(t, ctx, uid)

	for name, tc := range map[string]struct {
		lock  LockSpec
		write func(ctx context.Context)
	}{
		"WithTx SetState":   {LockSpec{}, func(ctx context.Context) { ptable.UserMailTable.GetById(ctx, id).SetState(3) }},
		"WithTx Delete":     {LockSpec{}, func(ctx context.Context) { ptable.UserMailTable.GetById(ctx, id).Delete() }},
		"WithTx DeleteById": {LockSpec{}, func(ctx context.Context) { ptable.UserMailTable.DeleteById(ctx, id) }},
		"双 int32 锁":         {AdvisoryLock2(7, 7), func(ctx context.Context) { ptable.UserMailTable.GetById(ctx, id).SetState(3) }},
		"列表查询": {LockSpec{}, func(ctx context.Context) {
			for _, m := range ptable.UserMailTable.ListByUidConfId(ctx, uid, 0) {
				m.SetState(3)
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			msg := catchPanic(t, func() {
				_ = Procedure(ctx, ProcedureOptions{Lock: tc.lock}, func(ctx context.Context) error {
					tc.write(ctx)
					return nil
				})
			})
			if !strings.Contains(msg, "当前事务没有持有这把锁") {
				t.Fatalf("应该 panic，实际: %q", msg)
			}
		})
	}
	if got := mailState(t, ctx, id); got != 1 {
		t.Fatalf("都不应该落库，实际 state=%d", got)
	}

	var inserted int64
	if err := WithTx(ctx, func(ctx context.Context) error {
		m := ptable.NewMail()
		m.SetUid(uid)
		ptable.UserMailTable.Insert(ctx, m)
		m.SetState(5) // 本事务新建的行，没有别的写者
		inserted = m.Id()
		return nil
	}); err != nil {
		t.Fatalf("插入后修改新行失败: %v", err)
	}
	if got := mailState(t, ctx, inserted); got != 5 {
		t.Fatalf("插入后的修改应该落库，实际 state=%d", got)
	}
}

// TestLostUpdateIsCaught 审查时用探针复现的丢更新：A 的请求顺手改了 B 的行，B 自己的请求同时也在改。
// 以前两个事务都提交成功、丢掉一次修改；现在 A 改的那一行当场 panic，B 的修改照常提交。
func TestLostUpdateIsCaught(t *testing.T) {
	setupTestDB(t)

	ctx := t.Context()
	uidA := time.Now().UnixNano() + 300
	uidB := uidA + 1
	defer cleanupMail(t, ctx, uidA, uidB)
	mailB := seedMail(t, ctx, uidB)

	aRead, bRead := make(chan struct{}), make(chan struct{})
	var aPanic string
	var wg sync.WaitGroup
	wg.Go(func() {
		aPanic = catchPanic(t, func() {
			_ = Procedure(ctx, ProcedureOptions{Lock: AdvisoryLock(uidA)}, func(ctx context.Context) error {
				m := ptable.UserMailTable.GetById(ctx, mailB)
				close(aRead)
				<-bRead
				m.SetState(m.State() + 1)
				return nil
			})
		})
	})
	errB := Procedure(ctx, ProcedureOptions{Lock: AdvisoryLock(uidB)}, func(ctx context.Context) error {
		<-aRead
		m := ptable.UserMailTable.GetById(ctx, mailB)
		close(bRead)
		m.SetState(m.State() + 1)
		return nil
	})
	wg.Wait()

	if errB != nil {
		t.Fatalf("B 自己的请求应该照常提交: %v", errB)
	}
	if !strings.Contains(aPanic, "当前事务没有持有这把锁") {
		t.Fatalf("A 改 B 的行应该 panic，实际: %q", aPanic)
	}
	if got := mailState(t, ctx, mailB); got != 2 {
		t.Fatalf("只有 B 的修改落库，期望 state=2，实际 %d", got)
	}
}
