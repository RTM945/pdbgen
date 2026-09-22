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
	Name          string `xml:"name,attr"`
	Variable      string `xml:"variable,attr"`
	AutoIncrement bool   `xml:"autoIncrement,attr"`
	Start         int64  `xml:"start,attr"`
}

type Index struct {
	Name     string `xml:"name,attr"`
	Variable string `xml:"variable,attr"`
	Unique   bool   `xml:"unique,attr"`
}

type Field struct {
	Name          string
	MethodName    string
	Column        string
	GoType        string
	IsPrimaryKey  bool
	AutoIncrement bool
}

type QueryMethod struct {
	MethodName string
	Params     string
	Args       string
	Where      string
}

type TableGen struct {
	TypeName       string
	ReceiverName   string
	TableName      string
	TableAccessor  string
	Fields         []Field
	UpdateFields   []Field
	InsertFields   []Field
	PK             Field
	AutoIncrement  bool
	SelectColumns  string
	ErrorName      string
	QueryMethods   []QueryMethod
	NeedTimeImport bool
}

type TableFileGen struct {
	Package string
	TableGen
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

func JoinFieldColumns(fields []Field) string {
	cols := make([]string, 0, len(fields))

	for _, f := range fields {
		cols = append(cols, f.Column)
	}

	return strings.Join(cols, ", ")
}

func Placeholders(fields []Field) string {
	result := make([]string, 0, len(fields))

	for i := range fields {
		result = append(
			result,
			fmt.Sprintf("$%d", i+1),
		)
	}

	return strings.Join(result, ", ")
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
