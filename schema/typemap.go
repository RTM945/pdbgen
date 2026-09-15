package schema

import "fmt"

// goToPG 只登记当前实际用到的类型。
// 遇到未登记的类型直接报错，而不是猜一个"看起来差不多"的默认类型——
// 类型映射错了很难在运行时发现，宁可在生成阶段就卡住，逼着人显式补充。
var goToPG = map[string]string{
	"int32":     "INTEGER",
	"int64":     "BIGINT",
	"string":    "TEXT",
	"bool":      "BOOLEAN",
	"float64":   "DOUBLE PRECISION",
	"time.Time": "TIMESTAMPTZ",
}

// PGType 返回Go类型对应的PG列类型
func PGType(goType string) (string, error) {
	t, ok := goToPG[goType]
	if !ok {
		return "", fmt.Errorf("未知的Go类型 %q，请先在 schema/typemap.go 的 goToPG 中补充对应的PG类型", goType)
	}
	return t, nil
}
