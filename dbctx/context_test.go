package dbctx

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type testRecord struct {
	table    string
	id       int64
	dirty    uint64
	deleted  bool
	detached bool
}

func (r *testRecord) TableName() string              { return r.table }
func (r *testRecord) Identity() int64                { return r.id }
func (r *testRecord) FlushStmt() (string, []any)     { return "", nil }
func (r *testRecord) FlushDone()                     {}
func (r *testRecord) DirtyColumns() uint64           { return r.dirty }
func (r *testRecord) IsDeleted() bool                { return r.deleted }
func (r *testRecord) SnapshotState() RecordState     { return *r }
func (r *testRecord) RestoreState(state RecordState) { *r = state.(testRecord) }
func (r *testRecord) Detach()                        { r.detached = true }

type otherRecord struct {
	testRecord
}

type testKey struct {
	table string
	id    int64
}

type testTransaction struct {
	records map[testKey]Flusher
	items   []Flusher
	hooks   int
}

func newTestTransaction(recs ...Flusher) *testTransaction {
	txn := &testTransaction{records: make(map[testKey]Flusher)}
	for _, rec := range recs {
		txn.AddCachedRecord(rec)
	}
	return txn
}

func (t *testTransaction) SQLTx() SQLTx { return nil }

func (t *testTransaction) GetCachedRecord(table string, id int64) (Flusher, bool) {
	rec, ok := t.records[testKey{table: table, id: id}]
	return rec, ok
}

func (t *testTransaction) AddCachedRecord(obj Flusher) Flusher {
	key := testKey{table: obj.TableName(), id: obj.Identity()}
	if existing, ok := t.records[key]; ok {
		return existing
	}
	t.records[key] = obj
	t.items = append(t.items, obj)
	return obj
}

func (t *testTransaction) Records() []Flusher { return t.items }

func (t *testTransaction) AfterCommit(func(context.Context))   { t.hooks++ }
func (t *testTransaction) AfterRollback(func(context.Context)) { t.hooks++ }

// mustPanic 跑 fn，断言它 panic 且信息包含 want。
func mustPanic(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		t.Helper()
		r := recover()
		if r == nil {
			t.Fatalf("应该 panic（期望包含 %q）", want)
		}
		if msg, _ := r.(string); !strings.Contains(msg, want) {
			t.Fatalf("panic 信息不对，期望包含 %q，实际: %v", want, r)
		}
	}()
	fn()
}

func TestGetCachedRecordAsMissWithoutTransaction(t *testing.T) {
	if rec, ok := GetCachedRecordAs[*testRecord](context.Background(), "t", 1); ok || rec != nil {
		t.Fatalf("没有事务时不应命中，实际 rec=%v ok=%v", rec, ok)
	}
}

func TestGetCachedRecordAsHit(t *testing.T) {
	obj := &testRecord{table: "t", id: 1}
	ctx := WithTransaction(context.Background(), newTestTransaction(obj))

	rec, ok := GetCachedRecordAs[*testRecord](ctx, "t", 1)
	if !ok || rec != obj {
		t.Fatalf("应命中同一个对象，实际 rec=%p ok=%v", rec, ok)
	}
}

// TestGetCachedRecordAsDeleted 本事务标记删除的行：命中，但当作不存在。
func TestGetCachedRecordAsDeleted(t *testing.T) {
	ctx := WithTransaction(context.Background(), newTestTransaction(&testRecord{table: "t", id: 1, deleted: true}))

	rec, ok := GetCachedRecordAs[*testRecord](ctx, "t", 1)
	if !ok || rec != nil {
		t.Fatalf("已删除的行应命中且返回 nil，实际 rec=%v ok=%v", rec, ok)
	}
}

func TestGetCachedRecordAsTypeMismatch(t *testing.T) {
	obj := &otherRecord{testRecord{table: "t", id: 1}}
	ctx := WithTransaction(context.Background(), newTestTransaction(obj))

	defer func() {
		if recover() == nil {
			t.Fatal("类型不一致时应该 panic")
		}
	}()
	GetCachedRecordAs[*testRecord](ctx, "t", 1)
}

// TestGetOrAddCachedRecordDetachesOutsideTransaction 事务外查出来的实体标记为脱离事务。
func TestGetOrAddCachedRecordDetachesOutsideTransaction(t *testing.T) {
	obj := &testRecord{table: "t", id: 1}
	if got := GetOrAddCachedRecord(context.Background(), obj); got != obj || !obj.detached {
		t.Fatalf("事务外应原样返回并标记 detached，实际 got=%p detached=%v", got, obj.detached)
	}

	inTx := &testRecord{table: "t", id: 2}
	ctx := WithTransaction(context.Background(), newTestTransaction())
	if got := GetOrAddCachedRecord(ctx, inTx); got != inTx || inTx.detached {
		t.Fatalf("事务里应登记且不 detach，实际 got=%p detached=%v", got, inTx.detached)
	}
}

// TestCheckPredicate 同一张表有记录在查询条件列上改了还没落库时 panic，其他情况放行。
func TestCheckPredicate(t *testing.T) {
	const uid, actId, total = 1 << 0, 1 << 1, 1 << 2
	ctx := WithTransaction(context.Background(), newTestTransaction(
		&testRecord{table: "red", id: 1, dirty: actId},
		&testRecord{table: "mail", id: 2, dirty: uid},
	))

	mustPanic(t, "red id=1", func() { CheckPredicate(ctx, "red", uid|actId) })

	CheckPredicate(ctx, "red", uid|total)              // 改的列不在条件里
	CheckPredicate(ctx, "other", uid|actId)            // 别的表
	CheckPredicate(context.Background(), "red", actId) // 不在事务里
}

