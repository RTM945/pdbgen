// Package gen 把 schema.PDB 描述的 bean+table 定义，生成对应的Go持久化代码。
//
// 生成出来的代码遵循几个约定（呼应之前的架构讨论）：
//   - 只有 Select 和 Update（按需 Insert），没有 Delete —— 业务上不需要删除玩家数据行
//   - Update 走"字段级乐观锁"：Load时给每个非主键字段记一份初始值快照，
//     Update时只对"被Set过的字段"生成 SET，并用快照值作为 WHERE 条件做CAS比对，
//     一次操作合并成一条SQL，不会逐字段拆成多条UPDATE
//   - 业务代码只应该调用 Load / SetXxx / Update，不会看到任何SQL字符串
//   - 不做 SELECT ... FOR UPDATE，因为乐观锁模式下读不需要占锁，
//     即使业务逻辑中间夹了耗时操作（比如调AI接口），也不会占用DB连接
package gen

import (
	"bytes"
	"fmt"
	"go/format"
	"sort"
	"strings"
	"text/template"
	"unicode"

	"pdbgen/schema"
)

// GeneratedFile 是一个待写出的Go源文件
type GeneratedFile struct {
	Name    string // 建议的文件名，如 model_users.go
	Content []byte
}

// Generate 为 pdb 中的每一张 table 各生成一个Go源文件
func Generate(pdb *schema.PDB, packageName string) ([]GeneratedFile, error) {
	var files []GeneratedFile
	for _, table := range pdb.Tables {
		bean := pdb.FindBean(table.Bean)
		if bean == nil {
			// schema.Parse阶段已经校验过，这里理论上不会发生
			return nil, fmt.Errorf("table %q 引用了不存在的 bean %q", table.Name, table.Bean)
		}
		data, err := buildTableData(packageName, bean, table)
		if err != nil {
			return nil, fmt.Errorf("table %q: %w", table.Name, err)
		}

		var buf bytes.Buffer
		if err := modelTmpl.Execute(&buf, data); err != nil {
			return nil, fmt.Errorf("table %q 渲染模板失败: %w", table.Name, err)
		}

		formatted, err := format.Source(buf.Bytes())
		if err != nil {
			// 保留未格式化的源码方便定位问题（比如模板产出的Go代码本身有语法错误）
			return nil, fmt.Errorf("table %q 生成的代码gofmt失败: %w\n--- 原始输出 ---\n%s", table.Name, err, buf.String())
		}

		files = append(files, GeneratedFile{
			Name:    fmt.Sprintf("model_%s_gen.go", table.Name),
			Content: formatted,
		})
	}
	return files, nil
}

// ---- 模板数据准备 ----

type fieldData struct {
	ExportedName string // Go导出字段名，如 LastLoginAt
	OrigName     string // 快照字段名（未导出），如 origLastLoginAt
	GoType       string
	Column       string // db列名（snake_case），如 last_login_at
	IsPK         bool
}

// indexParam 描述联合索引里的一个字段，用于生成 LoadXxxByYyy 的函数参数
type indexParam struct {
	ExportedName string
	GoType       string
}

type indexData struct {
	FuncSuffix  string // 生成 LoadXxxBy<FuncSuffix>；联合索引按字段顺序拼接，如 IdToken
	ColumnList  string // 注释里展示用，如 "id, token"
	WhereClause string // 预拼好的WHERE子句，如 "id = $1 AND token = $2"
	Params      []indexParam
	Unique      bool
}

type tableData struct {
	PackageName     string
	StructName      string // 导出的struct名，如 User
	TableName       string
	PK              fieldData
	PKAutoIncrement bool
	AllFields       []fieldData // 含主键，顺序与bean一致，供Load的SELECT列表使用
	NonPKFields     []fieldData // 不含主键，Update/Set只涉及这些
	Indexes         []indexData

	// 以下是预先在Go代码里拼好的字符串片段，模板只做插值，
	// 不在模板语法里做字符串拼接——模板越"笨"，生成逻辑越好读、越好调试。
	AllColumnList         string // "id, name, last_login_at, created_at, token"
	InsertColumnList      string // "name, last_login_at, created_at, token"（不含主键）
	InsertPlaceholderList string // "$1, $2, $3, $4"

	ExtraImports []string // 根据字段类型推导出的额外import，如 time.Time -> "time"
}

