package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"pdbgen/dbpool"
	"pdbgen/schema"
)

func main() {
	f, err := os.Open("pdb.xml")
	if err != nil {
		log.Fatal(err)
	}
	pdb, err := schema.Parse(f)
	f.Close()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("从pdb.xml读到的池配置: MaxConns=%d MinConns=%d MaxConnLifetime=%ds MaxConnIdleTime=%ds StatementTimeoutMs=%d IdleInTxTimeoutMs=%d AppName=%q\n",
		pdb.PoolMaxConns, pdb.PoolMinConns, pdb.PoolMaxConnLifetime, pdb.PoolMaxConnIdleTime,
		pdb.StatementTimeoutMs, pdb.IdleInTransactionSessionTimeoutMs, pdb.AppName)

	db, err := dbpool.Open(pdb)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		log.Fatal("ping失败:", err)
	}
	fmt.Println("[连接] 成功")

	// 验证1：application_name真的传到了PG那边，能在pg_stat_activity里看到
	var appName string
	must(db.QueryRowContext(ctx, "SELECT application_name FROM pg_stat_activity WHERE pid = pg_backend_pid()").Scan(&appName))
	if appName != pdb.AppName {
		log.Fatalf("application_name没有生效: 期望%q，实际%q", pdb.AppName, appName)
	}
	fmt.Printf("[application_name] 生效，PG侧看到的是: %q\n", appName)

	// 验证2：statement_timeout真的生效——故意跑一个比超时时间长的sleep，应该被服务端强制取消
	start := time.Now()
	_, err = db.ExecContext(ctx, "SELECT pg_sleep(2)") // pdb.xml里配的是5000ms超时，这里先证明"正常范围内不受影响"
	if err != nil {
		log.Fatalf("2秒的sleep不该触发5秒的statement_timeout，但报错了: %v", err)
	}
	fmt.Printf("[statement_timeout下限验证] 2秒查询正常完成，耗时%v，没有被误杀\n", time.Since(start))

	start = time.Now()
	_, err = db.ExecContext(ctx, "SELECT pg_sleep(8)") // 超过5秒的statement_timeout，应该被强制取消
	elapsed := time.Since(start)
	if err == nil {
		log.Fatal("8秒的sleep应该被statement_timeout(5秒)打断，但没有报错，说明这个配置没生效")
	}
	fmt.Printf("[statement_timeout生效] 8秒查询在%v时被强制取消，错误信息: %v\n", elapsed, err)

	// 验证3：idle_in_transaction_session_timeout真的生效——
	// 开一个事务，故意不提交、也不执行任何语句，只是干等，模拟"忘记提交/回滚"这种代码bug
	tx, err := db.BeginTx(ctx, nil)
	must(err)
	_, err = tx.ExecContext(ctx, "SELECT 1") // 先跑一条语句让事务真正"活"起来
	must(err)

	time.Sleep(7 * time.Second) // 超过5秒的idle_in_transaction_session_timeout，什么都不做，纯挂着

	_, err = tx.ExecContext(ctx, "SELECT 1") // 这时候再用这个事务，应该已经被服务端强制杀掉了
	if err == nil {
		log.Fatal("空闲事务应该在5秒后被idle_in_transaction_session_timeout杀掉，但这个事务还能正常用，说明配置没生效")
	}
	fmt.Printf("[idle_in_transaction_session_timeout生效] 空闲事务被服务端强制终止，错误信息: %v\n", err)
	_ = tx.Rollback() // 连接已经被服务端断了，这里的Rollback只是清理本地状态，忽略结果

	fmt.Println("\n全部验证通过：application_name / statement_timeout / idle_in_transaction_session_timeout 都是真实生效的会话级保护，不是摆在XML里没被使用的配置。")
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
