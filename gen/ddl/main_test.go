package main

import (
	"strings"
	"testing"

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
