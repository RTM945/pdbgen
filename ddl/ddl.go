// Package ddl 从 schema.PDB 生成建表DDL（CREATE TABLE + 主键 + 索引），
// 并提供一个"仅当表在数据库里还不存在时才执行"的建表函数。
//
// 这里的边界很明确，和 schemacheck 的"只读、只建议"原则是互补而不是矛盾的：
//   - 表已经存在 -> 一律不碰，改动交给人看着 schemacheck 的建议手动执行（风险在于可能有数据）
//   - 表完全不存在 -> 直接建，因为这张表里还没有任何数据，"建错了"的代价
//     只是删掉重来，不存在数据丢失的风险，这才是这里允许自动执行的前提
package ddl

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"pdbgen/schema"
)

// TableDDL 是一张表的建表语句，拆成三段（和之前手写的DDL风格一致）：
// CREATE TABLE本体、追加自增+主键的ALTER TABLE、以及若干条CREATE INDEX。
type TableDDL struct {
	TableName       string
	CreateTable     string
	AlterPrimaryKey string
	CreateIndexes   []string
}

// Statements 按"必须先建表、再加主键、再建索引"的依赖顺序返回全部语句
func (t *TableDDL) Statements() []string {
	stmts := []string{t.CreateTable, t.AlterPrimaryKey}
	return append(stmts, t.CreateIndexes...)
}

