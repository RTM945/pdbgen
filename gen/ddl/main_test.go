package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"pdbgen/readxml"
)

func TestSQLType(t *testing.T) {
	cases := []struct {
		goType     string
		columnType string
		defaultVal string
	}{
		{"int64", "BIGINT", "0"},
		{"int32", "INTEGER", "0"},
		{"int16", "SMALLINT", "0"},
		{"int8", "SMALLINT", "0"},
		{"string", "TEXT", "''"},
		{"bool", "BOOLEAN", "false"},
		{"float32", "REAL", "0"},
		{"float64", "DOUBLE PRECISION", "0"},
	}

	for _, c := range cases {
		columnType, defaultVal, err := sqlType(c.goType)
		if err != nil {
			t.Fatalf("sqlType(%q) failed: %v", c.goType, err)
		}
		if columnType != c.columnType || defaultVal != c.defaultVal {
			t.Fatalf(
				"sqlType(%q) = (%q, %q), want (%q, %q)",
				c.goType, columnType, defaultVal, c.columnType, c.defaultVal,
			)
		}
	}

	if _, _, err := sqlType("decimal"); err == nil {
		t.Fatal("unsupported type should return an error")
	}
}

func TestIdentityOptions(t *testing.T) {
	cases := []struct {
		start   int64
		step    int64
		want    string
		wantErr bool
	}{
		{0, 0, "", false},
		{1000, 0, " (START WITH 1000)", false},
		{0, 10, " (INCREMENT BY 10)", false},
		{1000, 10, " (START WITH 1000 INCREMENT BY 10)", false},
		{1000, 1, " (START WITH 1000)", false},
		{0, 1, "", false},
		{0, -1, "", true},
	}

	for _, c := range cases {
		got, err := identityOptions(c.start, c.step)
		if c.wantErr {
			if err == nil {
				t.Fatalf("start=%d step=%d should return an error", c.start, c.step)
			}
			continue
		}
		if err != nil {
			t.Fatalf("start=%d step=%d failed: %v", c.start, c.step, err)
		}
		if got != c.want {
			t.Fatalf("start=%d step=%d = %q, want %q", c.start, c.step, got, c.want)
		}
	}
}

func partialSchema(indexes ...readxml.Index) *readxml.Schema {
	return &readxml.Schema{
		Schema: "public",
		Beans: []readxml.Bean{{Name: "Probe", Variables: []readxml.Variable{
			{Name: "id", Type: "int64"}, {Name: "uid", Type: "int64"},
			{Name: "workerEnabled", Type: "bool"}, {Name: "state", Type: "int32"},
		}}},
		Tables: []readxml.Table{{
			Name: "probe", Bean: "Probe",
			PrimaryKey: &readxml.PrimaryKey{Variable: "id", AutoIncrement: true},
			Indexes:    indexes,
		}},
	}
}

func TestGenerateIndexDDLPartial(t *testing.T) {
	cases := []struct {
		index readxml.Index
		want  string
	}{
		{readxml.Index{Variable: "uid"},
			`CREATE INDEX IF NOT EXISTS "idx_probe_uid" ON "public"."probe" ("uid");`},
		{readxml.Index{Where: "workerEnabled=true", Method: "WorkerEnabled"},
			`CREATE INDEX IF NOT EXISTS "idx_probe_where_worker_enabled" ON "public"."probe" ("id") WHERE "worker_enabled";`},
		{readxml.Index{Variable: "uid", Unique: true, Where: "state=-1, workerEnabled=false", Method: "FailedByUid"},
			`CREATE UNIQUE INDEX IF NOT EXISTS "idx_probe_where_failed_by_uid" ON "public"."probe" ("uid") WHERE "state" = -1 AND NOT "worker_enabled";`},
	}
	for _, c := range cases {
		s := partialSchema(c.index)
		var b strings.Builder
		if err := generateIndexDDL(&b, s, s.Tables[0], c.index); err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(b.String()); got != c.want {
			t.Errorf("got  %s\nwant %s", got, c.want)
		}
	}

	s := partialSchema()
	if err := generateIndexDDL(new(strings.Builder), s, s.Tables[0], readxml.Index{Where: "state=0"}); err == nil {
		t.Error("部分索引没有 method 应该报错")
	}
}

