// pdbgen一站式命令行工具：读取 pdb.xml，做三件事：
//  1. 生成Go持久化代码（这一步不需要数据库，随时能跑）
//  2. 建表（仅对数据库里还不存在的表执行，已存在的表绝不touch）
//  3. 对比数据库实际结构和xml定义的差异，报告仅供参考，不会自动执行任何ALTER
//
// 用法：
//
//	go run ./gen/cmd -xml pdb.xml                 // 三步都跑
//	go run ./gen/cmd -xml pdb.xml -db=false        // 只生成代码，不连数据库（比如CI环境里没有DB）
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5/pgxpool"

	"pdbgen/ddl"
	"pdbgen/gen"
	"pdbgen/schema"
	"pdbgen/schemacheck"
)

func main() {
	xmlPath := flag.String("xml", "pdb.xml", "pdb.xml文件路径")
	outDir := flag.String("out", "", "生成代码的输出目录；不填则使用pdb.xml里的genOutput属性")
	pkgName := flag.String("pkg", "model", "生成代码的package名")
	dsn := flag.String("dsn", "", "PostgreSQL连接串；不填则使用pdb.xml里的url属性")
	syncDB := flag.Bool("db", false, "是否连接数据库做建表+结构检查；false则只生成代码，不需要数据库")
	flag.Parse()

	if err := run(*xmlPath, *outDir, *pkgName, *dsn, *syncDB); err != nil {
		fmt.Fprintln(os.Stderr, "失败:", err)
		os.Exit(1)
	}
}

func run(xmlPath, outDir, pkgName, dsn string, syncDB bool) error {
	f, err := os.Open(xmlPath)
	if err != nil {
		return fmt.Errorf("打开 %s: %w", xmlPath, err)
	}
	pdb, err := schema.Parse(f)
	f.Close()
	if err != nil {
		return err
	}

	// ---- 第1步：生成Go代码，不需要数据库，永远先跑这一步 ----
	fmt.Println("== 生成Go代码 ==")
	if err := generateCode(pdb, outDir, pkgName); err != nil {
		return fmt.Errorf("生成代码失败: %w", err)
	}

	if !syncDB {
		fmt.Println("\n（-db=false，跳过建表和结构检查）")
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
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping数据库失败: %w", err)
	}

	// ---- 第2步：建表，只对数据库里还不存在的表生效 ----
	fmt.Println("\n== 建表（仅对不存在的表生效，已存在的表不会被改动）==")
	if err := bootstrapTables(ctx, pool, pdb); err != nil {
		return fmt.Errorf("建表失败: %w", err)
	}

	// ---- 第3步：结构检查，只读，只报告，不执行任何ALTER ----
	fmt.Println("\n== 结构检查（仅报告差异，不会自动执行任何修改）==")
	if err := checkSchema(ctx, pool, pdb); err != nil {
		return fmt.Errorf("结构检查失败: %w", err)
	}

	return nil
}

func generateCode(pdb *schema.PDB, outDir, pkgName string) error {
	if outDir == "" {
		outDir = pdb.GenOutput
	}
	if outDir == "" {
		return fmt.Errorf("没有指定输出目录：既没有传 -out，pdb.xml里也没配置genOutput属性")
	}

	files, err := gen.Generate(pdb, pkgName)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("创建输出目录 %s: %w", outDir, err)
	}

	for _, gf := range files {
		path := filepath.Join(outDir, gf.Name)
		if err := os.WriteFile(path, gf.Content, 0o644); err != nil {
			return fmt.Errorf("写入 %s: %w", path, err)
		}
		fmt.Println("  已生成", path)
	}
	return nil
}

func bootstrapTables(ctx context.Context, pool *pgxpool.Pool, pdb *schema.PDB) error {
	results, err := ddl.Bootstrap(ctx, pool, pdb)
	if err != nil {
		return err
	}
	for _, r := range results {
		switch r.Action {
		case ddl.ActionCreated:
			fmt.Printf("  [已创建] %s\n", r.Table)
		case ddl.ActionSkippedExists:
			fmt.Printf("  [跳过] %s 已存在，未做任何改动\n", r.Table)
		}
	}
	return nil
}

func checkSchema(ctx context.Context, pool *pgxpool.Pool, pdb *schema.PDB) error {
	diffs, err := schemacheck.Check(ctx, pool, pdb)
	if err != nil {
		return err
	}
	if len(diffs) == 0 {
		fmt.Println("  数据库结构与 pdb.xml 一致，没有发现差异。")
		return nil
	}
	fmt.Printf("  发现 %d 处差异（以下建议SQL仅供参考，不会被自动执行，请人工确认后手动执行）：\n\n", len(diffs))
	for _, d := range diffs {
		fmt.Println("  " + d.String())
		fmt.Println()
	}
	return nil
}
