package dbpool

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"pdbgen/dbctx"
	"pdbgen/ptable"
)

// 基准要连 pdb.xml 里的库，结果主要是往返时间；分配数（allocs/op）和库无关，是客户端的开销。
// 数字记在 docs/design.md 的"开销"里：go test ./dbpool -run '^$' -bench . -benchmem

func benchProcedure(b *testing.B, showSQL bool, logger *slog.Logger, lock func(uid int64) LockSpec) {
	if testInitErr != nil {
		b.Skip(testInitErr)
	}
	cfg := SchemaConfig(testSchema)
	cfg.ShowSQL = showSQL
	Close()
	if err := Open(context.Background(), cfg); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		Close()
		if err := Init(context.Background(), testSchema); err != nil {
			b.Error(err)
		}
	})
	if logger != nil {
		old := sqlLog.Load()
		SetLogger(logger)
		b.Cleanup(func() { SetLogger(old) })
	}

	ctx := b.Context()
	uid := time.Now().UnixNano() + 990
	var id int64
	if err := WithTx(ctx, func(ctx context.Context) error {
		v := ptable.NewRedEnvelope()
		v.SetUid(uid)
		v.SetActId(99)
		ptable.UserRedEnvelopeTable.Insert(ctx, v)
		id = v.Id()
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		_ = WithTx(context.Background(), func(ctx context.Context) error {
			_, err := dbctx.SQLTxFromCtx(ctx).Exec(ctx, `DELETE FROM user_red_envelope WHERE uid = $1`, uid)
			return err
		})
	})

	opts := ProcedureOptions{}
	if lock != nil {
		opts.Lock = lock(uid)
	}
	var n int32
	b.ReportAllocs()
	for b.Loop() {
		n++
		if err := Procedure(ctx, opts, func(ctx context.Context) error {
			ptable.UserRedEnvelopeTable.GetById(ctx, id).SetTotal(n)
			return nil
		}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkProcedureGetSet 最小的一次 Procedure：BEGIN（带 SET LOCAL）、GetById、SetTotal、flush 一条 UPDATE、COMMIT，
// 四次往返。
func BenchmarkProcedureGetSet(b *testing.B) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	b.Run("showSQL=false", func(b *testing.B) { benchProcedure(b, false, nil, nil) })
	// showSQL 打开但 handler 只收 Error：Info 级的 SQL 日志不会输出，也不该付格式化的开销
	b.Run("showSQL=true,level=Error", func(b *testing.B) { benchProcedure(b, true, quiet, nil) })
	// 玩家请求的写法：先拿 uid 的 try 锁，多一次往返
	b.Run("TryAdvisoryLock", func(b *testing.B) {
		benchProcedure(b, false, nil, func(uid int64) LockSpec { return TryAdvisoryLock(uid) })
	})
}

// BenchmarkFormatSQL SQL 日志格式化一条语句的开销（不连库）。
func BenchmarkFormatSQL(b *testing.B) {
	const q = `UPDATE "public"."user_red_envelope" SET "today_count" = $1, "total" = $2 WHERE "id" = $3`
	args := []any{int32(3), int32(4), int64(434)}
	b.ReportAllocs()
	for b.Loop() {
		_ = formatSQL(q, args)
	}
}
