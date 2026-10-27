package main

import (
	"strings"
	"testing"

	"pdbgen/readxml"
)

// probeSchema 构造一张 probe 表：主键 id（int64 自增），再加上 vars。
func probeSchema(vars []readxml.Variable, indexes ...readxml.Index) *readxml.Schema {
	return &readxml.Schema{
		Package: "ptable",
		Beans:   []readxml.Bean{{Name: "Probe", Variables: append([]readxml.Variable{{Name: "id", Type: "int64"}}, vars...)}},
		Tables: []readxml.Table{{
			Name:       "probe",
			Bean:       "Probe",
			PrimaryKey: &readxml.PrimaryKey{Variable: "id", AutoIncrement: true},
			Indexes:    indexes,
		}},
	}
}

func generate(t *testing.T, schema *readxml.Schema) string {
	t.Helper()
	out, err := generateTable(schema, schema.Tables[0])
	if err != nil {
		t.Fatalf("generateTable failed: %v", err)
	}
	return string(out)
}

func mustContain(t *testing.T, src string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(src, want) {
			t.Fatalf("generated source missing %q:\n%s", want, src)
		}
	}
}

func TestGenerateTableSupportsBoolAndFloats(t *testing.T) {
	src := generate(t, probeSchema([]readxml.Variable{
		{Name: "flag", Type: "bool"},
		{Name: "ratio", Type: "float32"},
		{Name: "score", Type: "float64"},
	}))
	mustContain(t, src,
		"func (o *Probe) SetFlag(v bool)",
		"func (o *Probe) SetRatio(v float32)",
		"func (o *Probe) SetScore(v float64)",
		`"\"flag\"",`, `"\"ratio\"",`, `"\"score\"",`,
		"o.flag,", "o.ratio,", "o.score,",
	)
}

// TestGenerateTableWrapsSensitiveColumns sensitive 列在 UPDATE、INSERT 和查询参数里都包成 dbctx.Secret。
func TestGenerateTableWrapsSensitiveColumns(t *testing.T) {
	src := generate(t, probeSchema([]readxml.Variable{
		{Name: "name", Type: "string"},
		{Name: "token", Type: "string", Sensitive: true},
	}, readxml.Index{Variable: "token", Unique: true}))

	if n := strings.Count(src, "dbctx.Secret(o.token)"); n != 2 {
		t.Fatalf("FlushStmt 和 Insert 都应该包 dbctx.Secret(o.token)，实际 %d 处:\n%s", n, src)
	}
	mustContain(t, src, "scanProbe, dbctx.Secret(token))")
	if strings.Contains(src, "dbctx.Secret(o.name)") {
		t.Fatal("非 sensitive 列不应包 dbctx.Secret")
	}
}

// TestGenerateTableGuardsPredicateColumns 索引查询前检查条件列，按主键查不检查。
func TestGenerateTableGuardsPredicateColumns(t *testing.T) {
	src := generate(t, probeSchema([]readxml.Variable{
		{Name: "uid", Type: "int64"},
		{Name: "actId", Type: "int32"},
	}, readxml.Index{Variable: "uid, actId", Unique: true}))

	mustContain(t, src, `dbctx.CheckPredicate(ctx, "probe", dirtyProbeUid|dirtyProbeActId)`)
	if n := strings.Count(src, "dbctx.CheckPredicate("); n != 1 {
		t.Fatalf("只有索引查询需要检查条件列，实际 %d 处", n)
	}
}

// TestBuildTableGenRejects 框架不支持的写法在生成阶段直接报错，而不是生成出编不过或跑不对的代码。
func TestBuildTableGenRejects(t *testing.T) {
	str := readxml.Variable{Name: "name", Type: "string"}
	for _, tc := range []struct {
		name   string
		schema *readxml.Schema
		want   string
	}{
		{"int32 主键", func() *readxml.Schema {
			s := probeSchema([]readxml.Variable{str})
			s.Beans[0].Variables[0].Type = "int32"
			return s
		}(), "必须是 int64"},
		{"非自增主键", func() *readxml.Schema {
			s := probeSchema([]readxml.Variable{str})
			s.Tables[0].PrimaryKey.AutoIncrement = false
			return s
		}(), `autoIncrement="true"`},
		{"只有主键", probeSchema(nil), "至少要有一列"},
		{"Go 关键字", probeSchema([]readxml.Variable{{Name: "type", Type: "int32"}}), "Go 关键字"},
		{"内部字段", probeSchema([]readxml.Variable{{Name: "deleted", Type: "bool"}}), "内部名字冲突"},
		{"和 Flusher 方法同名", probeSchema([]readxml.Variable{{Name: "tableName", Type: "string"}}), "内部名字冲突"},
		{"查询方法的参数名", probeSchema([]readxml.Variable{{Name: "ctx", Type: "string"}}), "内部名字冲突"},
		{"大写开头", probeSchema([]readxml.Variable{{Name: "Name", Type: "string"}}), "小写字母开头"},
		{"不支持的类型", probeSchema([]readxml.Variable{{Name: "at", Type: "time.Time"}}), "不支持"},
		{"非 string 的 sensitive", probeSchema([]readxml.Variable{{Name: "pin", Type: "int64", Sensitive: true}}), "不能标记 sensitive"},
		{"重复索引", probeSchema([]readxml.Variable{str},
			readxml.Index{Variable: "name"}, readxml.Index{Variable: "name", Unique: true}), "重复"},
		{"和主键相同的索引", probeSchema([]readxml.Variable{str}, readxml.Index{Variable: "id", Unique: true}), "重复"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildTableGen(tc.schema, tc.schema.Tables[0])
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("期望报错包含 %q，实际: %v", tc.want, err)
			}
		})
	}
}

// TestCheckTablesRejectsSharedBean 一个 bean 只能映射一张表。
func TestCheckTablesRejectsSharedBean(t *testing.T) {
	s := probeSchema([]readxml.Variable{{Name: "name", Type: "string"}})
	s.Tables = append(s.Tables, readxml.Table{Name: "probe2", Bean: "Probe", PrimaryKey: s.Tables[0].PrimaryKey})
	if err := checkTables(s); err == nil || !strings.Contains(err.Error(), "一个 bean 只能映射一张表") {
		t.Fatalf("期望拒绝共用 bean，实际: %v", err)
	}
}
