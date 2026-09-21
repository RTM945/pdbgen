package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"
)

type Schema struct {
	XMLName   xml.Name `xml:"pdb"`
	GenOutput string   `xml:"genOutput,attr"`
	Package   string   `xml:"package,attr"`

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

func snakeCase(s string) string {
	if s == "" {
		return ""
	}

	s = firstCap.ReplaceAllString(s, `${1}_${2}`)
	s = allCap.ReplaceAllString(s, `${1}_${2}`)
	s = strings.ReplaceAll(s, "-", "_")

	return strings.ToLower(s)
}

func pascalCase(s string) string {
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

func lowerCamelCase(s string) string {
	p := pascalCase(s)
	if p == "" {
		return ""
	}

	return strings.ToLower(p[:1]) + p[1:]
}

func splitCSV(s string) []string {
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

func findBean(schema *Schema, name string) (*Bean, error) {
	for i := range schema.Beans {
		if schema.Beans[i].Name == name {
			return &schema.Beans[i], nil
		}
	}

	return nil, fmt.Errorf("bean %q not found", name)
}

func buildTableGen(schema *Schema, table Table) (TableGen, error) {
	bean, err := findBean(schema, table.Bean)
	if err != nil {
		return TableGen{}, err
	}

	if table.Name == "" {
		return TableGen{}, errors.New("table name is empty")
	}

	if bean.Name == "" {
		return TableGen{}, errors.New("bean name is empty")
	}

	if len(bean.Variables) == 0 {
		return TableGen{}, fmt.Errorf(
			"bean %q has no variables",
			bean.Name,
		)
	}

	if table.PrimaryKey == nil {
		return TableGen{}, fmt.Errorf(
			"table %q has no primaryKey",
			table.Name,
		)
	}

	gen := TableGen{
		TypeName:      pascalCase(bean.Name),
		ReceiverName:  lowerCamelCase(bean.Name),
		TableAccessor: pascalCase(table.Name) + "Table",
		TableName:     table.Name,
		ErrorName:     "Err" + pascalCase(bean.Name) + "NotFound",
	}

	fieldMap := make(map[string]Field, len(bean.Variables))

	for _, v := range bean.Variables {
		if v.Name == "" || v.Type == "" {
			return TableGen{}, fmt.Errorf(
				"table %q: variable name/type cannot be empty",
				table.Name,
			)
		}

		if _, exists := fieldMap[v.Name]; exists {
			return TableGen{}, fmt.Errorf(
				"table %q: duplicate variable %q",
				table.Name,
				v.Name,
			)
		}

		f := Field{
			Name:       v.Name,
			MethodName: pascalCase(v.Name),
			Column:     snakeCase(v.Name),
			GoType:     v.Type,
		}

		fieldMap[v.Name] = f
		gen.Fields = append(gen.Fields, f)

		if v.Type == "time.Time" {
			gen.NeedTimeImport = true
		}
	}

	pk, ok := fieldMap[table.PrimaryKey.Variable]
	if !ok {
		return TableGen{}, fmt.Errorf(
			"table %q: primary key variable %q not found",
			table.Name,
			table.PrimaryKey.Variable,
		)
	}

	pk.IsPrimaryKey = true
	pk.AutoIncrement = table.PrimaryKey.AutoIncrement

	gen.PK = pk
	gen.AutoIncrement = pk.AutoIncrement

	for i := range gen.Fields {
		if gen.Fields[i].Name == pk.Name {
			gen.Fields[i] = pk
		}
	}

	// INSERT:
	// auto increment PK 不需要传。
	for _, f := range gen.Fields {
		if f.IsPrimaryKey && f.AutoIncrement {
			continue
		}

		gen.InsertFields = append(gen.InsertFields, f)
	}

	// UPDATE:
	// PK 永远不更新，其他所有字段都可以更新。
	for _, f := range gen.Fields {
		if f.IsPrimaryKey {
			continue
		}

		gen.UpdateFields = append(gen.UpdateFields, f)
	}

	columns := make([]string, 0, len(gen.Fields))

	for _, f := range gen.Fields {
		columns = append(columns, f.Column)
	}

	gen.SelectColumns = strings.Join(columns, ", ")

	// 主键查询
	pkMethod := "GetBy" + pk.MethodName

	gen.QueryMethods = append(gen.QueryMethods, QueryMethod{
		MethodName: pkMethod,
		Params:     pk.Name + " " + pk.GoType,
		Args:       pk.Name,
		Where:      pk.Column + " = $1",
	})

	seenMethods := map[string]struct{}{
		pkMethod: {},
	}

	// unique index -> GetByXXX
	for _, index := range table.Indexes {
		if !index.Unique {
			continue
		}

		varNames := splitCSV(index.Variable)

		if len(varNames) == 0 {
			return TableGen{}, fmt.Errorf(
				"table %q: index %q has empty variable list",
				table.Name,
				index.Name,
			)
		}

		params := make([]string, 0, len(varNames))
		args := make([]string, 0, len(varNames))
		conditions := make([]string, 0, len(varNames))
		methodParts := make([]string, 0, len(varNames))

		for i, name := range varNames {
			f, exists := fieldMap[name]

			if !exists {
				return TableGen{}, fmt.Errorf(
					"table %q: index %q references unknown variable %q",
					table.Name,
					index.Name,
					name,
				)
			}

			params = append(
				params,
				fmt.Sprintf("%s %s", f.Name, f.GoType),
			)

			args = append(args, f.Name)

			conditions = append(
				conditions,
				fmt.Sprintf("%s = $%d", f.Column, i+1),
			)

			methodParts = append(
				methodParts,
				f.MethodName,
			)
		}

		methodName := "GetBy" + strings.Join(methodParts, "")

		if _, exists := seenMethods[methodName]; exists {
			continue
		}

		seenMethods[methodName] = struct{}{}

		gen.QueryMethods = append(
			gen.QueryMethods,
			QueryMethod{
				MethodName: methodName,
				Params:     strings.Join(params, ", "),
				Args:       strings.Join(args, ", "),
				Where:      strings.Join(conditions, " AND "),
			},
		)
	}

	return gen, nil
}

func joinFieldColumns(fields []Field) string {
	cols := make([]string, 0, len(fields))

	for _, f := range fields {
		cols = append(cols, f.Column)
	}

	return strings.Join(cols, ", ")
}

func placeholders(fields []Field) string {
	result := make([]string, 0, len(fields))

	for i := range fields {
		result = append(
			result,
			fmt.Sprintf("$%d", i+1),
		)
	}

	return strings.Join(result, ", ")
}

func sortTables(tables []TableGen) {
	sort.Slice(tables, func(i, j int) bool {
		return tables[i].TableName < tables[j].TableName
	})
}

const sourceTemplate = `// Code generated by pdbgen; DO NOT EDIT.

package {{ .Package }}

import (
	"context"
	"errors"
	"fmt"
	"strings"
{{- if .NeedTimeImport }}
	"time"
{{- end }}

	"github.com/jackc/pgx/v5"
)

{{ $table := . }}

var {{ .TableAccessor }} {{ .ReceiverName }}

type {{ .ReceiverName }} struct {}

type {{ .TypeName }} struct {
{{- range .Fields }}
	{{ .Name }} {{ .GoType }}
{{- end }}

	loaded bool

{{- range .UpdateFields }}
	orig{{ .MethodName }} {{ .GoType }}
{{- end }}

	dirty map[string]struct{}
}

func New{{ .TypeName }}() *{{ .TypeName }} {
	return &{{ .TypeName }}{
		dirty: make(map[string]struct{}),
	}
}

func loaded{{ .TypeName }}(
{{- range .Fields }}
	{{ .Name }} {{ .GoType }},
{{- end }}
) *{{ .TypeName }} {
	return &{{ .TypeName }}{
{{- range .Fields }}
		{{ .Name }}: {{ .Name }},
{{- end }}

		loaded: true,

{{- range .UpdateFields }}
		orig{{ .MethodName }}: {{ .Name }},
{{- end }}

		dirty: make(map[string]struct{}),
	}
}

{{ range .Fields }}
{{- if not .IsPrimaryKey }}

func (o *{{ $table.TypeName }}) Set{{ .MethodName }}(v {{ .GoType }}) {
	if o.{{ .Name }} == v {
		return
	}

	o.{{ .Name }} = v
	o.dirty["{{ .Column }}"] = struct{}{}
}

func (o *{{ $table.TypeName }}) {{ .MethodName }}() {{ .GoType }} {
	return o.{{ .Name }}
}

{{ end }}
{{ end }}

func (o *{{ .TypeName }}) {{ .PK.MethodName }}() {{ .PK.GoType }} {
	return o.{{ .PK.Name }}
}

// ResetToLoaded restores every non-primary-key field to the last
// successfully loaded/inserted/updated snapshot.
func (o *{{ .TypeName }}) ResetToLoaded() {
{{- range .UpdateFields }}
	o.{{ .Name }} = o.orig{{ .MethodName }}
{{- end }}

	o.dirty = make(map[string]struct{})
}

const selectColumns{{ .TypeName }} = "{{ .SelectColumns }}"

func scan{{ .TypeName }}(row pgx.Row) *{{ .TypeName }} {
	var (
{{- range .Fields }}
		{{ .Name }} {{ .GoType }}
{{- end }}
	)

	if err := row.Scan(
{{- range .Fields }}
		&{{ .Name }},
{{- end }}
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}

		panic(err)
	}

	return loaded{{ .TypeName }}(
{{- range .Fields }}
		{{ .Name }},
{{- end }}
	)
}

{{ range .QueryMethods }}

func ({{ $table.ReceiverName }}) {{ .MethodName }}(
	ctx context.Context,
	{{ .Params }},
) *{{ $table.TypeName }} {
	tx := txFromCtx(ctx)

	const q =
		"SELECT " + selectColumns{{ $table.TypeName }} +
			" FROM {{ $table.TableName }}" +
			" WHERE {{ .Where }}"

	row := tx.QueryRow(
		ctx,
		q,
		{{ .Args }},
	)

	return scan{{ $table.TypeName }}(row)
}

{{ end }}

func ({{ .ReceiverName }}) Update(
	ctx context.Context,
	o *{{ .TypeName }},
) error {
	tx := txFromCtx(ctx)

	if !o.loaded {
		return errors.New(
			"update {{ .TypeName }} must load first",
		)
	}

	if len(o.dirty) == 0 {
		return nil
	}

	var sets []string
	var args []any

	n := 0

	next := func() int {
		n++
		return n
	}

{{ range .UpdateFields }}
	if _, ok := o.dirty["{{ .Column }}"]; ok {
		sets = append(
			sets,
			fmt.Sprintf(
				"{{ .Column }} = $%d",
				next(),
			),
		)

		args = append(
			args,
			o.{{ .Name }},
		)
	}
{{ end }}

	args = append(args, o.{{ .PK.Name }})

	q := fmt.Sprintf(
		"UPDATE {{ .TableName }} SET %s WHERE {{ .PK.Column }} = $%d",
		strings.Join(sets, ", "),
		n+1,
	)

	tag, err := tx.Exec(ctx, q, args...)
	if err != nil {
		panic(err)
	}

	if tag.RowsAffected() == 0 {
		return {{ .ErrorName }}
	}

{{ range .UpdateFields }}
	o.orig{{ .MethodName }} = o.{{ .Name }}
{{ end }}

	o.dirty = make(map[string]struct{})

	return nil
}

var {{ .ErrorName }} = errors.New(
	"{{ .TableName }}: row not found at update time",
)

func ({{ .ReceiverName }}) Insert(
	ctx context.Context,
	o *{{ .TypeName }},
) {
	tx := txFromCtx(ctx)

	const q =
		"INSERT INTO {{ .TableName }} " +
			"({{ joinColumns .InsertFields }}) " +
			"VALUES ({{ placeholders .InsertFields }})" +
			{{ if .AutoIncrement }} " RETURNING {{ .PK.Column }}" {{ end }}

{{ if .AutoIncrement }}

	row := tx.QueryRow(
		ctx,
		q{{ range .InsertFields }},
		o.{{ .Name }}{{ end }},
	)

	if err := row.Scan(&o.{{ .PK.Name }}); err != nil {
		panic(err)
	}

{{ else }}

	if _, err := tx.Exec(
		ctx,
		q{{ range .InsertFields }},
		o.{{ .Name }}{{ end }},
	); err != nil {
		panic(err)
	}

{{ end }}

	o.loaded = true

{{ range .UpdateFields }}
	o.orig{{ .MethodName }} = o.{{ .Name }}
{{ end }}

	o.dirty = make(map[string]struct{})
}
`

func loadSchema(filename string) (*Schema, error) {
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

func generateTable(
	schema *Schema,
	table Table,
) ([]byte, error) {
	gen, err := buildTableGen(schema, table)
	if err != nil {
		return nil, err
	}

	data := TableFileGen{
		Package:  schema.Package,
		TableGen: gen,
	}

	tpl, err := template.New("generated").
		Funcs(template.FuncMap{
			"joinColumns":  joinFieldColumns,
			"placeholders": placeholders,
		}).
		Parse(sourceTemplate)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer

	if err := tpl.Execute(&buf, data); err != nil {
		return nil, err
	}

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf(
			"gofmt generated source for table %q: %w",
			table.Name,
			err,
		)
	}

	return formatted, nil
}

func main() {
	schemaFile := flag.String(
		"schema",
		"./pdb.xml",
		"schema xml file",
	)

	outDir := flag.String(
		"out",
		"ptable",
		"generated output directory; default: genOutput in schema",
	)

	packageName := flag.String(
		"package",
		"ptable",
		"generated package name",
	)

	flag.Parse()

	schema, err := loadSchema(*schemaFile)
	if err != nil {
		panic(err)
	}

	// schema 中指定了 package 时，以 schema 为准；
	// 命令行显式传入 -package 时覆盖。
	if schema.Package == "" {
		schema.Package = *packageName
	} else if *packageName != "ptable" {
		schema.Package = *packageName
	}

	if *outDir == "" {
		*outDir = schema.GenOutput
	}

	if *outDir == "" {
		*outDir = "./ptable"
	}

	for _, table := range schema.Tables {
		data, err := generateTable(schema, table)
		if err != nil {
			panic(err)
		}

		filename := table.Name + ".go"
		output := filepath.Join(*outDir, filename)

		if err := os.MkdirAll(
			filepath.Dir(output),
			0o755,
		); err != nil {
			panic(err)
		}

		if err := os.WriteFile(
			output,
			data,
			0o644,
		); err != nil {
			panic(err)
		}

		fmt.Printf(
			"generated %s\n",
			output,
		)
	}
}
