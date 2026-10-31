package readxml

import (
	"strings"
	"testing"
)

// TestLoadSchemaRepoFile 仓库里的 pdb.xml 能读，值和行号都对。
func TestLoadSchemaRepoFile(t *testing.T) {
	s, err := LoadSchema("../pdb.xml")
	if err != nil {
		t.Fatal(err)
	}
	if s.File != "../pdb.xml" || s.Package != "ptable" || s.PoolMaxConns != 100 || s.LockTimeoutMs != 1000 || !s.ShowSQL {
		t.Fatalf("根元素的属性没读对: %+v", s)
	}
	if len(s.Beans) == 0 || len(s.Tables) == 0 {
		t.Fatalf("没读到 bean / table")
	}
	for _, b := range s.Beans {
		if b.Line == 0 || len(b.Variables) == 0 || b.Variables[0].Line <= b.Line {
			t.Fatalf("bean %s 的行号不对: %+v", b.Name, b)
		}
	}
	for _, tb := range s.Tables {
		if tb.Line == 0 || tb.PrimaryKey == nil || tb.PrimaryKey.Line <= tb.Line {
			t.Fatalf("table %s 的行号不对: %+v", tb.Name, tb)
		}
		if want := map[string]string{"user_mail": "uid"}[tb.Name]; tb.Lock != want {
			t.Fatalf("table %s 的 lock 应该是 %q，实际 %q", tb.Name, want, tb.Lock)
		}
	}
}

const validTable = `
  <bean name="Probe">
    <variable name="id" type="int64"/> 自增
    <variable name="uid" type="int64"/>
  </bean>
  <table name="probe" bean="Probe">
    <primaryKey variable="id" autoIncrement="true" start="1000"/>
    <index variable="uid" unique="true"/>
  </table>
`

func parse(src string) (*Schema, error) {
	return ParseSchema(strings.NewReader(src), "pdb.xml")
}

// TestParseSchemaValid 注释文字、XML 注释都不影响；值的两边可以有空白。
func TestParseSchemaValid(t *testing.T) {
	s, err := parse(`<?xml version="1.0" encoding="UTF-8"?>
<!-- 测试用 -->
<pdb url="postgres://x" poolMaxConns=" 8 " showSQL="true">` + validTable + `</pdb>`)
	if err != nil {
		t.Fatal(err)
	}
	pk := s.Tables[0].PrimaryKey
	if s.PoolMaxConns != 8 || !s.ShowSQL || pk.Variable != "id" || !pk.AutoIncrement || pk.Start != 1000 ||
		!s.Tables[0].Indexes[0].Unique || s.Beans[0].Line != 4 || s.Beans[0].Variables[0].Line != 5 {
		t.Fatalf("没读对: %+v / %+v / %+v", s, s.Tables[0], s.Beans[0])
	}
}

