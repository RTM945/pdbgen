package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"pdbgen/readxml"
)

func main() {
	schemaFile := flag.String("schema", "./pdb.xml", "schema xml file")
	flag.Parse()

	if err := run(*schemaFile); err != nil {
		fmt.Fprintln(os.Stderr, "genbeans:", err)
		os.Exit(1)
	}
}

func run(schemaFile string) error {
	schema, err := readxml.LoadSchema(schemaFile)
	if err != nil {
		return err
	}
	if err := checkTables(schema); err != nil {
		return err
	}
	for _, table := range schema.Tables {
		data, err := generateTable(schema, table)
		if err != nil {
			return err
		}
		output := filepath.Join(schema.GenOutput, table.Name+".go")
		if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(output, data, 0o644); err != nil {
			return err
		}
		fmt.Printf("generated %s\n", output)
	}
	return nil
}

// field 是 bean 里的一列。
type field struct {
	Name         string // Go 字段名，也是查询方法的参数名
	MethodName   string
	Column       string // 已加引号、已转义成 Go 字符串内容的列名：\"act_id\"
	GoType       string
	IsPrimaryKey bool
}

// queryMethod 是按主键或索引生成的一组查询方法。
type queryMethod struct {
	Name         string // GetBy<Name> / ListBy<Name>
	Params       string // uid int64, actId int32
	Args         string // uid, actId
	Where        string
	Columns      string // 条件里可更新列的 dirty 位，给 CheckPredicate；只有主键时为空
	IsList       bool
	IsPrimaryKey bool
}

type tableGen struct {
	Package       string
	TypeName      string
	ReceiverName  string
	TableAccessor string
	TableName     string

	Fields       []field
	UpdateFields []field // 除主键外的列，与 dirty 位一一对应
	PK           field

	SelectColumns string
	InsertColumns string
	InsertValues  string

	QueryMethods []queryMethod
}

var tpl = template.Must(template.New("table").Funcs(template.FuncMap{
	"quote": quoteIdent,
}).Parse(sourceTemplate))

func generateTable(schema *readxml.Schema, table readxml.Table) ([]byte, error) {
	gen, err := buildTableGen(schema, table)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, gen); err != nil {
		return nil, err
	}
	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("gofmt generated source for table %q: %w", table.Name, err)
	}
	return formatted, nil
}

// quoteIdent 返回嵌入 Go 字符串字面量后的 SQL 标识符：user -> \"user\"。
// 表名、列名一律加引号：user、end、order 这类保留字不加引号会直接语法错误。
func quoteIdent(s string) string {
	q := strconv.Quote(`"` + strings.ReplaceAll(s, `"`, `""`) + `"`)
	return q[1 : len(q)-1]
}

// supportedTypes 与 gen/ddl 的类型映射一致。
var supportedTypes = map[string]bool{
	"int64": true, "int32": true, "int16": true, "int8": true,
	"string": true, "bool": true, "float32": true, "float64": true,
}

// reservedNames 是生成代码自己用的名字：实体的内部字段，查询方法的接收者、参数和局部变量。
var reservedNames = map[string]bool{
	"loaded": true, "deleted": true, "detached": true, "dirty": true,
	"t": true, "o": true, "q": true, "ctx": true, "query": true,
}

// reservedMethods 是实体上已有的方法，列的 getter 不能和它们同名。
var reservedMethods = map[string]bool{
	"TableName": true, "Identity": true, "IsDeleted": true, "Delete": true, "Detach": true,
	"DirtyColumns": true, "FlushStmt": true, "FlushDone": true, "SnapshotState": true, "RestoreState": true,
}

func checkVariable(table string, v readxml.Variable) error {
	switch {
	case token.IsKeyword(v.Name):
		return fmt.Errorf("table %q: 列名 %q 是 Go 关键字，换个名字（比如 typ、kind）", table, v.Name)
	case !token.IsIdentifier(v.Name) || v.Name[0] < 'a' || v.Name[0] > 'z':
		return fmt.Errorf("table %q: 列名 %q 必须是小写字母开头的 Go 标识符", table, v.Name)
	case reservedNames[v.Name] || reservedMethods[readxml.PascalCase(v.Name)]:
		return fmt.Errorf("table %q: 列名 %q 和生成代码的内部名字冲突，换个名字", table, v.Name)
	case !supportedTypes[v.Type]:
		return fmt.Errorf("table %q: 列 %q 的类型 %q 不支持", table, v.Name, v.Type)
	}
	return nil
}

// checkTables 检查跨表的约束：一个 bean 只能映射一张表——生成的类型和表一一对应，
// 身份表按表名区分记录；表名、类型名都不能重复。
func checkTables(schema *readxml.Schema) error {
	tables := make(map[string]bool)
	types := make(map[string]string)
	for _, t := range schema.Tables {
		if tables[t.Name] {
			return fmt.Errorf("table %q 重复定义", t.Name)
		}
		tables[t.Name] = true
		typeName := readxml.PascalCase(t.Bean)
		if other, ok := types[typeName]; ok {
			return fmt.Errorf("table %q 和 %q 生成同一个类型 %s：一个 bean 只能映射一张表", t.Name, other, typeName)
		}
		types[typeName] = t.Name
	}
	return nil
}

