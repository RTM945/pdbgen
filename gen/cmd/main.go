// pdbgen命令行工具：读取 pdb.xml，生成对应的Go持久化代码。
//
// 用法：
//
//	go run ./gen/cmd -xml pdb.xml -out ./model -pkg model
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"pdbgen/gen"
	"pdbgen/schema"
)

func main() {
	xmlPath := flag.String("xml", "pdb.xml", "pdb.xml文件路径")
	outDir := flag.String("out", "", "生成代码的输出目录；不填则使用pdb.xml里的genOutput属性")
	pkgName := flag.String("pkg", "model", "生成代码的package名")
	flag.Parse()

	if err := run(*xmlPath, *outDir, *pkgName); err != nil {
		fmt.Fprintln(os.Stderr, "生成失败:", err)
		os.Exit(1)
	}
}

func run(xmlPath, outDir, pkgName string) error {
	f, err := os.Open(xmlPath)
	if err != nil {
		return fmt.Errorf("打开 %s: %w", xmlPath, err)
	}
	defer f.Close()

	pdb, err := schema.Parse(f)
	if err != nil {
		return err
	}

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
		fmt.Println("已生成", path)
	}
	return nil
}
