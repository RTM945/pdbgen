// Package schema 定义 pdb.xml 的结构，并提供解析能力。
// 这里只描述"数据长什么样"，不涉及任何PG/Go代码生成的具体逻辑。
package schema

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

type PDB struct {
	XMLName xml.Name `xml:"pdb"`

	URL       string `xml:"url,attr"`       // PG连接串
	GenOutput string `xml:"genOutput,attr"` // 生成的Go代码输出目录
	Schema    string `xml:"schema,attr"`    // PG schema，不填默认 public（schemacheck等也按这个来查）

	PoolMaxConns          int `xml:"poolMaxConns,attr"`
	PoolMinConns          int `xml:"poolMinConns,attr"`
	PoolMaxConnLifetime   int `xml:"poolMaxConnLifetime,attr"`   // 秒
	PoolMaxConnIdleTime   int `xml:"poolMaxConnIdleTime,attr"`   // 秒
	PoolHealthCheckPeriod int `xml:"poolHealthCheckPeriod,attr"` // 秒

	// 以下两个是建议新增的属性，原始XML没有，见下方“还需要注意的属性”说明
	StatementTimeoutMs                int    `xml:"statementTimeoutMs,attr"`
	IdleInTransactionSessionTimeoutMs int    `xml:"idleInTransactionSessionTimeoutMs,attr"`
	AppName                           string `xml:"appName,attr"`

	Beans  []Bean  `xml:"bean"`
	Tables []Table `xml:"table"`
}

type Bean struct {
	Name      string     `xml:"name,attr"`
	Variables []Variable `xml:"variable"`
}

// Variable 里的 Type 是Go类型（int64/string/time.Time...），
// 刻意不出现任何PG的东西——bean只负责描述Go侧的数据形状。
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
}

type Index struct {
	Name     string `xml:"name,attr"`
	Variable string `xml:"variable,attr"` // 单字段写 "token"；联合索引用逗号分隔，如 "id,token"
	Unique   bool   `xml:"unique,attr"`
}

// Variables 把 Variable 属性按逗号拆开，返回字段名列表（顺序即索引的列顺序，
// 联合索引的顺序很关键——决定了"最左前缀"规则下哪些查询能命中这个索引）
func (idx Index) Variables() []string {
	var vars []string
	for _, p := range strings.Split(idx.Variable, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			vars = append(vars, p)
		}
	}
	return vars
}

func Parse(r io.Reader) (*PDB, error) {
	var p PDB
	dec := xml.NewDecoder(r)
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("解析pdb.xml失败: %w", err)
	}
	p.applyDefaults()
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// applyDefaults 给没写的属性补上合理默认值，避免每个调用方都要自己判断"没配置怎么办"
func (p *PDB) applyDefaults() {
	if p.Schema == "" {
		p.Schema = "public"
	}
	if p.PoolMaxConns == 0 {
		p.PoolMaxConns = 20
	}
	if p.PoolMaxConnLifetime == 0 {
		p.PoolMaxConnLifetime = 3600
	}
	if p.PoolMaxConnIdleTime == 0 {
		p.PoolMaxConnIdleTime = 1800
	}
	if p.StatementTimeoutMs == 0 {
		p.StatementTimeoutMs = 5000
	}
	if p.IdleInTransactionSessionTimeoutMs == 0 {
		p.IdleInTransactionSessionTimeoutMs = 5000
	}
}

// FindBean 按名字取bean，找不到返回nil
func (p *PDB) FindBean(name string) *Bean {
	for i := range p.Beans {
		if p.Beans[i].Name == name {
			return &p.Beans[i]
		}
	}
	return nil
}

// FindVariable 按名字取bean里的字段
func (b *Bean) FindVariable(name string) *Variable {
	for i := range b.Variables {
		if b.Variables[i].Name == name {
			return &b.Variables[i]
		}
	}
	return nil
}

// validate 在解析阶段就把明显对不上的配置拦下来，
// 不要让generator在生成代码时才因为"bean引用不存在"这类问题崩溃，
// 报错信息也更直接地指向xml本身的问题。
func (p *PDB) validate() error {
	if p.URL == "" {
		return fmt.Errorf("pdb根节点缺少 url 属性")
	}
	for _, t := range p.Tables {
		bean := p.FindBean(t.Bean)
		if bean == nil {
			return fmt.Errorf("table %q 引用了不存在的 bean %q", t.Name, t.Bean)
		}
		if t.PrimaryKey == nil {
			return fmt.Errorf("table %q 没有声明 primaryKey", t.Name)
		}
		if bean.FindVariable(t.PrimaryKey.Variable) == nil {
			return fmt.Errorf("table %q 的 primaryKey 引用了 bean %q 中不存在的字段 %q",
				t.Name, bean.Name, t.PrimaryKey.Variable)
		}
		for _, idx := range t.Indexes {
			vars := idx.Variables()
			if len(vars) == 0 {
				return fmt.Errorf("table %q 的 index %q 没有声明任何variable", t.Name, idx.Name)
			}
			for _, vn := range vars {
				if bean.FindVariable(vn) == nil {
					return fmt.Errorf("table %q 的 index %q 引用了 bean %q 中不存在的字段 %q",
						t.Name, idx.Name, bean.Name, vn)
				}
			}
		}
	}
	return nil
}