// TestTransactionOnlyHelpers 只能在事务里调用的入口，ctx 传错直接 panic。
func TestTransactionOnlyHelpers(t *testing.T) {
	ctx := context.Background()
	mustPanic(t, "dbctx.AfterCommit", func() { AfterCommit(ctx, func(context.Context) {}) })
	mustPanic(t, "dbctx.AfterRollback", func() { AfterRollback(ctx, func(context.Context) {}) })
	mustPanic(t, "dbpool.Procedure", func() { SQLTxFromCtx(ctx) })

	txn := newTestTransaction()
	AfterCommit(WithTransaction(ctx, txn), func(context.Context) {})
	AfterRollback(WithTransaction(ctx, txn), func(context.Context) {})
	if txn.hooks != 2 {
		t.Fatalf("事务里的钩子应该挂到事务上，实际 %d 个", txn.hooks)
	}
}

// TestQuerierFromCtx SelectByXxx 只能在 dbpool.Query 里、事务外调用。
func TestQuerierFromCtx(t *testing.T) {
	mustPanic(t, "dbpool.Query", func() { QuerierFromCtx(context.Background()) })

	inTx := WithTransaction(context.Background(), newTestTransaction())
	mustPanic(t, "不能在事务里调用", func() { QuerierFromCtx(inTx) })
}

// TestUpdateStmt 只拼脏列，主键是最后一个参数。
func TestUpdateStmt(t *testing.T) {
	stmt, args := UpdateStmt(`"t"`, `"id"`, 7, []string{`"a"`, `"b"`, `"c"`}, 1<<0|1<<2, "x", 2, "s")
	if want := `UPDATE "t" SET "a" = $1, "c" = $2 WHERE "id" = $3`; stmt != want {
		t.Fatalf("语句不符\n期望 %s\n实际 %s", want, stmt)
	}
	if len(args) != 3 || args[0] != "x" || args[1] != "s" || args[2] != int64(7) {
		t.Fatalf("参数不符，实际 %#v", args)
	}
}

// failingQuerier 的每条查询都失败，用来验证查询出错时走 abort。
type failingQuerier struct{ err error }

func (q failingQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return errRow{q.err} }
func (q failingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, q.err
}
func (q failingQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, q.err
}
func (q failingQuerier) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { return nil }

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

// abortOf 跑 fn，返回它以 abort 结束时带出来的错误；没有 panic 或者 panic 的不是 abort 时测试失败。
func abortOf(t *testing.T, fn func()) (err error) {
	t.Helper()
	defer func() {
		t.Helper()
		r := recover()
		if err = AbortError(r); err == nil {
			t.Fatalf("应该以 abort 结束，实际 panic: %v", r)
		}
	}()
	fn()
	return nil
}

func scanTestRecord(s Scanner) (*testRecord, error) {
	r := &testRecord{table: "t"}
	return r, s.Scan(&r.id)
}

// TestQueryErrorsAbort 查询出错时以 abort 结束，错误里带着原错误和缩写的语句。
func TestQueryErrorsAbort(t *testing.T) {
	boom := errors.New("boom")
	q := failingQuerier{boom}
	const query = `SELECT "id", "a", "b" FROM "t" WHERE "a" = $1`

	for name, fn := range map[string]func(){
		"QueryOne":  func() { QueryOne(context.Background(), q, query, scanTestRecord, 1) },
		"QueryList": func() { QueryList(context.Background(), q, query, scanTestRecord, 1) },
		"InsertReturning": func() {
			InsertReturning(context.Background(), q, `INSERT INTO "t" ("a") VALUES ($1) RETURNING "id"`, new(int64), 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := abortOf(t, fn)
			if !errors.Is(err, boom) {
				t.Fatalf("abort 里应该带着原错误，实际: %v", err)
			}
			if !strings.HasPrefix(err.Error(), "pdbgen: ") || strings.Contains(err.Error(), `"a", "b"`) {
				t.Fatalf("错误应该带缩写的语句，实际: %v", err)
			}
		})
	}
}

// TestQueryOneNoRowsIsNotAnError 查不到返回零值，不是错误。
func TestQueryOneNoRowsIsNotAnError(t *testing.T) {
	if got := QueryOne(context.Background(), failingQuerier{pgx.ErrNoRows}, "SELECT 1", scanTestRecord); got != nil {
		t.Fatalf("查不到应该返回 nil，实际 %v", got)
	}
}

// TestAbortErrorIgnoresOtherPanics 只有 abort 才转成返回值：业务 panic、误用的 panic 照旧上抛。
func TestAbortErrorIgnoresOtherPanics(t *testing.T) {
	for _, r := range []any{nil, "boom", errors.New("plain error"), &testRecord{}} {
		if err := AbortError(r); err != nil {
			t.Fatalf("%v 不是 abort，实际取出了 %v", r, err)
		}
	}
}

func TestSummary(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`SELECT "id", "uid" FROM "user_mail" WHERE "uid" = $1`, `SELECT ... FROM "user_mail" WHERE "uid" = $1`},
		{`INSERT INTO "user_mail" ("uid") VALUES ($1) RETURNING "id"`, `INSERT INTO "user_mail"`},
		{`DELETE FROM "t" WHERE "id" = $1`, `DELETE FROM "t" WHERE "id" = $1`},
	} {
		if got := summary(tc.in); got != tc.want {
			t.Errorf("summary(%q)\n期望 %s\n实际 %s", tc.in, tc.want, got)
		}
	}
}