// TestParseSchemaRejects 拼错、写错的地方都报错，带行号，不再被悄悄忽略。
func TestParseSchemaRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want []string
	}{
		{"unique 拼错", `<pdb>
  <bean name="Probe"><variable name="id" type="int64"/></bean>
  <table name="probe" bean="Probe">
    <primaryKey variable="id" autoIncrement="true"/>
    <index variable="id" uniqe="true"/>
  </table>
</pdb>`, []string{"pdb.xml:5: <index> 不认识的属性 uniqe，是不是 unique？"}},
		{"根元素的属性拼错", `<pdb poolMaxConn="3" statementTimeoutMS="10">` + validTable + `</pdb>`, []string{
			"pdb.xml:1: <pdb> 不认识的属性 poolMaxConn，是不是 poolMaxConns？",
			"<pdb> 不认识的属性 statementTimeoutMS，是不是 statementTimeoutMs？",
		}},
		{"完全不认识的属性", `<pdb sensitive="true">` + validTable + `</pdb>`, []string{"不认识的属性 sensitive"}},
		{"元素名写错", `<pdb>` + validTable + `<table name="t" bean="Probe"><indexes variable="uid"/></table></pdb>`,
			[]string{"<table> 里不认识的元素 <indexes>"}},
		{"pdb 下不认识的元素", `<pdb><beans/></pdb>`, []string{"pdb.xml:1: <pdb> 里不认识的元素 <beans>"}},
		{"整数写错", `<pdb poolMaxConns="abc">` + validTable + `</pdb>`, []string{`<pdb> 的属性 poolMaxConns="abc" 不是合法的整数`}},
		{"整数越界", `<pdb poolMaxConns="3000000000">` + validTable + `</pdb>`, []string{"poolMaxConns"}},
		{"布尔值写错", `<pdb showSQL="yes">` + validTable + `</pdb>`, []string{`showSQL="yes" 不是合法的布尔值`}},
		{"缺少属性", `<pdb><bean name="Probe"><variable name="id"/></bean></pdb>`, []string{"<variable> 缺少属性 type"}},
		{"属性为空", `<pdb><bean name=""/></pdb>`, []string{"<bean> 缺少属性 name"}},
		{"两个主键", `<pdb><table name="t" bean="B"><primaryKey variable="id"/>
<primaryKey variable="id"/></table></pdb>`, []string{`pdb.xml:2: <table name="t"> 有多个 <primaryKey>`}},
		{"叶子元素有子元素", `<pdb><bean name="B"><variable name="id" type="int64"><x/></variable></bean></pdb>`,
			[]string{"<variable> 里不能有子元素 <x>"}},
		{"根元素不对", `<xdb/>`, []string{"根元素应该是 <pdb>，实际是 <xdb>"}},
		{"XML 语法错误", "<pdb>\n<bean name=\"B\">\n</pdb>", []string{"pdb.xml", "line 3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(tc.src)
			if err == nil {
				t.Fatal("应该报错")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("错误里应该有 %q，实际:\n%v", want, err)
				}
			}
		})
	}
}

// TestParseSchemaReportsAllErrors 一次把所有错误都报出来，按出现的顺序。
func TestParseSchemaReportsAllErrors(t *testing.T) {
	_, err := parse(`<pdb showSQL="yes">
  <bean name="B">
    <variable name="id" typ="int64"/>
  </bean>
  <table name="t" bean="B" lock2="uid"/>
</pdb>`)
	if err == nil {
		t.Fatal("应该报错")
	}
	lines := strings.Split(err.Error(), "\n")
	want := []string{"pdb.xml:1:", "pdb.xml:3:", "pdb.xml:3:", "pdb.xml:5:"}
	if len(lines) != len(want) {
		t.Fatalf("应该报 %d 个错误，实际:\n%v", len(want), err)
	}
	for i, prefix := range want {
		if !strings.HasPrefix(lines[i], prefix) {
			t.Fatalf("第 %d 个错误应该以 %s 开头，实际:\n%v", i+1, prefix, err)
		}
	}
}

// TestParseSchemaLineOfMultilineTag 开始标签跨好几行时，报的是标签的第一行。
func TestParseSchemaLineOfMultilineTag(t *testing.T) {
	_, err := parse(`<?xml version="1.0"?>
<pdb url="postgres://x"
     genOutput="./ptable"
     poolMaxConn="3">
</pdb>`)
	if err == nil || !strings.HasPrefix(err.Error(), "pdb.xml:2: ") {
		t.Fatalf("应该报在第 2 行（<pdb 所在的行），实际: %v", err)
	}
}

func TestDistance(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"", "", 0}, {"unique", "unique", 0}, {"uniqe", "unique", 1}, {"poolMaxConn", "poolMaxConns", 1},
		{"abc", "", 3}, {"kitten", "sitting", 3},
	} {
		if got := distance(tc.a, tc.b); got != tc.want {
			t.Errorf("distance(%q, %q) = %d，期望 %d", tc.a, tc.b, got, tc.want)
		}
	}
}
