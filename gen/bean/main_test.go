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
		Schema:  "public",
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

// TestGenerateTablePartialIndex 部分索引：常量写进 SQL，按 method 命名，条件列也进 CheckPredicate。
func TestGenerateTablePartialIndex(t *testing.T) {
	src := generate(t, probeSchema([]readxml.Variable{
		{Name: "uid", Type: "int64"},
		{Name: "workerEnabled", Type: "bool"},
		{Name: "state", Type: "int32"},
	},
		readxml.Index{Where: "workerEnabled=true", Method: "WorkerEnabled"},
		readxml.Index{Variable: "uid", Where: "state=-1, workerEnabled=false", Method: "FailedByUid", Unique: true},
	))
	mustContain(t, src,
		"func (t probe) ListWorkerEnabled(ctx context.Context) []*Probe {",
		"func (t probe) SelectListWorkerEnabled(ctx context.Context) []*Probe {",
		"return t.queryWhereWorkerEnabled(ctx, dbctx.QuerierFromCtx(ctx))",
		`WHERE \"worker_enabled\""`,
		"dbctx.QueryList(ctx, q, query, scanProbe)\n",
		`dbctx.CheckPredicate(ctx, "probe", dirtyProbeWorkerEnabled)`,
		"func (t probe) GetFailedByUid(ctx context.Context, uid int64) *Probe {",
		"func (t probe) SelectFailedByUid(ctx context.Context, uid int64) *Probe {",
		`WHERE \"uid\" = $1 AND \"state\" = -1 AND NOT \"worker_enabled\""`,
		`dbctx.CheckPredicate(ctx, "probe", dirtyProbeUid|dirtyProbeState|dirtyProbeWorkerEnabled)`,
	)
}

// TestBuildTableGenRejects 框架不支持的写法在生成阶段直接报错，而不是生成出编不过或跑不对的代码。
func TestBuildTableGenRejects(t *testing.T) {
	str := readxml.Variable{Name: "name", Type: "string"}
	flag := readxml.Variable{Name: "on", Type: "bool"}
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
		{"重复索引", probeSchema([]readxml.Variable{str},
			readxml.Index{Variable: "name"}, readxml.Index{Variable: "name", Unique: true}), "重复"},
		{"和主键相同的索引", probeSchema([]readxml.Variable{str}, readxml.Index{Variable: "id", Unique: true}), "重复"},
		{"部分索引没有 method", probeSchema([]readxml.Variable{flag}, readxml.Index{Where: "on=true"}), "method"},
		{"method 小写", probeSchema([]readxml.Variable{flag}, readxml.Index{Where: "on=true", Method: "on"}), "method"},
		{"普通索引带 method", probeSchema([]readxml.Variable{str}, readxml.Index{Variable: "name", Method: "Name"}), "要配 where"},
		{"没有 variable 的 unique 部分索引", probeSchema([]readxml.Variable{flag},
			readxml.Index{Where: "on=true", Method: "On", Unique: true}), "不能 unique"},
		{"条件用了主键", probeSchema([]readxml.Variable{flag}, readxml.Index{Where: "id=1", Method: "One"}), "主键"},
		{"条件列不存在", probeSchema([]readxml.Variable{flag}, readxml.Index{Where: "off=true", Method: "Off"}), "不在 bean 里"},
		{"条件列是字符串", probeSchema([]readxml.Variable{str}, readxml.Index{Where: "name=a", Method: "A"}), "只支持 bool 和整数"},
		{"bool 常量写错", probeSchema([]readxml.Variable{flag}, readxml.Index{Where: "on=1", Method: "On"}), "true 或 false"},
		{"整数越界", probeSchema([]readxml.Variable{{Name: "kind", Type: "int8"}},
			readxml.Index{Where: "kind=300", Method: "Big"}), "范围内的整数"},
		{"条件重复", probeSchema([]readxml.Variable{flag}, readxml.Index{Where: "on=true, on=false", Method: "On"}), "重复"},
		{"相同的部分索引", probeSchema([]readxml.Variable{flag},
			readxml.Index{Where: "on=true", Method: "On"}, readxml.Index{Where: "on = true", Method: "On2"}), "重复"},
		{"部分索引方法名重复", probeSchema([]readxml.Variable{flag},
			readxml.Index{Where: "on=true", Method: "On"}, readxml.Index{Where: "on=false", Method: "On"}), "方法名"},
		{"部分索引方法名和普通索引重复", probeSchema([]readxml.Variable{str, flag},
			readxml.Index{Variable: "name"}, readxml.Index{Where: "on=true", Method: "ByName"}), "方法名"},
		{"lock 列不存在", withLock(probeSchema([]readxml.Variable{str}), "uid"), "不在 bean 里"},
		{"lock 列不是 int64", withLock(probeSchema([]readxml.Variable{str}), "name"), "必须是 int64"},
		{"和 foreign 字段同名", probeSchema([]readxml.Variable{{Name: "foreign", Type: "bool"}}), "内部名字冲突"},
		{"和 LockKey 方法同名", probeSchema([]readxml.Variable{{Name: "lockKey", Type: "int64"}}), "内部名字冲突"},
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
	if errs := checkTables(s); len(errs) != 1 || !strings.Contains(errs[0], "一个 bean 只能映射一张表") {
		t.Fatalf("期望拒绝共用 bean，实际: %v", errs)
	}
}

func withLock(s *readxml.Schema, lock string) *readxml.Schema {
	s.Tables[0].Lock = lock
	return s
}

// TestGenerateTableLock 声明了 lock 的表：实体实现 dbctx.Owned，SetXxx / Delete 先查 foreign；没声明的表不生成这些。
func TestGenerateTableLock(t *testing.T) {
	vars := []readxml.Variable{{Name: "uid", Type: "int64"}, {Name: "state", Type: "int32"}}
	src := generate(t, withLock(probeSchema(vars), "uid"))
	mustContain(t, src,
		"var _ dbctx.Owned = (*Probe)(nil)",
		`func (o *Probe) LockKey() (string, int64) { return "uid", o.uid }`,
		"func (o *Probe) MarkForeign() { o.foreign = true }",
	)
	// 每个 setter 和 Delete 都要检查
	if n, want := strings.Count(src, "if o.foreign {\n\t\tdbctx.PanicForeign(o)"), len(vars)+1; n != want {
		t.Fatalf("SetXxx 和 Delete 都要检查 foreign，期望 %d 处，实际 %d 处:\n%s", want, n, src)
	}

	plain := generate(t, probeSchema(vars))
	if strings.Contains(plain, "foreign") || strings.Contains(plain, "LockKey") {
		t.Fatalf("没声明 lock 的表不应该生成归属检查:\n%s", plain)
	}
}
