package readxml

import (
	"encoding/xml"
	"fmt"
	"os"
	"regexp"
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

	// Sensitive 标记敏感列（token、密码哈希等）：SQL 日志里它的参数只打成 '***'。
	Sensitive bool `xml:"sensitive,attr"`
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
