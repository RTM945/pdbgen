package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"pdbgen/readxml"
	"strings"
	"text/template"
)

func main() {
	schemaFile := flag.String(
		"schema",
		"./pdb.xml",
		"schema xml file",
	)

	flag.Parse()

	schema, err := readxml.LoadSchema(*schemaFile)
	if err != nil {
		panic(err)
	}

	for _, table := range schema.Tables {
		data, err := generateTable(schema, table)
		if err != nil {
			panic(err)
		}

		filename := table.Name + ".go"
		output := filepath.Join(schema.GenOutput, filename)

		if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
			panic(err)
		}

		if err := os.WriteFile(output, data, 0o644); err != nil {
			panic(err)
		}

		fmt.Printf("generated %s\n", output)
	}

	tpl, err := template.New("context").Parse(constTemplate)
	if err != nil {
		panic(err)
	}
	var buf bytes.Buffer

	var data = struct {
		Package string
	}{
		Package: schema.Package,
	}

	if err := tpl.Execute(&buf, data); err != nil {
		panic(err)
	}

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		panic(err)
	}

	filename := "context.go"
	output := filepath.Join(schema.GenOutput, filename)

	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		panic(err)
	}

	if err := os.WriteFile(output, formatted, 0o644); err != nil {
		panic(err)
	}

	fmt.Printf("generated %s\n", output)
}

func generateTable(schema *readxml.Schema, table readxml.Table) ([]byte, error) {
	gen, err := buildTableGen(schema, table)
	if err != nil {
		return nil, err
	}

	data := readxml.TableFileGen{
		Package:  schema.Package,
		TableGen: gen,
	}

	tpl, err := template.New("generated").
		Funcs(template.FuncMap{
			"add": func(a, b int) int {
				return a + b
			},
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
		return nil, fmt.Errorf("gofmt generated source for table %q: %w", table.Name, err)
	}

	return formatted, nil
}

func buildTableGen(schema *readxml.Schema, table readxml.Table) (readxml.TableGen, error) {
	gen := readxml.TableGen{}
	bean, err := readxml.FindBean(schema, table.Bean)
	if err != nil {
		return gen, err
	}

	if table.PrimaryKey == nil {
		return gen, fmt.Errorf("table %q has no primary key", table.Name)
	}
	gen.TypeName = readxml.PascalCase(bean.Name)
	gen.ReceiverName = readxml.LowerCamelCase(bean.Name)
	gen.TableAccessor = readxml.PascalCase(table.Name) + "Table"
	gen.TableName = table.Name
	gen.ErrorName = "Err" + readxml.PascalCase(bean.Name) + "NotFound"

	fieldMap := make(map[string]readxml.Field, len(bean.Variables))

	for _, variable := range bean.Variables {
		if variable.Name == "" {
			return gen, fmt.Errorf("table %q has variable with empty name", table.Name)
		}

		if variable.Type == "" {
			return gen, fmt.Errorf("table %q variable %q has empty type", table.Name, variable.Name)
		}

		if _, exists := fieldMap[variable.Name]; exists {
			return gen, fmt.Errorf("table %q duplicate variable %q", table.Name, variable.Name)
		}

		field := readxml.Field{
			Name:       variable.Name,
			MethodName: readxml.PascalCase(variable.Name),
			Column:     readxml.SnakeCase(variable.Name),
			GoType:     variable.Type,
		}

		fieldMap[variable.Name] = field
		gen.Fields = append(gen.Fields, field)
	}

	pk, ok := fieldMap[table.PrimaryKey.Variable]
	if !ok {
		return gen, fmt.Errorf("table %q primary key variable %q not found", table.Name, table.PrimaryKey.Variable)
	}

	pk.IsPrimaryKey = true
	pk.AutoIncrement = table.PrimaryKey.AutoIncrement

	gen.PK = pk
	gen.AutoIncrement = pk.AutoIncrement

	for i := range gen.Fields {
		if gen.Fields[i].Name == pk.Name {
			gen.Fields[i] = pk
			break
		}
	}

	// INSERT:
	// 自增 PK 不需要传。
	for _, field := range gen.Fields {
		if field.IsPrimaryKey && field.AutoIncrement {
			continue
		}

		gen.InsertFields = append(gen.InsertFields, field)
	}

	// UPDATE:
	// PK 不更新，其余字段全部允许 dirty。
	for _, field := range gen.Fields {
		if field.IsPrimaryKey {
			continue
		}

		gen.UpdateFields = append(gen.UpdateFields, field)
	}

	columns := make([]string, 0, len(gen.Fields))

	for _, field := range gen.Fields {
		columns = append(columns, field.Column)
	}

	gen.SelectColumns = strings.Join(columns, ", ")

	// ------------------------------------------------------------
	// 查询方法
	// ------------------------------------------------------------

	seen := make(map[string]struct{})

	// PK:
	// GetById / SelectById
	pkQuery := readxml.QueryMethod{
		Name:   pk.MethodName,
		Params: pk.Name + " " + pk.GoType,
		Args:   pk.Name,
		Where:  pk.Column + " = $1",
		IsList: false,
	}

	gen.QueryMethods = append(gen.QueryMethods, pkQuery)

	seen[queryKey(pkQuery)] = struct{}{}

	// Index:
	//
	// unique=true:
	//     GetByUidActId / SelectByUidActId
	//
	// unique=false:
	//     ListByUid / SelectListByUid
	for _, index := range table.Indexes {
		varNames := readxml.SplitTrimSpace(index.Variable)

		if len(varNames) == 0 {
			return gen, fmt.Errorf("table %q has empty index", table.Name)
		}

		params := make([]string, 0, len(varNames))
		args := make([]string, 0, len(varNames))
		conditions := make([]string, 0, len(varNames))
		methodParts := make([]string, 0, len(varNames))

		for i, variableName := range varNames {
			field, ok := fieldMap[variableName]
			if !ok {
				return gen, fmt.Errorf("table %q index references unknown variable %q", table.Name, variableName)
			}

			params = append(params, fmt.Sprintf("%s %s", field.Name, field.GoType))

			args = append(args, field.Name)

			conditions = append(conditions,
				fmt.Sprintf("%s = $%d", field.Column, i+1),
			)

			methodParts = append(methodParts, field.MethodName)
		}

		query := readxml.QueryMethod{
			Name:   strings.Join(methodParts, ""),
			Params: strings.Join(params, ", "),
			Args:   strings.Join(args, ", "),
			Where:  strings.Join(conditions, " AND "),
			IsList: !index.Unique,
		}

		key := queryKey(query)

		if _, exists := seen[key]; exists {
			continue
		}

		seen[key] = struct{}{}

		gen.QueryMethods = append(gen.QueryMethods, query)
	}

	return gen, nil
}

func queryKey(query readxml.QueryMethod) string {
	return fmt.Sprintf(
		"%t|%s|%s|%s",
		query.IsList,
		query.Name,
		query.Params,
		query.Where,
	)
}
