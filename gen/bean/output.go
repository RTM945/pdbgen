package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"pdbgen/readxml"
)

// writeFiles 把生成的文件写进 dir：内容没变的不写（不动修改时间，也不触发重新编译），
// 不再需要的生成文件删掉——表从 pdb.xml 里删了，或者文件名的规则变了（以前是 <表名>.go）。
// 只删第一行是 header 的 .go 文件，手写的文件不碰。
func writeFiles(dir string, files map[string][]byte, log io.Writer) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		path := filepath.Join(dir, name)
		if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, files[name]) {
			continue
		}
		if err := os.WriteFile(path, files[name], 0o644); err != nil {
			return err
		}
		fmt.Fprintf(log, "generated %s\n", path)
	}
	stale, err := staleFiles(dir, files)
	if err != nil {
		return err
	}
	for _, path := range stale {
		if err := os.Remove(path); err != nil {
			return err
		}
		fmt.Fprintf(log, "removed %s\n", path)
	}
	return nil
}

// staleFiles 返回 dir 里不在 files 里的生成文件。
func staleFiles(dir string, files map[string][]byte) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var stale []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || files[e.Name()] != nil {
			continue
		}
		path := filepath.Join(dir, e.Name())
		generated, err := isGenerated(path)
		if err != nil {
			return nil, err
		}
		if generated {
			stale = append(stale, path)
		}
	}
	return stale, nil
}

// isGenerated 报告 path 是不是 genbeans 生成的文件：第一行是 header。
func isGenerated(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return strings.TrimRight(line, "\r\n") == header, nil
}

// buildsEverywhere 报告文件名 name 会不会在所有平台都编译。Go 按文件名排除文件：_linux、_amd64 这类后缀
// 只在对应的平台编译，_ 或 . 开头的文件被忽略。直接用 go/build 的规则判断：在 GOOS、GOARCH 都不同的
// 两个平台上都要能编译，就不会有任何平台后缀。
func buildsEverywhere(name string) bool {
	for _, p := range [][2]string{{"linux", "amd64"}, {"windows", "arm64"}} {
		ctx := build.Default
		ctx.GOOS, ctx.GOARCH = p[0], p[1]
		ctx.OpenFile = func(string) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("package p\n")), nil
		}
		if ok, err := ctx.MatchFile(".", name); err != nil || !ok {
			return false
		}
	}
	return true
}

// checkDeclarations 检查各张表生成的包级名字有没有撞上：user_mail 和 userMail 都生成 UserMailTable，
// bean Mail 的 boxIdx 和 bean MailBox 的 idx 都生成 dirtyMailBoxIdx。撞上了生成的包编不过，
// 所以在生成的时候就报出来，指到 pdb.xml 的行。名字直接从生成的代码里取，不会和模板对不上。
func checkDeclarations(schema *readxml.Schema, files map[string][]byte, tables map[string]readxml.Table) []string {
	var errs []string
	owner := make(map[string]string) // 名字 → 文件
	for _, name := range slices.Sorted(maps.Keys(files)) {
		f, err := parser.ParseFile(token.NewFileSet(), name, files[name], parser.SkipObjectResolution)
		if err != nil {
			errs = append(errs, fmt.Sprintf("解析生成的 %s 失败（生成器的 bug）: %v", name, err))
			continue
		}
		for _, id := range topLevelNames(f) {
			other, ok := owner[id]
			if !ok {
				owner[id] = name
				continue
			}
			a, b := tables[other], tables[name]
			errs = append(errs, at(schema, b.Line)+fmt.Sprintf(
				"table %q 和 table %q（第 %d 行）都生成了 %s，生成的包编不过：换一个表名、bean 名或列名", b.Name, a.Name, a.Line, id))
		}
	}
	return errs
}

// topLevelNames 返回文件里的包级名字：类型、变量、常量和函数（不含方法和 _）。
func topLevelNames(f *ast.File) []string {
	var names []string
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil {
				names = append(names, d.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					names = append(names, s.Name.Name)
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if n.Name != "_" {
							names = append(names, n.Name)
						}
					}
				}
			}
		}
	}
	return names
}