// TestNormalizePredicate 右边是本地 PostgreSQL 的 pg_get_expr(indpred) 实测结果。
func TestNormalizePredicate(t *testing.T) {
	cases := []struct{ ours, pg string }{
		{`"worker_enabled"`, `worker_enabled`},
		{`NOT "worker_enabled"`, `(NOT worker_enabled)`},
		{`"state" = 0`, `(state = 0)`},
		{`"state" = -1`, `(state = '-1'::integer)`},
		{`"big" = 5000000000`, `(big = '5000000000'::bigint)`},
		{`"state" = 0 AND "kind" = 2`, `((state = 0) AND (kind = 2))`},
		{`"worker_enabled" AND "state" = 1`, `(worker_enabled AND (state = 1))`},
		{``, ``},
	}
	for _, c := range cases {
		if a, b := normalizePredicate(c.ours), normalizePredicate(c.pg); a != b {
			t.Errorf("%q → %q, %q → %q", c.ours, a, c.pg, b)
		}
	}
	if normalizePredicate(`"state" = 0`) == normalizePredicate(`(state = 1)`) {
		t.Error("不同的条件不能相等")
	}
}

// TestCheckNames 名字超过 63 字节、在 schema 里重名都报错；可以用 name 属性起一个短的。
func TestCheckNames(t *testing.T) {
	long := readxml.Index{Variable: "uid", Line: 9}
	s := &readxml.Schema{
		File: "pdb.xml", Schema: "public",
		Beans: []readxml.Bean{{Name: "Probe", Variables: []readxml.Variable{{Name: "id", Type: "int64"}, {Name: "uid", Type: "int64"}}}},
		Tables: []readxml.Table{{
			Name: "user_activity_reward_progress_for_the_spring_festival_event", Bean: "Probe", Line: 7,
			PrimaryKey: &readxml.PrimaryKey{Variable: "id", AutoIncrement: true, Line: 8},
			Indexes:    []readxml.Index{long},
		}},
	}
	err := checkNames(s)
	if err == nil {
		t.Fatal("应该报错")
	}
	for _, want := range []string{
		`pdb.xml:8: 主键名 "pk_user_activity_reward_progress_for_the_spring_festival_event_id" 有 65 字节`,
		`<primaryKey name="...">`,
		`pdb.xml:9: 索引名`, `<index name="...">`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误里应该有 %q，实际:\n%v", want, err)
		}
	}

	s.Tables[0].PrimaryKey.Name = "pk_reward_progress"
	s.Tables[0].Indexes[0].Name = "idx_reward_progress_uid"
	if err := checkNames(s); err != nil {
		t.Fatalf("起了短名字之后应该通过: %v", err)
	}

	s.Tables[0].Indexes[0].Name = s.Tables[0].Name
	if err := checkNames(s); err == nil || !strings.Contains(err.Error(), "重名") {
		t.Fatalf("索引和表重名应该报错，实际: %v", err)
	}
}

