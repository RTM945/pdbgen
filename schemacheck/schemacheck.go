// Package schemacheck 对比数据库里表的实际结构，和 pdb.xml 里声明的结构是否一致。
//
// 这个包只读，不会执行任何 CREATE/ALTER/DROP——表结构变更是有风险的操作
// （锁表、影响线上流量），应该由人看着建议的SQL自己决定要不要改、什么时候改，
// 不应该被生成器自动执行。这里只做一件事：告诉你哪里对不上、可能需要什么样的ALTER语句。
package schemacheck

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"pdbgen/schema"
)

// DiffKind 差异类型
type DiffKind string

const (
	MissingTable        DiffKind = "缺少整张表"
	MissingColumn       DiffKind = "缺少列"
	ExtraColumn         DiffKind = "数据库里多出的列(xml未声明)"
	TypeMismatch        DiffKind = "列类型不一致"
	NullableIssue       DiffKind = "可空性不一致"
	MissingPrimary      DiffKind = "缺少主键约束"
	MissingIndex        DiffKind = "缺少索引"
	IndexColumnMismatch DiffKind = "索引列不一致"
	ExtraIndex          DiffKind = "数据库里多出的索引(xml未声明)"
)

// Diff 一条差异记录
type Diff struct {
	Table        string
	Kind         DiffKind
	Detail       string
	SuggestedSQL string // 仅供参考，不会被本工具执行
}

func (d Diff) String() string {
	s := fmt.Sprintf("[%s] table=%s: %s", d.Kind, d.Table, d.Detail)
	if d.SuggestedSQL != "" {
		s += fmt.Sprintf("\n    建议SQL(需人工确认后手动执行): %s", d.SuggestedSQL)
	}
	return s
}

// pgTypeToInfoSchema 把 schema.PGType() 产出的DDL类型名，
// 转成 information_schema.columns.data_type 里实际会看到的写法，用于比对。
var pgTypeToInfoSchema = map[string]string{
	"BIGINT":           "bigint",
	"INTEGER":          "integer",
	"TEXT":             "text",
	"BOOLEAN":          "boolean",
	"DOUBLE PRECISION": "double precision",
	"TIMESTAMPTZ":      "timestamp with time zone",
}

type dbColumn struct {
	Name       string
	DataType   string
	IsNullable bool
	IsIdentity bool
}

// Check 对pdb里的每一张table，查询db的实际结构并给出差异列表。
// db需要已经指向目标数据库（哪个schema/哪个库由调用方在连接串里决定，这里固定查 public schema）。
func Check(ctx context.Context, db *sql.DB, pdb *schema.PDB) ([]Diff, error) {
	pgSchema := pdb.Schema
	if pgSchema == "" {
		pgSchema = "public" // pdb.Schema 正常情况下已经在schema.Parse阶段被兜底成public，这里是双重保险
	}

	var diffs []Diff
	for _, table := range pdb.Tables {
		bean := pdb.FindBean(table.Bean)
		if bean == nil {
			return nil, fmt.Errorf("table %q 引用了不存在的 bean %q", table.Name, table.Bean)
		}

		exists, err := tableExists(ctx, db, pgSchema, table.Name)
		if err != nil {
			return nil, err
		}
		if !exists {
			diffs = append(diffs, Diff{
				Table:  table.Name,
				Kind:   MissingTable,
				Detail: "数据库中不存在这张表",
			})
			continue // 整张表都不存在，后面的列/索引比对没有意义
		}

		colDiffs, err := checkColumns(ctx, db, pgSchema, bean, table)
		if err != nil {
			return nil, err
		}
		diffs = append(diffs, colDiffs...)

		pkDiffs, err := checkPrimaryKey(ctx, db, pgSchema, table)
		if err != nil {
			return nil, err
		}
		diffs = append(diffs, pkDiffs...)

		idxDiffs, err := checkIndexes(ctx, db, pgSchema, table)
		if err != nil {
			return nil, err
		}
		diffs = append(diffs, idxDiffs...)
	}
	return diffs, nil
}

func tableExists(ctx context.Context, db *sql.DB, pgSchema, table string) (bool, error) {
	const q = `SELECT EXISTS (
		SELECT 1 FROM information_schema.tables
		WHERE table_schema = $1 AND table_name = $2
	)`
	var exists bool
	if err := db.QueryRowContext(ctx, q, pgSchema, table).Scan(&exists); err != nil {
		return false, fmt.Errorf("检查表 %q 是否存在: %w", table, err)
	}
	return exists, nil
}

