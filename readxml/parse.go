package readxml

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
)

// LoadSchema 读 pdb.xml。不认识的元素和属性、写不对的值都报错，带上 文件:行号，一次全部报出来：
// 拼错的属性如果被忽略（比如把 unique 写成 uniqe），生成的代码和 DDL 会悄悄地变成另一个样子。
// 元素后面的文字（<variable .../> 自增）是注释，不检查。
func LoadSchema(filename string) (*Schema, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseSchema(f, filename)
}

// ParseSchema 同 LoadSchema，从 r 读；file 只用在错误信息里。
func ParseSchema(r io.Reader, file string) (*Schema, error) {
	root, err := parseXML(r, file)
	if err != nil {
		return nil, err
	}
	p := &parser{file: file}
	s := p.schema(root)
	if len(p.errs) > 0 {
		return nil, errors.New(strings.Join(p.errs, "\n"))
	}
	// 没写时的默认值：生成到 pdb.xml 旁边的 ptable 目录，表建在 public 下
	if s.Package == "" {
		s.Package = "ptable"
	}
	if s.GenOutput == "" {
		s.GenOutput = "./ptable"
	}
	if s.Schema == "" {
		s.Schema = "public"
	}
	return s, nil
}

// node 是一个 XML 元素。line 是开始标签所在的行（标签跨行时是第一行）。
type node struct {
	name     string
	attrs    []xml.Attr
	line     int
	children []*node
}

func parseXML(r io.Reader, file string) (*node, error) {
	d := xml.NewDecoder(r)
	var root *node
	var stack []*node
	for {
		// 元素之间的空白和注释文字也是 token，所以读下一个 token 之前的位置就是开始标签的 '<'
		line, _ := d.InputPos()
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &node{name: t.Name.Local, attrs: t.Attr, line: line}
			if len(stack) == 0 {
				if root != nil {
					return nil, fmt.Errorf("%s:%d: 只能有一个根元素", file, line)
				}
				root = n
			} else {
				p := stack[len(stack)-1]
				p.children = append(p.children, n)
			}
			stack = append(stack, n)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		}
	}
	if root == nil {
		return nil, fmt.Errorf("%s: 没有根元素", file)
	}
	return root, nil
}

type parser struct {
	file string
	errs []string
}

func (p *parser) errorf(line int, format string, args ...any) {
	p.errs = append(p.errs, fmt.Sprintf("%s:%d: %s", p.file, line, fmt.Sprintf(format, args...)))
}

func (p *parser) schema(root *node) *Schema {
	s := &Schema{File: p.file}
	if root.name != "pdb" {
		p.errorf(root.line, "根元素应该是 <pdb>，实际是 <%s>", root.name)
		return s
	}
	p.attrs(root, map[string]any{
		"url":                               &s.URL,
		"genOutput":                         &s.GenOutput,
		"schema":                            &s.Schema,
		"package":                           &s.Package,
		"poolMaxConns":                      &s.PoolMaxConns,
		"poolMinConns":                      &s.PoolMinConns,
		"poolMaxConnLifetime":               &s.PoolMaxConnLifetime,
		"poolMaxConnIdleTime":               &s.PoolMaxConnIdleTime,
		"poolHealthCheckPeriod":             &s.PoolHealthCheckPeriod,
		"statementTimeoutMs":                &s.StatementTimeoutMs,
		"idleInTransactionSessionTimeoutMs": &s.IdleInTransactionSessionTimeoutMs,
		"lockTimeoutMs":                     &s.LockTimeoutMs,
		"transactionTimeoutMs":              &s.TransactionTimeoutMs,
		"commitTimeoutMs":                   &s.CommitTimeoutMs,
		"showSQL":                           &s.ShowSQL,
		"appName":                           &s.AppName,
	})
	for _, c := range root.children {
		switch c.name {
		case "bean":
			s.Beans = append(s.Beans, p.bean(c))
		case "table":
			s.Tables = append(s.Tables, p.table(c))
		default:
			p.errorf(c.line, "<pdb> 里不认识的元素 <%s>：只能是 <bean>、<table>", c.name)
		}
	}
	return s
}

func (p *parser) bean(n *node) Bean {
	b := Bean{Line: n.line}
	p.attrs(n, map[string]any{"name": &b.Name}, "name")
	for _, c := range n.children {
		if c.name != "variable" {
			p.errorf(c.line, "<bean> 里不认识的元素 <%s>：只能是 <variable>", c.name)
			continue
		}
		v := Variable{Line: c.line}
		p.attrs(c, map[string]any{"name": &v.Name, "type": &v.Type}, "name", "type")
		p.leaf(c)
		b.Variables = append(b.Variables, v)
	}
	return b
}

