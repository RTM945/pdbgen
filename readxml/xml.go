package readxml

import (
	"encoding/xml"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

type Schema struct {
	XMLName      xml.Name `xml:"pdb"`
	URL          string   `xml:"url,attr"`
	GenOutput    string   `xml:"genOutput,attr"`
	Schema       string   `xml:"schema,attr"`
	Package      string   `xml:"package,attr"`
	PoolMaxConns int32    `xml:"poolMaxConns,attr"`
	PoolMinConns int32    `xml:"poolMinConns,attr"`

	PoolMaxConnLifetime   int `xml:"poolMaxConnLifetime,attr"`
	PoolMaxConnIdleTime   int `xml:"poolMaxConnIdleTime,attr"`
	PoolHealthCheckPeriod int `xml:"poolHealthCheckPeriod,attr"`

	StatementTimeoutMs                int `xml:"statementTimeoutMs,attr"`
	IdleInTransactionSessionTimeoutMs int `xml:"idleInTransactionSessionTimeoutMs,attr"`
	LockTimeoutMs                     int `xml:"lockTimeoutMs,attr"`

	// TransactionTimeoutMs 一次 Procedure（BEGIN 到 COMMIT）或 dbpool.Query 回调的客户端总时长上限。
	// statement_timeout 只管单条语句，管不住"很多条语句加起来很慢"；
	// PG 17 之前也没有事务级的 GUC，所以只能在客户端这边包一层。
	TransactionTimeoutMs int `xml:"transactionTimeoutMs,attr"`

	ShowSQL bool `xml:"showSQL,attr"`

	AppName string `xml:"appName,attr"`

	Beans  []Bean  `xml:"bean"`
	Tables []Table `xml:"table"`
}

type Bean struct {
	Name      string     `xml:"name,attr"`
	Variables []Variable `xml:"variable"`
}

type Variable struct {
	Name string `xml:"name,attr"`
	Type string `xml:"type,attr"`
}

type Table struct {
	Name       string      `xml:"name,attr"`
	Bean       string      `xml:"bean,attr"`
	PrimaryKey *PrimaryKey `xml:"primaryKey"`
	Indexes    []Index     `xml:"index"`
}

type PrimaryKey struct {
	Variable      string `xml:"variable,attr"`
	AutoIncrement bool   `xml:"autoIncrement,attr"`
	Start         int64  `xml:"start,attr"`
	Step          int64  `xml:"step,attr"`
}

type Index struct {
	Variable string `xml:"variable,attr"`
	Unique   bool   `xml:"unique,attr"`
	// Where 非空就是部分索引（CREATE INDEX ... WHERE）：逗号分隔的 变量=常量，之间是 AND，见 ParseWhere。
	// 这时 Variable 可以为空（索引建在主键上，查询没有参数）。
	Where string `xml:"where,attr"`
	// Method 是部分索引生成的方法名：List<Method> / SelectList<Method>，unique 时 Get<Method> / Select<Method>。
	// 只用于部分索引，必填；普通索引的方法名由列名拼出。
	Method string `xml:"method,attr"`
}

// Condition 是部分索引 where 里的一个条件：列 = 常量。
type Condition struct {
	Variable string // bean 里的变量名
	Column   string // 列名（snake_case，不带引号）
	Value    string // bool 列是 true / false，整数列是规范化后的十进制整数
	Bool     bool
}

// SQL 返回这个条件的 SQL 片段，quote 给列名加引号。
// bool 列写成 col / NOT col：PostgreSQL 基本原样存进索引定义，查询里写成同样的形式，规划器才能认出能用这个索引。
func (c Condition) SQL(quote func(string) string) string {
	switch {
	case c.Bool && c.Value == "true":
		return quote(c.Column)
	case c.Bool:
		return "NOT " + quote(c.Column)
	}
	return quote(c.Column) + " = " + c.Value
}

// WhereSQL 把条件用 AND 连起来。
func WhereSQL(conds []Condition, quote func(string) string) string {
	parts := make([]string, len(conds))
	for i, c := range conds {
		parts[i] = c.SQL(quote)
	}
	return strings.Join(parts, " AND ")
}

// intBits 是整数类型的位数，校验常量范围用。
var intBits = map[string]int{"int64": 64, "int32": 32, "int16": 16, "int8": 8}

// ParseWhere 解析部分索引的 where（如 "workerEnabled=true"、"state=0, kind=2"）。
// 只支持 bool 和整数列（常量写进 SQL，查询和索引条件要逐字一致，字符串和浮点数的写法太多，不支持）；
// 不能用主键，同一列只能出现一次。
func ParseWhere(bean *Bean, primaryKey, where string) ([]Condition, error) {
	types := make(map[string]string, len(bean.Variables))
	for _, v := range bean.Variables {
		types[v.Name] = v.Type
	}
	var conds []Condition
	seen := map[string]bool{}
	for _, part := range SplitTrimSpace(where) {
		name, value, ok := strings.Cut(part, "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || name == "" || value == "" {
			return nil, fmt.Errorf("where %q: %q 不是 变量=常量", where, part)
		}
		typ, ok := types[name]
		switch {
		case !ok:
			return nil, fmt.Errorf("where %q: 变量 %q 不在 bean 里", where, name)
		case name == primaryKey:
			return nil, fmt.Errorf("where %q: 不能用主键 %q", where, name)
		case seen[name]:
			return nil, fmt.Errorf("where %q: 变量 %q 重复", where, name)
		}
		seen[name] = true
		c := Condition{Variable: name, Column: SnakeCase(name)}
		switch {
		case typ == "bool":
			if value != "true" && value != "false" {
				return nil, fmt.Errorf("where %q: bool 变量 %q 只能等于 true 或 false", where, name)
			}
			c.Value, c.Bool = value, true
		case intBits[typ] > 0:
			n, err := strconv.ParseInt(value, 10, intBits[typ])
			if err != nil {
				return nil, fmt.Errorf("where %q: %q 不是 %s 范围内的整数", where, value, typ)
			}
			c.Value = strconv.FormatInt(n, 10)
		default:
			return nil, fmt.Errorf("where %q: 变量 %q 是 %s，只支持 bool 和整数", where, name, typ)
		}
		conds = append(conds, c)
	}
	if len(conds) == 0 {
		return nil, fmt.Errorf("where 为空")
	}
	return conds, nil
}

var (
	firstCap = regexp.MustCompile(`(.)([A-Z][a-z]+)`)
	allCap   = regexp.MustCompile(`([a-z0-9])([A-Z])`)
)

func SnakeCase(s string) string {
	if s == "" {
		return ""
	}

	s = firstCap.ReplaceAllString(s, `${1}_${2}`)
	s = allCap.ReplaceAllString(s, `${1}_${2}`)
	s = strings.ReplaceAll(s, "-", "_")

	return strings.ToLower(s)
}

func PascalCase(s string) string {
	if s == "" {
		return ""
	}

	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '_' || r == '-' || r == ' '
	})

	var b strings.Builder

	for _, p := range parts {
		if p == "" {
			continue
		}

		b.WriteString(strings.ToUpper(p[:1]))
		b.WriteString(p[1:])
	}

	return b.String()
}

func LowerCamelCase(s string) string {
	p := PascalCase(s)
	if p == "" {
		return ""
	}

	return strings.ToLower(p[:1]) + p[1:]
}

func SplitTrimSpace(s string) []string {
	parts := strings.Split(s, ",")

	result := make([]string, 0, len(parts))

	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}

	return result
}

func FindBean(schema *Schema, name string) (*Bean, error) {
	for i := range schema.Beans {
		if schema.Beans[i].Name == name {
			return &schema.Beans[i], nil
		}
	}

	return nil, fmt.Errorf("bean %q not found", name)
}

func LoadSchema(filename string) (*Schema, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	var schema Schema

	if err := xml.Unmarshal(data, &schema); err != nil {
		return nil, err
	}

	if schema.Package == "" {
		schema.Package = "ptable"
	}

	return &schema, nil
}