func buildTableGen(schema *readxml.Schema, table readxml.Table) (tableGen, error) {
	gen := tableGen{Package: schema.Package, TableName: table.Name}
	bean, err := readxml.FindBean(schema, table.Bean)
	if err != nil {
		return gen, err
	}
	pk := table.PrimaryKey
	if pk == nil {
		return gen, fmt.Errorf("table %q has no primary key", table.Name)
	}
	gen.TypeName = readxml.PascalCase(bean.Name)
	gen.ReceiverName = readxml.LowerCamelCase(bean.Name)
	gen.TableAccessor = readxml.PascalCase(table.Name) + "Table"

	fields := make(map[string]field, len(bean.Variables))
	for _, v := range bean.Variables {
		if err := checkVariable(table.Name, v); err != nil {
			return gen, err
		}
		if _, ok := fields[v.Name]; ok {
			return gen, fmt.Errorf("table %q: 列 %q 重复", table.Name, v.Name)
		}
		f := field{
			Name:         v.Name,
			MethodName:   readxml.PascalCase(v.Name),
			Column:       quoteIdent(readxml.SnakeCase(v.Name)),
			GoType:       v.Type,
			IsPrimaryKey: v.Name == pk.Variable,
		}
		fields[v.Name] = f
		gen.Fields = append(gen.Fields, f)
		if f.IsPrimaryKey {
			gen.PK = f
		} else {
			gen.UpdateFields = append(gen.UpdateFields, f)
		}
	}

	// 身份表、Insert 回填、GetById 的缓存都按 int64 自增主键实现，其他主键一律拒绝。
	switch {
	case gen.PK.Name == "":
		return gen, fmt.Errorf("table %q: 主键 %q 不在 bean 里", table.Name, pk.Variable)
	case gen.PK.GoType != "int64" || !pk.AutoIncrement:
		return gen, fmt.Errorf("table %q: 主键 %q 必须是 int64 且 autoIncrement=\"true\"", table.Name, pk.Variable)
	case len(gen.UpdateFields) == 0:
		return gen, fmt.Errorf("table %q: 除主键外至少要有一列", table.Name)
	case len(gen.UpdateFields) > 64:
		return gen, fmt.Errorf("table %q: 除主键外最多 64 列（dirty 是 uint64 位掩码）", table.Name)
	}

	var cols, insertCols, insertValues []string
	for _, f := range gen.Fields {
		cols = append(cols, f.Column)
	}
	for i, f := range gen.UpdateFields {
		insertCols = append(insertCols, f.Column)
		insertValues = append(insertValues, "$"+strconv.Itoa(i+1))
	}
	gen.SelectColumns = strings.Join(cols, ", ")
	gen.InsertColumns = strings.Join(insertCols, ", ")
	gen.InsertValues = strings.Join(insertValues, ", ")

	// 查询方法：主键固定生成 GetById / SelectById；unique 索引生成 GetByXxx / SelectByXxx，
	// 非 unique 索引生成 ListByXxx / SelectListByXxx。同一组列只能有一个查询。
	pkQuery := gen.newQuery([]field{gen.PK}, true)
	pkQuery.IsPrimaryKey = true
	gen.QueryMethods = append(gen.QueryMethods, pkQuery)
	seen := map[string]bool{pk.Variable: true}
	names := map[string]bool{pkQuery.Name: true}
	for _, index := range table.Indexes {
		vars := readxml.SplitTrimSpace(index.Variable)
		key := strings.Join(vars, ",")
		if len(vars) == 0 {
			return gen, fmt.Errorf("table %q has empty index", table.Name)
		}
		if seen[key] {
			return gen, fmt.Errorf("table %q: 索引 (%s) 和主键或另一个索引重复", table.Name, key)
		}
		seen[key] = true

		indexFields := make([]field, 0, len(vars))
		used := make(map[string]bool, len(vars))
		for _, name := range vars {
			f, ok := fields[name]
			if !ok {
				return gen, fmt.Errorf("table %q index references unknown variable %q", table.Name, name)
			}
			if used[name] {
				return gen, fmt.Errorf("table %q: 索引 (%s) 里列 %q 重复", table.Name, key, name)
			}
			used[name] = true
			indexFields = append(indexFields, f)
		}
		q := gen.newQuery(indexFields, index.Unique)
		if names[q.Name] {
			return gen, fmt.Errorf("table %q: 索引 (%s) 生成的方法名 %s 和别的查询重复", table.Name, key, q.Name)
		}
		names[q.Name] = true
		gen.QueryMethods = append(gen.QueryMethods, q)
	}
	return gen, nil
}

func (gen *tableGen) newQuery(fields []field, unique bool) queryMethod {
	var name, params, args, where, columns []string
	for i, f := range fields {
		name = append(name, f.MethodName)
		params = append(params, f.Name+" "+f.GoType)
		args = append(args, f.Name)
		where = append(where, f.Column+" = $"+strconv.Itoa(i+1))
		if !f.IsPrimaryKey {
			columns = append(columns, "dirty"+gen.TypeName+f.MethodName)
		}
	}
	return queryMethod{
		Name:    strings.Join(name, ""),
		Params:  strings.Join(params, ", "),
		Args:    strings.Join(args, ", "),
		Where:   strings.Join(where, " AND "),
		Columns: strings.Join(columns, " | "),
		IsList:  !unique,
	}
}
