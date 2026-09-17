// schemacheck命令行工具：只读对比数据库实际结构和pdb.xml的差异，输出报告。
// 不会执行任何 CREATE/ALTER/DROP，所有建议SQL都需要人工确认后手动执行。
//
// 用法：
//
//	go run ./schemacheck/cmd -xml pdb.xml -dsn "postgres://app:app@localhost/gamedb?sslmode=disable"
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"pdbgen/schema"
	"pdbgen/schemacheck"
)

func main() {
	xmlPath := flag.String("xml", "pdb.xml", "pdb.xml文件路径")
	dsn := flag.String("dsn", "", "PostgreSQL连接串；不填则使用pdb.xml里的url属性")
	flag.Parse()

	if err := run(*xmlPath, *dsn); err != nil {
		fmt.Fprintln(os.Stderr, "检查失败:", err)
		os.Exit(1)
	}
}

func run(xmlPath, dsn string) error {
	f, err := os.Open(xmlPath)
	if err != nil {
		return fmt.Errorf("打开 %s: %w", xmlPath, err)
	}
	defer f.Close()

	pdb, err := schema.Parse(f)
	if err != nil {
		return err
	}

	if dsn == "" {
		dsn = pdb.URL
	}

	ctx := context.Background()
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("连接数据库: %w", err)
	}
	defer db.Close()

	if err := db.Ping(ctx); err != nil {
		return fmt.Errorf("ping数据库失败: %w", err)
	}

	diffs, err := schemacheck.Check(ctx, db, pdb)
	if err != nil {
		return err
	}

	if len(diffs) == 0 {
		fmt.Println("数据库结构与 pdb.xml 一致，没有发现差异。")
		return nil
	}

	fmt.Printf("发现 %d 处差异（以下建议SQL仅供参考，不会被自动执行，请人工确认后手动执行）：\n\n", len(diffs))
	for _, d := range diffs {
		fmt.Println(d)
		fmt.Println()
	}
	return nil
}
