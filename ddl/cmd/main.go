// ddl命令行工具。
//
// 默认只打印生成的DDL文本，不碰数据库：
//
//	go run ./ddl/cmd -xml pdb.xml
//
// 加 -apply 才会真正连接数据库执行——而且只会对"数据库里还没有的表"执行
// CREATE TABLE+主键+索引，已经存在的表一律跳过、不做任何ALTER：
//
//	go run ./ddl/cmd -xml pdb.xml -apply
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"pdbgen/ddl"
	"pdbgen/schema"
)

func main() {
	xmlPath := flag.String("xml", "pdb.xml", "pdb.xml文件路径")
	dsn := flag.String("dsn", "", "PostgreSQL连接串；不填则使用pdb.xml里的url属性（仅-apply时需要连接）")
	apply := flag.Bool("apply", false, "true时真正连接数据库建表（仅对不存在的表生效）；不加则只打印DDL，不碰数据库")
	flag.Parse()

	if err := run(*xmlPath, *dsn, *apply); err != nil {
		fmt.Fprintln(os.Stderr, "失败:", err)
		os.Exit(1)
	}
}

func run(xmlPath, dsn string, apply bool) error {
	f, err := os.Open(xmlPath)
	if err != nil {
		return fmt.Errorf("打开 %s: %w", xmlPath, err)
	}
	defer f.Close()

	pdb, err := schema.Parse(f)
	if err != nil {
		return err
	}

	tableDDLs, err := ddl.Generate(pdb)
	if err != nil {
		return err
	}

	if !apply {
		for i, td := range tableDDLs {
			if i > 0 {
				fmt.Println()
			}
			fmt.Println(td.String())
		}
		fmt.Fprintln(os.Stderr, "\n（以上只是打印，没有连接数据库；加 -apply 才会真正执行，且只对不存在的表生效）")
		return nil
	}

	if dsn == "" {
		dsn = pdb.URL
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("连接数据库: %w", err)
	}
	defer pool.Close()

	results, err := ddl.Bootstrap(ctx, pool, pdb)
	if err != nil {
		return err
	}

	for _, r := range results {
		switch r.Action {
		case ddl.ActionCreated:
			fmt.Printf("[已创建] %s\n", r.Table)
			for _, stmt := range r.Statements {
				fmt.Println("  " + stmt)
			}
		case ddl.ActionSkippedExists:
			fmt.Printf("[跳过] %s 已存在，未做任何改动\n", r.Table)
		}
	}
	return nil
}
