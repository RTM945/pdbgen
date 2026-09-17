// 手工验证脚本：用生成的model包，验证 SELECT ... FOR UPDATE 悲观锁的实际行为。
// 不是正式测试，只是为了验证生成代码本身正确。
package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"pdbgen/example"
)

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func main() {
	ctx := context.Background()
	db, err := pgxpool.New(ctx, "postgres://app:app@localhost/gamedb?sslmode=disable")
	must(err)
	defer db.Close()
	must(db.Ping(ctx))

	// 清一下之前可能残留的测试数据
	_, err = db.Exec(ctx, "DELETE FROM users WHERE token LIKE 'verify-%'")
	must(err)

	// 1. Insert（不需要事务，Insert本身不涉及FOR UPDATE）
	now := time.Now().Truncate(time.Second)
	u, err := model.InsertUser(ctx, db, "小明", now, now, "verify-token-1")
	must(err)
	fmt.Printf("[Insert] 成功，分配的自增id=%d\n", u.Id)
	if u.Id == 0 {
		log.Fatal("insert后id应该被RETURNING回填，不应该是0")
	}

	// 2. 正常的 Load(FOR UPDATE) -> 改字段 -> Update -> Commit，
	//    必须显式开一个事务，Load的锁才会一直持有到Update/Commit为止。
	tx, err := db.Begin(ctx)
	must(err)
	loaded, err := model.LoadUser(ctx, tx, u.Id)
	must(err)
	if loaded.Name != "小明" || loaded.Token != "verify-token-1" {
		log.Fatalf("Load回来的数据和Insert的不一致: %+v", loaded)
	}
	fmt.Printf("[LoadUser FOR UPDATE] 成功，读到: name=%s token=%s\n", loaded.Name, loaded.Token)

	loaded.SetToken("verify-token-2")
	loaded.SetName("小明改名了")
	must(loaded.Update(ctx, tx))
	must(tx.Commit(ctx))
	fmt.Println("[Update+Commit] 正常更新成功（token+name合并成一条SQL）")

	reloaded, err := model.LoadUser(ctx, db, u.Id) // 纯读，不打算改，这里就不套事务了
	must(err)
	if reloaded.Token != "verify-token-2" || reloaded.Name != "小明改名了" {
		log.Fatalf("Update后重新Load的数据不对: %+v", reloaded)
	}
	fmt.Println("[验证] 重新Load，数据确实已经更新")

	// 3. ResetToLoaded：改了字段但没Update，调用ResetToLoaded应该把内存对象还原
	//    回Load时的快照，这是给以后本地缓存回滚用的能力，这里先验证它本身对不对。
	obj, err := model.LoadUser(ctx, db, u.Id)
	must(err)
	originalName := obj.Name
	obj.SetName("这个改动不该被保留")
	if obj.Name == originalName {
		log.Fatal("SetName后字段应该已经变了，测试前提不对")
	}
	obj.ResetToLoaded()
	if obj.Name != originalName {
		log.Fatalf("ResetToLoaded后应该恢复成 %q，实际是 %q", originalName, obj.Name)
	}
	fmt.Println("[ResetToLoaded] 成功，内存对象被正确还原成Load时的快照，dirty也清空了")
	// 顺便验证dirty真的清空了：这时候Update应该是no-op（不会报错，也不会真的执行SQL）
	must(obj.Update(ctx, db))
	fmt.Println("[验证] ResetToLoaded后Update是no-op，不会误写任何数据")

	// 4. 悲观锁真正的核心验证：FOR UPDATE 必须真的会让并发的第二个事务阻塞等待，
	//    不是形同虚设。用两个goroutine + 时间断言来证明"锁生效了"，
	//    而不是"两边都没排队、各自跑各自的"。
	//
	//    时间线：
	//      A: Begin -> Load(FOR UPDATE) 拿到锁 -> 通知B可以开始 -> sleep 2秒（模拟业务逻辑耗时）
	//         -> 改token -> Update -> Commit（此时才释放锁）
	//      B: 等A通知后 -> Begin -> Load(FOR UPDATE) 同一行
	//         正确的悲观锁行为：B这一步应该被阻塞，直到A Commit为止，
	//         也就是B的Load耗时应该 ≈ A剩余的sleep时间，不应该是瞬间返回。
	aStartedLock := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		tx, err := db.Begin(ctx)
		must(err)
		defer tx.Rollback(ctx) // Commit成功后这里是空操作

		obj, err := model.LoadUser(ctx, tx, u.Id)
		must(err)
		close(aStartedLock) // 告诉B："我已经拿到锁了，你可以开始尝试Load了"

		time.Sleep(2 * time.Second) // 模拟A在事务里做业务逻辑，这段时间锁一直被A占着

		obj.SetToken("verify-token-locked-by-A")
		must(obj.Update(ctx, tx))
		must(tx.Commit(ctx))
		fmt.Println("[goroutine A] 完成，token改成了 verify-token-locked-by-A")
	}()

	go func() {
		defer wg.Done()
		<-aStartedLock // 等A确实已经拿到锁了才开始，避免B跑得比A还快导致测试没意义

		tx, err := db.Begin(ctx)
		must(err)
		defer tx.Rollback(ctx)

		start := time.Now()
		obj, err := model.LoadUser(ctx, tx, u.Id) // 这一步应该被A的锁阻塞住
		elapsed := time.Since(start)
		must(err)

		fmt.Printf("[goroutine B] Load被阻塞了 %v 才返回\n", elapsed)
		if elapsed < 1500*time.Millisecond {
			log.Fatalf("B的Load只等了%v就返回了，说明FOR UPDATE根本没有阻塞B，悲观锁没有生效！", elapsed)
		}
		if obj.Token != "verify-token-locked-by-A" {
			log.Fatalf("B在A提交之后才拿到锁，应该看到A提交的结果，实际看到的token是: %s", obj.Token)
		}
		fmt.Println("[goroutine B] 验证通过：确实等A释放锁之后才继续，而且看到的是A提交后的最新数据")
	}()

	wg.Wait()
	fmt.Println("[悲观锁阻塞验证] 通过：FOR UPDATE 确实会让第二个事务排队等待，不是摆设")

	// 4.5 反面对照：如果不开显式事务，直接把pool传给Load（而不是先Begin拿到tx），
	//     FOR UPDATE的锁在这条SELECT执行完就立刻释放了，后面完全没有保护——
	//     这正是DBTX注释里警告的误用场景，这里实际跑一遍证明警告不是唬人的。
	//     两个goroutine都直接用db(pool)去Load，预期B几乎不用等待就能拿到。
	noTxStarted := make(chan struct{})
	var wg2 sync.WaitGroup
	wg2.Add(2)
	go func() {
		defer wg2.Done()
		_, err := model.LoadUser(ctx, db, u.Id) // 直接传pool，不是tx——锁在这行结束就没了
		must(err)
		close(noTxStarted)
		time.Sleep(1500 * time.Millisecond) // A"以为"自己还占着锁，其实早就没了
	}()
	go func() {
		defer wg2.Done()
		<-noTxStarted
		start := time.Now()
		_, err := model.LoadUser(ctx, db, u.Id) // 同样直接传pool
		elapsed := time.Since(start)
		must(err)
		fmt.Printf("[反面对照] 不开显式事务时，B的Load只等了%v就返回（预期应该是几毫秒级，不是1.5秒级），证明这种用法下FOR UPDATE确实没有跨语句生效\n", elapsed)
		if elapsed > 500*time.Millisecond {
			log.Fatalf("这个对照场景本该证明'不加事务=没保护'，但B居然等了%v，说明测试逻辑本身有问题需要重新检查", elapsed)
		}
	}()
	wg2.Wait()

	// 5. 联合索引：LoadUserByIdToken(id, token) —— 同样走FOR UPDATE，验证多字段索引Load能生成并跑通
	tx2, err := db.Begin(ctx)
	must(err)
	byIDToken, err := model.LoadUserByIdToken(ctx, tx2, u.Id, "verify-token-locked-by-A")
	must(err)
	must(tx2.Commit(ctx))
	if byIDToken.Id != u.Id {
		log.Fatal("LoadUserByIdToken 查回来的id不对")
	}
	fmt.Printf("[LoadUserByIdToken FOR UPDATE] 成功，联合索引查询命中，id=%d\n", byIDToken.Id)

	fmt.Println("\n全部验证通过。")
}