// goTypeImport 记录哪些bean字段类型需要额外import。
// 像int64/string/bool/float64这些内建类型不需要import，不在这里登记。
var goTypeImport = map[string]string{
	"time.Time": "time",
}

func buildTableData(packageName string, bean *schema.Bean, table schema.Table) (*tableData, error) {
	td := &tableData{
		PackageName:     packageName,
		StructName:      exportName(bean.Name),
		TableName:       table.Name,
		PKAutoIncrement: table.PrimaryKey.AutoIncrement,
	}

	for _, v := range bean.Variables {
		fd := fieldData{
			ExportedName: exportName(v.Name),
			OrigName:     "orig" + exportName(v.Name),
			GoType:       v.Type,
			Column:       schema.ColumnName(v.Name),
			IsPK:         v.Name == table.PrimaryKey.Variable,
		}
		td.AllFields = append(td.AllFields, fd)
		if !fd.IsPK {
			td.NonPKFields = append(td.NonPKFields, fd)
		} else {
			td.PK = fd
		}
	}
	if td.PK.ExportedName == "" {
		return nil, fmt.Errorf("primaryKey变量 %q 在bean中找不到", table.PrimaryKey.Variable)
	}

	for _, idx := range table.Indexes {
		vars := idx.Variables()
		var params []indexParam
		var funcSuffixParts []string
		var columns []string
		for _, vn := range vars {
			v := bean.FindVariable(vn)
			if v == nil {
				// schema.Parse阶段已校验过，这里理论上不会发生
				return nil, fmt.Errorf("index %q 引用了不存在的字段 %q", idx.Name, vn)
			}
			params = append(params, indexParam{ExportedName: exportName(vn), GoType: v.Type})
			funcSuffixParts = append(funcSuffixParts, exportName(vn))
			columns = append(columns, schema.ColumnName(vn))
		}

		whereParts := make([]string, len(columns))
		for i, col := range columns {
			whereParts[i] = fmt.Sprintf("%s = $%d", col, i+1)
		}

		td.Indexes = append(td.Indexes, indexData{
			FuncSuffix:  strings.Join(funcSuffixParts, ""),
			ColumnList:  strings.Join(columns, ", "),
			WhereClause: strings.Join(whereParts, " AND "),
			Params:      params,
			Unique:      idx.Unique,
		})
	}

	// 预拼字符串，模板里不再需要做任何拼接逻辑
	allCols := make([]string, len(td.AllFields))
	for i, f := range td.AllFields {
		allCols[i] = f.Column
	}
	td.AllColumnList = strings.Join(allCols, ", ")

	insertCols := make([]string, len(td.NonPKFields))
	placeholders := make([]string, len(td.NonPKFields))
	for i, f := range td.NonPKFields {
		insertCols[i] = f.Column
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	td.InsertColumnList = strings.Join(insertCols, ", ")
	td.InsertPlaceholderList = strings.Join(placeholders, ", ")

	// 推导额外import，去重后按字母排序，让生成的import块稳定、可复现
	seen := make(map[string]bool)
	for _, f := range td.AllFields {
		if imp, ok := goTypeImport[f.GoType]; ok && !seen[imp] {
			seen[imp] = true
			td.ExtraImports = append(td.ExtraImports, imp)
		}
	}
	sort.Strings(td.ExtraImports)

	return td, nil
}

// exportName 把 lastLoginAt 变成 LastLoginAt（Go导出标识符）
func exportName(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// ---- 模板 ----

var funcMap = template.FuncMap{
	// lowerFirst 把导出字段名变成合适的局部变量名，如 LastLoginAt -> lastLoginAt
	"lowerFirst": func(s string) string {
		if s == "" {
			return s
		}
		r := []rune(s)
		r[0] = unicode.ToLower(r[0])
		return string(r)
	},
}

var modelTmpl = template.Must(template.New("model").Funcs(funcMap).Parse(modelTmplSrc))
