package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pdbgen/readxml"
)

func parseSchema(t *testing.T, src string) *readxml.Schema {
	t.Helper()
	s, err := readxml.ParseSchema(strings.NewReader(src), "pdb.xml")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestPtableIsUpToDate 仓库里的 ptable 是由 pdb.xml 生成的最新结果，也没有残留的旧生成文件。
// 改了 pdb.xml、模板或者生成器，都要重新生成：go run ./gen/bean -schema ./pdb.xml
func TestPtableIsUpToDate(t *testing.T) {
	const schemaFile = "../../pdb.xml"
	schema, err := readxml.LoadSchema(schemaFile)
	if err != nil {
		t.Fatal(err)
	}
	files, err := generateAll(schema)
	if err != nil {
		t.Fatal(err)
	}
	dir := outputDir(schemaFile, schema.GenOutput)
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s 不是最新的（err=%v），重新生成：go run ./gen/bean -schema ./pdb.xml", name, err)
		}
	}
	stale, err := staleFiles(dir, files)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) > 0 {
		t.Errorf("ptable 里有不再需要的生成文件 %v，重新生成会删掉它们", stale)
	}
}

// TestGenerateAllReportsLines 所有表的问题一次报出来，每条都带 pdb.xml 的行号。
func TestGenerateAllReportsLines(t *testing.T) {
	_, err := generateAll(parseSchema(t, `<pdb>
  <bean name="A">
    <variable name="id" type="int64"/>
    <variable name="type" type="int32"/>
  </bean>
  <table name="a" bean="A">
    <primaryKey variable="id" autoIncrement="true"/>
    <index variable="missing"/>
  </table>
  <table name="Bad" bean="A">
    <primaryKey variable="id" autoIncrement="true"/>
  </table>
  <table name="item_linux" bean="Nope"/>
</pdb>`))
	if err == nil {
		t.Fatal("应该报错")
	}
	for _, want := range []string{
		`pdb.xml:10: table "Bad": 表名只能用小写字母`,
		`pdb.xml:13: table "item_linux": 生成的文件 item_linux.pdb.go 会被 Go 当成只在部分平台编译的文件`,
		`pdb.xml:4: table "a": 列名 "type" 是 Go 关键字`,
		`pdb.xml:8: table "a": 索引 (missing) 里的列 "missing" 不在 bean 里`,
		`pdb.xml:13: table "item_linux": bean "Nope" 没有定义`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误里应该有 %q，实际:\n%v", want, err)
		}
	}
}

// TestGenerateAllRejectsCollidingNames 不同的表生成同一个包级名字：生成的包编不过，生成时就报出来。
func TestGenerateAllRejectsCollidingNames(t *testing.T) {
	_, err := generateAll(parseSchema(t, `<pdb>
  <bean name="Mail"><variable name="id" type="int64"/><variable name="boxIdx" type="int32"/></bean>
  <bean name="MailBox"><variable name="id" type="int64"/><variable name="idx" type="int32"/></bean>
  <bean name="Other"><variable name="id" type="int64"/><variable name="x" type="int32"/></bean>
  <table name="user_mail" bean="Mail"><primaryKey variable="id" autoIncrement="true"/></table>
  <table name="mail_box" bean="MailBox"><primaryKey variable="id" autoIncrement="true"/></table>
  <table name="user__mail" bean="Other"><primaryKey variable="id" autoIncrement="true"/></table>
</pdb>`))
	if err == nil {
		t.Fatal("应该报错")
	}
	for _, want := range []string{"都生成了 dirtyMailBoxIdx", "都生成了 UserMailTable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误里应该有 %q，实际:\n%v", want, err)
		}
	}
}

// TestFileNamesBuildEverywhere 生成的文件名不能带平台后缀；_test 结尾的表名不会变成测试文件。
func TestFileNamesBuildEverywhere(t *testing.T) {
	for name, want := range map[string]bool{
		"user_mail.pdb.go":    true,
		"ab_test.pdb.go":      true, // 以前生成 ab_test.go，被当成测试文件，整张表从构建里消失
		"item_linux.pdb.go":   false,
		"item_windows.pdb.go": false,
		"item_amd64.pdb.go":   false,
		"item_js_wasm.pdb.go": false,
		"_hidden.pdb.go":      false,
		"linux_item.pdb.go":   true,
		"item_linuxes.pdb.go": true,
	} {
		if got := buildsEverywhere(name); got != want {
			t.Errorf("buildsEverywhere(%q) = %v，期望 %v", name, got, want)
		}
	}
	if strings.HasSuffix(fileName("ab_test"), "_test.go") {
		t.Fatal("_test 结尾的表名不能生成测试文件")
	}
}