func fetchColumns(ctx context.Context, db *sql.DB, pgSchema, table string) (map[string]dbColumn, error) {
	const q = `SELECT column_name, data_type, is_nullable, is_identity
		FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2`
	rows, err := db.QueryContext(ctx, q, pgSchema, table)
	if err != nil {
		return nil, fmt.Errorf("读取表 %q 的列信息: %w", table, err)
	}
	defer rows.Close()

	cols := make(map[string]dbColumn)
	for rows.Next() {
		var c dbColumn
		var nullable, identity string
		if err := rows.Scan(&c.Name, &c.DataType, &nullable, &identity); err != nil {
			return nil, err
		}
		c.IsNullable = nullable == "YES"
		c.IsIdentity = identity == "YES"
		cols[c.Name] = c
	}
	return cols, rows.Err()
}

func checkColumns(ctx context.Context, db *sql.DB, pgSchema string, bean *schema.Bean, table schema.Table) ([]Diff, error) {
	actual, err := fetchColumns(ctx, db, pgSchema, table.Name)
	if err != nil {
		return nil, err
	}

	var diffs []Diff
	expectedCols := make(map[string]bool)

	for _, v := range bean.Variables {
		col := camelToSnake(v.Name)
		expectedCols[col] = true

		pgType, err := schema.PGType(v.Type)
		if err != nil {
			return nil, fmt.Errorf("bean %q 字段 %q: %w", bean.Name, v.Name, err)
		}
		expectedDataType := pgTypeToInfoSchema[pgType]

		isPK := v.Name == table.PrimaryKey.Variable

		actualCol, ok := actual[col]
		if !ok {
			var suggest string
			if isPK {
				suggest = fmt.Sprintf("-- 缺主键列的情况建议对照整体建表流程重新处理，不建议单独补一条ALTER ADD COLUMN")
			} else {
				suggest = fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s NOT NULL DEFAULT <需要人工决定默认值>;", table.Name, col, pgType)
			}
			diffs = append(diffs, Diff{
				Table:        table.Name,
				Kind:         MissingColumn,
				Detail:       fmt.Sprintf("bean %s.%s 对应的列 %q 在数据库里不存在", bean.Name, v.Name, col),
				SuggestedSQL: suggest,
			})
			continue
		}

		if actualCol.DataType != expectedDataType {
			diffs = append(diffs, Diff{
				Table:  table.Name,
				Kind:   TypeMismatch,
				Detail: fmt.Sprintf("列 %q 期望类型 %q，实际是 %q", col, expectedDataType, actualCol.DataType),
				SuggestedSQL: fmt.Sprintf(
					"-- 改列类型可能导致数据截断/转换失败，务必先确认现有数据兼容再执行:\nALTER TABLE %s ALTER COLUMN %s TYPE %s USING %s::%s;",
					table.Name, col, pgType, col, pgType),
			})
		}

		// 自增主键列由 IDENTITY 隐含 NOT NULL，不需要额外检查可空性
		if !isPK && actualCol.IsNullable {
			diffs = append(diffs, Diff{
				Table:  table.Name,
				Kind:   NullableIssue,
				Detail: fmt.Sprintf("列 %q 允许为NULL，但bean字段是非指针类型，读到NULL会导致Scan报错", col),
				SuggestedSQL: fmt.Sprintf(
					"-- 加NOT NULL前要确认表里没有NULL值，否则这条ALTER本身会失败:\nALTER TABLE %s ALTER COLUMN %s SET NOT NULL;",
					table.Name, col),
			})
		}
	}

	// 数据库里存在、但xml没声明的列——不建议自动给出DROP COLUMN建议，
	// 删列是破坏性操作，只提示，具体怎么处理由人决定。
	var extra []string
	for name := range actual {
		if !expectedCols[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		diffs = append(diffs, Diff{
			Table:  table.Name,
			Kind:   ExtraColumn,
			Detail: fmt.Sprintf("数据库里存在列 %q，但当前bean定义里没有对应字段（是否是遗留字段？需要人工确认，不建议在这里直接给DROP建议）", name),
		})
	}

	return diffs, nil
}

func checkPrimaryKey(ctx context.Context, db *sql.DB, pgSchema string, table schema.Table) ([]Diff, error) {
	const q = `SELECT EXISTS (
		SELECT 1 FROM information_schema.table_constraints
		WHERE table_schema = $1 AND table_name = $2 AND constraint_type = 'PRIMARY KEY'
	)`
	var exists bool
	if err := db.QueryRowContext(ctx, q, pgSchema, table.Name).Scan(&exists); err != nil {
		return nil, fmt.Errorf("检查表 %q 主键: %w", table.Name, err)
	}
	if exists {
		return nil, nil
	}
	col := camelToSnake(table.PrimaryKey.Variable)
	return []Diff{{
		Table:  table.Name,
		Kind:   MissingPrimary,
		Detail: fmt.Sprintf("表 %q 没有主键约束（期望主键列: %s）", table.Name, col),
		SuggestedSQL: fmt.Sprintf(
			"ALTER TABLE %s ADD CONSTRAINT %s PRIMARY KEY (%s);",
			table.Name, table.PrimaryKey.Name, col),
	}}, nil
}

func checkIndexes(ctx context.Context, db *sql.DB, pgSchema string, table schema.Table) ([]Diff, error) {
	const q = `SELECT indexname FROM pg_indexes WHERE schemaname = $1 AND tablename = $2`
	rows, err := db.QueryContext(ctx, q, pgSchema, table.Name)
	if err != nil {
		return nil, fmt.Errorf("读取表 %q 的索引: %w", table.Name, err)
	}
	defer rows.Close()

	actualNames := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		actualNames[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var diffs []Diff
	expected := make(map[string]bool)
	for _, idx := range table.Indexes {
		expected[idx.Name] = true

		vars := idx.Variables()
		cols := make([]string, len(vars))
		for i, v := range vars {
			cols[i] = camelToSnake(v)
		}
		colList := strings.Join(cols, ", ")
		uniqueKw := ""
		if idx.Unique {
			uniqueKw = "UNIQUE "
		}

		if !actualNames[idx.Name] {
			diffs = append(diffs, Diff{
				Table:        table.Name,
				Kind:         MissingIndex,
				Detail:       fmt.Sprintf("索引 %q（列 %s）在数据库里不存在", idx.Name, colList),
				SuggestedSQL: fmt.Sprintf("CREATE %sINDEX %s ON %s (%s);", uniqueKw, idx.Name, table.Name, colList),
			})
			continue
		}

		// 名字对得上不代表列一致——联合索引的列顺序决定了"最左前缀"规则下
		// 哪些查询能命中这个索引，名字相同但列顺序不同，实际上是完全不同的索引。
		actualCols, err := fetchIndexColumns(ctx, db, pgSchema, idx.Name)
		if err != nil {
			return nil, err
		}
		if !equalStringSlice(actualCols, cols) {
			diffs = append(diffs, Diff{
				Table:  table.Name,
				Kind:   IndexColumnMismatch,
				Detail: fmt.Sprintf("索引 %q 期望列顺序 (%s)，实际是 (%s)", idx.Name, colList, strings.Join(actualCols, ", ")),
				SuggestedSQL: fmt.Sprintf(
					"-- PG不支持直接修改已有索引的列，通常需要先DROP再重建（注意重建期间该索引不可用，大表要评估锁的影响）:\nDROP INDEX %s;\nCREATE %sINDEX %s ON %s (%s);",
					idx.Name, uniqueKw, idx.Name, table.Name, colList),
			})
		}
	}

	// PK会自带一个同名的唯一索引，比对多余索引时要把它排除掉，否则每次都会被误报
	pkIndexName := table.PrimaryKey.Name
	var extra []string
	for name := range actualNames {
		if !expected[name] && name != pkIndexName && !strings.HasSuffix(name, "_pkey") {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		diffs = append(diffs, Diff{
			Table:  table.Name,
			Kind:   ExtraIndex,
			Detail: fmt.Sprintf("数据库里存在索引 %q，但xml未声明（是否是遗留索引？需要人工确认）", name),
		})
	}

	return diffs, nil
}

// fetchIndexColumns 按索引名读取实际的列组成，顺序按索引定义中的顺序返回——
// 联合索引的顺序有意义，不能当成集合比较。
func fetchIndexColumns(ctx context.Context, db *sql.DB, pgSchema, indexName string) ([]string, error) {
	const q = `
		SELECT a.attname
		FROM pg_class t
		JOIN pg_namespace n ON n.oid = t.relnamespace
		JOIN pg_index ix ON ix.indrelid = t.oid
		JOIN pg_class i ON i.oid = ix.indexrelid
		JOIN unnest(ix.indkey::int2[]) WITH ORDINALITY AS k(attnum, ord) ON true
		JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = k.attnum
		WHERE n.nspname = $1 AND i.relname = $2
		ORDER BY k.ord`
	rows, err := db.QueryContext(ctx, q, pgSchema, indexName)
	if err != nil {
		return nil, fmt.Errorf("读取索引 %q 的列组成: %w", indexName, err)
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// camelToSnake 与 gen 包内的实现保持一致的转换规则
func camelToSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r - 'A' + 'a')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