// String 把三段拼成一份可读的完整DDL文本，用于打印/存进.sql文件/代码审查
func (t *TableDDL) String() string {
	var b strings.Builder
	b.WriteString(t.CreateTable)
	b.WriteString("\n\n")
	b.WriteString(t.AlterPrimaryKey)
	b.WriteString("\n\n")
	for i, idx := range t.CreateIndexes {
		b.WriteString(idx)
		if i < len(t.CreateIndexes)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// Generate 为pdb里的每一张表生成DDL
func Generate(pdb *schema.PDB) ([]*TableDDL, error) {
	var result []*TableDDL
	for _, table := range pdb.Tables {
		bean := pdb.FindBean(table.Bean)
		if bean == nil {
			return nil, fmt.Errorf("table %q 引用了不存在的 bean %q", table.Name, table.Bean)
		}
		td, err := generateTable(bean, table)
		if err != nil {
			return nil, fmt.Errorf("table %q: %w", table.Name, err)
		}
		result = append(result, td)
	}
	return result, nil
}

func generateTable(bean *schema.Bean, table schema.Table) (*TableDDL, error) {
	pkCol := schema.ColumnName(table.PrimaryKey.Variable)

	// 列宽对齐，纯粹是为了生成的DDL读起来和手写的一样整齐，不影响语义
	maxColLen := 0
	type colDef struct {
		name   string
		pgType string
	}
	var cols []colDef
	for _, v := range bean.Variables {
		pgType, err := schema.PGType(v.Type)
		if err != nil {
			return nil, fmt.Errorf("bean %q 字段 %q: %w", bean.Name, v.Name, err)
		}
		col := schema.ColumnName(v.Name)
		cols = append(cols, colDef{name: col, pgType: pgType})
		if len(col) > maxColLen {
			maxColLen = len(col)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE IF NOT EXISTS %s (\n", table.Name)
	for i, c := range cols {
		comma := ","
		if i == len(cols)-1 {
			comma = ""
		}
		fmt.Fprintf(&b, "    %-*s %s NOT NULL%s\n", maxColLen, c.name, c.pgType, comma)
	}
	b.WriteString(");")
	createTable := b.String()

	alterPK := fmt.Sprintf(
		"ALTER TABLE %s\n    ALTER COLUMN %s ADD GENERATED ALWAYS AS IDENTITY,\n    ADD CONSTRAINT %s PRIMARY KEY (%s);",
		table.Name, pkCol, table.PrimaryKey.Name, pkCol)
	if !table.PrimaryKey.AutoIncrement {
		// 非自增主键不需要IDENTITY这一步，只加约束
		alterPK = fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s PRIMARY KEY (%s);",
			table.Name, table.PrimaryKey.Name, pkCol)
	}

	var indexes []string
	for _, idx := range table.Indexes {
		vars := idx.Variables()
		cols := make([]string, len(vars))
		for i, v := range vars {
			cols[i] = schema.ColumnName(v)
		}
		uniqueKw := ""
		if idx.Unique {
			uniqueKw = "UNIQUE "
		}
		indexes = append(indexes, fmt.Sprintf("CREATE %sINDEX %s ON %s (%s);",
			uniqueKw, idx.Name, table.Name, strings.Join(cols, ",")))
	}

	return &TableDDL{
		TableName:       table.Name,
		CreateTable:     createTable,
		AlterPrimaryKey: alterPK,
		CreateIndexes:   indexes,
	}, nil
}

// ---- 建表（仅当表不存在时才执行）----

// DB 是Bootstrap需要的最小接口，*pgxpool.Pool 和 pgx.Tx 都天然满足
type DB interface {
	QueryRow(ctx context.Context, sql string, args ...interface{}) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Action 描述Bootstrap对某张表实际做了什么
type Action string

const (
	ActionCreated       Action = "created"        // 表不存在，已按DDL建好
	ActionSkippedExists Action = "skipped_exists" // 表已存在，什么都没做（这是刻意的，不做任何ALTER）
)

// TableResult 是Bootstrap对单张表的处理结果
type TableResult struct {
	Table      string
	Action     Action
	Statements []string // 只有Action=created时才有内容，方便调用方记录日志
}

// Bootstrap 对pdb里的每一张表：不存在就按生成的DDL建（同一张表的CREATE TABLE+
// 主键+索引包在一个事务里，任何一步失败整体回滚，不会留下半成品表）；
// 已存在就跳过，绝不touch——表结构变更永远只走schemacheck建议+人工手动执行这条路，
// Bootstrap自始至终只做"从无到有"这一件事。
func Bootstrap(ctx context.Context, db DB, pdb *schema.PDB) ([]TableResult, error) {
	pgSchema := pdb.Schema
	if pgSchema == "" {
		pgSchema = "public"
	}

	tableDDLs, err := Generate(pdb)
	if err != nil {
		return nil, err
	}

	var results []TableResult
	for _, td := range tableDDLs {
		exists, err := tableExists(ctx, db, pgSchema, td.TableName)
		if err != nil {
			return results, fmt.Errorf("table %q: 检查是否存在失败: %w", td.TableName, err)
		}
		if exists {
			results = append(results, TableResult{Table: td.TableName, Action: ActionSkippedExists})
			continue
		}

		tx, err := db.Begin(ctx)
		if err != nil {
			return results, fmt.Errorf("table %q: 开启事务失败: %w", td.TableName, err)
		}

		stmts := td.Statements()
		var execErr error
		for _, stmt := range stmts {
			if _, execErr = tx.Exec(ctx, stmt); execErr != nil {
				execErr = fmt.Errorf("执行失败: %w\n语句: %s", execErr, stmt)
				break
			}
		}
		if execErr != nil {
			_ = tx.Rollback(ctx)
			return results, fmt.Errorf("table %q: %w", td.TableName, execErr)
		}
		if err := tx.Commit(ctx); err != nil {
			return results, fmt.Errorf("table %q: 提交事务失败: %w", td.TableName, err)
		}

		results = append(results, TableResult{Table: td.TableName, Action: ActionCreated, Statements: stmts})
	}
	return results, nil
}

func tableExists(ctx context.Context, db DB, pgSchema, table string) (bool, error) {
	const q = `SELECT EXISTS (
		SELECT 1 FROM information_schema.tables
		WHERE table_schema = $1 AND table_name = $2
	)`
	var exists bool
	if err := db.QueryRow(ctx, q, pgSchema, table).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}