// TestWriteFiles 内容没变的不重写；不再需要的生成文件删掉（包括以前的 <表名>.go），手写的文件不碰。
func TestWriteFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gen := header + "\n\npackage ptable\n"
	write("user.go", gen)                       // 旧的文件名
	write("dropped.pdb.go", gen)                // 表已经从 pdb.xml 里删了
	write("helper.go", "package ptable\n")      // 手写的
	write("helper_test.go", "package ptable\n") // 手写的测试
	write("kept.pdb.go", gen+"// same\n")       // 内容没变
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "kept.pdb.go"), old, old); err != nil {
		t.Fatal(err)
	}

	var log bytes.Buffer
	files := map[string][]byte{"user.pdb.go": []byte(gen), "kept.pdb.go": []byte(gen + "// same\n")}
	if err := writeFiles(dir, files, &log); err != nil {
		t.Fatal(err)
	}

	for name, exists := range map[string]bool{
		"user.pdb.go": true, "kept.pdb.go": true, "helper.go": true, "helper_test.go": true,
		"user.go": false, "dropped.pdb.go": false,
	} {
		_, err := os.Stat(filepath.Join(dir, name))
		if exists != (err == nil) {
			t.Errorf("%s: 期望存在=%v，实际 err=%v", name, exists, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, "kept.pdb.go")); err != nil || !fi.ModTime().Equal(old) {
		t.Errorf("内容没变的文件不应该重写")
	}
	if got := log.String(); strings.Contains(got, "kept.pdb.go") || !strings.Contains(got, "removed") {
		t.Errorf("日志不对:\n%s", got)
	}
}

func TestOutputDir(t *testing.T) {
	if got, want := outputDir(filepath.Join("app", "test", "pdb.xml"), "./ptable"), filepath.Join("app", "test", "ptable"); got != want {
		t.Errorf("相对路径应该相对 pdb.xml 所在的目录：期望 %s，实际 %s", want, got)
	}
	abs, _ := filepath.Abs("out")
	if got := outputDir("pdb.xml", abs); got != abs {
		t.Errorf("绝对路径原样用：期望 %s，实际 %s", abs, got)
	}
}

// TestGeneratedImportsOwnModule 生成代码导入的 dbctx 和生成器在同一个模块里。
func TestGeneratedImportsOwnModule(t *testing.T) {
	if dbctxImport != "pdbgen/dbctx" {
		t.Fatalf("dbctx 的导入路径应该是 pdbgen/dbctx，实际 %s", dbctxImport)
	}
	src := generate(t, probeSchema([]readxml.Variable{{Name: "name", Type: "string"}}))
	mustContain(t, src, header+"\n", `"pdbgen/dbctx"`, `FROM \"public\".\"probe\"`)
}

// TestRunWritesNextToSchema genOutput 相对 pdb.xml 所在的目录：从别的目录运行，生成的文件也在 pdb.xml 旁边；
// 再跑一次，内容没变，什么都不写。
func TestRunWritesNextToSchema(t *testing.T) {
	dir := t.TempDir()
	schemaFile := filepath.Join(dir, "pdb.xml")
	if err := os.WriteFile(schemaFile, []byte(`<pdb genOutput="./out">
  <bean name="Probe"><variable name="id" type="int64"/><variable name="uid" type="int64"/></bean>
  <table name="probe" bean="Probe"><primaryKey variable="id" autoIncrement="true"/></table>
</pdb>`), 0o644); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	if err := run(schemaFile, &log); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "out", "probe.pdb.go")); err != nil {
		t.Fatalf("应该生成在 pdb.xml 旁边的 out 目录里: %v（日志：%s）", err, log.String())
	}
	log.Reset()
	if err := run(schemaFile, &log); err != nil {
		t.Fatal(err)
	}
	if log.Len() != 0 {
		t.Fatalf("内容没变，第二次不应该写任何文件，实际:\n%s", log.String())
	}
}