func (p *parser) table(n *node) Table {
	t := Table{Line: n.line}
	p.attrs(n, map[string]any{"name": &t.Name, "bean": &t.Bean, "lock": &t.Lock}, "name", "bean")
	for _, c := range n.children {
		switch c.name {
		case "primaryKey":
			pk := &PrimaryKey{Line: c.line}
			p.attrs(c, map[string]any{
				"variable":      &pk.Variable,
				"autoIncrement": &pk.AutoIncrement,
				"start":         &pk.Start,
				"step":          &pk.Step,
				"name":          &pk.Name,
			}, "variable")
			p.leaf(c)
			if t.PrimaryKey != nil {
				p.errorf(c.line, "<table name=%q> 有多个 <primaryKey>", t.Name)
				continue
			}
			t.PrimaryKey = pk
		case "index":
			ix := Index{Line: c.line}
			p.attrs(c, map[string]any{
				"variable": &ix.Variable,
				"unique":   &ix.Unique,
				"where":    &ix.Where,
				"method":   &ix.Method,
				"name":     &ix.Name,
			})
			p.leaf(c)
			t.Indexes = append(t.Indexes, ix)
		default:
			p.errorf(c.line, "<table> 里不认识的元素 <%s>：只能是 <primaryKey>、<index>", c.name)
		}
	}
	return t
}

// leaf 报告不该有子元素的元素里出现的子元素。
func (p *parser) leaf(n *node) {
	for _, c := range n.children {
		p.errorf(c.line, "<%s> 里不能有子元素 <%s>", n.name, c.name)
	}
}

// attrs 把 n 的属性解析进 fields 里对应的变量（*string、*bool、*int、*int32、*int64）。
// 不认识的、重复的属性和解析不了的值记错误；required 里的属性必须写，而且不能为空。
// 和 encoding/xml 一样，值两边的空白去掉，空值是零值。
func (p *parser) attrs(n *node, fields map[string]any, required ...string) {
	seen := make(map[string]bool, len(n.attrs))
	for _, a := range n.attrs {
		name := a.Name.Local
		if a.Name.Space != "" {
			name = a.Name.Space + ":" + name
		}
		dst, ok := fields[name]
		switch {
		case !ok:
			p.errorf(n.line, "<%s> 不认识的属性 %s%s", n.name, name, suggest(name, fields))
			continue
		case seen[name]:
			p.errorf(n.line, "<%s> 的属性 %s 写了两次", n.name, name)
			continue
		}
		seen[name] = true

		v := strings.TrimSpace(a.Value)
		var err error
		switch dst := dst.(type) {
		case *string:
			*dst = v
		case *bool:
			if v != "" {
				*dst, err = strconv.ParseBool(v)
			}
		case *int:
			*dst, err = parseInt[int](v, strconv.IntSize)
		case *int32:
			*dst, err = parseInt[int32](v, 32)
		case *int64:
			*dst, err = parseInt[int64](v, 64)
		default:
			panic(fmt.Sprintf("readxml: 属性 %s 的类型 %T 没有实现", name, dst))
		}
		if err != nil {
			p.errorf(n.line, "<%s> 的属性 %s=%q 不是合法的%s", n.name, name, a.Value, kindOf(dst))
		}
	}
	for _, name := range required {
		if s, ok := fields[name].(*string); !seen[name] || ok && *s == "" {
			p.errorf(n.line, "<%s> 缺少属性 %s", n.name, name)
		}
	}
}

func parseInt[T int | int32 | int64](v string, bits int) (T, error) {
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, bits)
	return T(n), err
}

func kindOf(dst any) string {
	if _, ok := dst.(*bool); ok {
		return "布尔值（true / false）"
	}
	return "整数"
}

// suggest 给拼错的属性名找最像的已知属性：只差大小写，或者编辑距离不超过 2。
func suggest(name string, fields map[string]any) string {
	best, bestDist := "", 3
	for _, k := range slices.Sorted(maps.Keys(fields)) { // 排序：距离相同时结果固定
		if d := distance(strings.ToLower(name), strings.ToLower(k)); d < bestDist {
			best, bestDist = k, d
		}
	}
	if best == "" {
		return ""
	}
	return "，是不是 " + best + "？"
}

// distance 是两个字符串的编辑距离（Levenshtein）。
func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := range len(a) {
		cur[0] = i + 1
		for j := range len(b) {
			cost := 1
			if a[i] == b[j] {
				cost = 0
			}
			cur[j+1] = min(prev[j+1]+1, cur[j]+1, prev[j]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
