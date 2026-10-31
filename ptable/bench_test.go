package ptable

import "testing"

// BenchmarkSetAndFlush 实体自己的开销（不连库）：改一列、拼 UPDATE、清脏位。
func BenchmarkSetAndFlush(b *testing.B) {
	v := loadedRedEnvelope()
	var n int32
	b.ReportAllocs()
	for b.Loop() {
		n++
		v.SetTotal(n)
		if stmt, _ := v.FlushStmt(); stmt == "" {
			b.Fatal("应该有 UPDATE")
		}
		v.FlushDone()
	}
}

// BenchmarkSetUnchanged 写回同样的值：不置脏位，什么都不分配。
func BenchmarkSetUnchanged(b *testing.B) {
	v := loadedRedEnvelope()
	b.ReportAllocs()
	for b.Loop() {
		v.SetTotal(3)
	}
}