// TestGenerateAgainstDatabase 在真实 PG 的临时 schema 里跑一遍完整的流程：建表、再跑一遍什么都不用做、
// 加列加索引、定义对不上时报错。PDBGEN_REQUIRE_DB=1 时连不上库直接失败。
func TestGenerateAgainstDatabase(t *testing.T) {
	base, err := readxml.LoadSchema("../../pdb.xml")
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	conn, err := connect(ctx, base.URL)
	if err != nil {
		if os.Getenv("PDBGEN_REQUIRE_DB") != "" {
			t.Fatalf("连不上 pdb.xml 里的数据库: %v", err)
		}
		t.Skipf("跳过集成测试，连不上 pdb.xml 里的数据库: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) }) // 先注册的后执行：删完临时 schema 再关连接

	schemaName := fmt.Sprintf("pdbgen_ddl_test_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		if _, err := conn.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`); err != nil {
			t.Errorf("删除临时 schema %s 失败: %v", schemaName, err)
		}
	})

	parse := func(state, unique, extra string) *readxml.Schema {
		t.Helper()
		s, err := readxml.ParseSchema(strings.NewReader(`<pdb schema="`+schemaName+`">
  <bean name="Probe">
    <variable name="id" type="int64"/>
    <variable name="uid" type="int64"/>
    <variable name="state" type="`+state+`"/>`+extra+`
  </bean>
  <table name="probe" bean="Probe">
    <primaryKey variable="id" autoIncrement="true" start="1000"/>
    <index variable="uid" unique="`+unique+`"/>
  </table>
</pdb>`), "pdb.xml")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	gen := func(s *readxml.Schema) string {
		t.Helper()
		ddl, err := generate(ctx, s, conn)
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if !strings.Contains(ddl, "BEGIN;") || !strings.HasSuffix(ddl, "COMMIT;\n") {
			t.Fatalf("DDL 应该包在事务里:\n%s", ddl)
		}
		return ddl
	}
	apply := func(s *readxml.Schema) string {
		t.Helper()
		ddl := gen(s)
		if _, err := conn.Exec(ctx, ddl); err != nil {
			t.Fatalf("执行生成的 DDL 失败: %v\n%s", err, ddl)
		}
		return ddl
	}
	nothingToDo := func(s *readxml.Schema) {
		t.Helper()
		ddl := gen(s)
		for _, stmt := range []string{"CREATE TABLE", "ALTER TABLE", "CREATE INDEX", "CREATE UNIQUE INDEX"} {
			if strings.Contains(ddl, stmt) {
				t.Fatalf("库里已经是 pdb.xml 的样子，不应该再生成 %s:\n%s", stmt, ddl)
			}
		}
	}
	mustContain := func(ddl string, wants ...string) {
		t.Helper()
		for _, want := range wants {
			if !strings.Contains(ddl, want) {
				t.Fatalf("DDL 里应该有 %q:\n%s", want, ddl)
			}
		}
	}

	v1 := parse("int32", "true", "")
	mustContain(apply(v1),
		`CREATE TABLE IF NOT EXISTS "`+schemaName+`"."probe"`,
		`ADD GENERATED ALWAYS AS IDENTITY (START WITH 1000)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS "idx_probe_uid"`,
	)
	nothingToDo(v1)

	var id int64
	if err := conn.QueryRow(ctx, `INSERT INTO "`+schemaName+`"."probe" ("uid", "state") VALUES (1, 0) RETURNING "id"`).Scan(&id); err != nil || id != 1000 {
		t.Fatalf("identity 应该从 1000 开始，实际 id=%d err=%v", id, err)
	}

	// 加一列、一个部分索引
	v2 := parse("int32", "true", `
    <variable name="flag" type="bool"/>`)
	v2.Tables[0].Indexes = append(v2.Tables[0].Indexes, readxml.Index{Where: "flag=true", Method: "Flagged", Line: 11})
	mustContain(apply(v2),
		`ADD COLUMN "flag" BOOLEAN NOT NULL DEFAULT false`,
		`CREATE INDEX IF NOT EXISTS "idx_probe_where_flagged" ON "`+schemaName+`"."probe" ("id") WHERE "flag"`,
	)
	nothingToDo(v2)

	// 已有的列和索引只校验：对不上就报错（带 pdb.xml 的行号），不改表
	if _, err := generate(ctx, parse("int64", "true", ""), conn); err == nil || !strings.Contains(err.Error(), "pdb.xml:5: table probe column state type mismatch") {
		t.Fatalf("列的类型对不上应该报错，实际: %v", err)
	}
	if _, err := generate(ctx, parse("int32", "false", ""), conn); err == nil || !strings.Contains(err.Error(), "index idx_probe_uid definition mismatch") {
		t.Fatalf("索引定义对不上应该报错，实际: %v", err)
	}
}
