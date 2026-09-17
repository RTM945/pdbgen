package schema

import (
	"fmt"
	"strings"
	"unicode"
)

// goToPG 只登记当前实际用到的类型。
// 遇到未登记的类型直接报错，而不是猜一个"看起来差不多"的默认类型——
// 类型映射错了很难在运行时发现，宁可在生成阶段就卡住，逼着人显式补充。
var goToPG = map[string]string{
	"int32":  "INTEGER",
	"int64":  "BIGINT",
	"uint64": "BIGINT", // 注意：PG无无符号类型，可用范围被砍半，自增主键场景可放心用，
	// 若某字段业务上真的可能超过 2^63-1，需要改用 NUMERIC，不能直接套这个映射
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

// ColumnName 把bean字段名（驼峰）转成PG列名习惯（蛇形），如 lastLoginAt -> last_login_at。
// gen/schemacheck/ddl 三个包都要用同一套命名规则，统一放在这里，不要各自维护一份。
func ColumnName(varName string) string {
	var b strings.Builder
	for i, r := range varName {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
