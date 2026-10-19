package dbctx

import (
	"context"
	"testing"
)

type testRecord struct {
	table string
	id    int64
}

func (r *testRecord) TableName() string          { return r.table }
func (r *testRecord) Identity() int64            { return r.id }
func (r *testRecord) FlushStmt() (string, []any) { return "", nil }
func (r *testRecord) FlushDone(int64) error      { return nil }
func (r *testRecord) SnapshotState() RecordState { return *r }
func (r *testRecord) RestoreState(state RecordState) {
	*r = state.(testRecord)
}
func (r *testRecord) ResetNew() { r.id = 0 }

type otherRecord struct {
	testRecord
}

type testKey struct {
	table string
	id    int64
}

type testTransaction struct {
	records map[testKey]Flusher
}

func (t *testTransaction) GetCachedRecord(tableName string, id int64) (Flusher, bool) {
	rec, ok := t.records[testKey{table: tableName, id: id}]
	return rec, ok
}

func (t *testTransaction) AddCachedRecord(obj Flusher) Flusher {
	key := testKey{table: obj.TableName(), id: obj.Identity()}
	if existing, ok := t.records[key]; ok {
		return existing
	}
	t.records[key] = obj
	return obj
}

func (t *testTransaction) RegisterNew(Flusher) {}

func (t *testTransaction) Rollback() {}

func (t *testTransaction) AfterCommit(func(context.Context))   {}
func (t *testTransaction) AfterRollback(func(context.Context)) {}

func TestGetCachedRecordAsMissWithoutTransaction(t *testing.T) {
	if rec, ok := GetCachedRecordAs[*testRecord](context.Background(), "t", 1); ok || rec != nil {
		t.Fatalf("没有事务时不应命中，实际 rec=%v ok=%v", rec, ok)
	}
}

func TestGetCachedRecordAsHit(t *testing.T) {
	obj := &testRecord{table: "t", id: 1}
	txn := &testTransaction{records: map[testKey]Flusher{
		{table: "t", id: 1}: obj,
	}}
	ctx := WithTransaction(context.Background(), txn)

	rec, ok := GetCachedRecordAs[*testRecord](ctx, "t", 1)
	if !ok || rec != obj {
		t.Fatalf("应命中同一个对象，实际 rec=%p ok=%v", rec, ok)
	}
}

func TestGetCachedRecordAsTypeMismatch(t *testing.T) {
	obj := &otherRecord{testRecord{table: "t", id: 1}}
	txn := &testTransaction{records: map[testKey]Flusher{
		{table: "t", id: 1}: obj,
	}}
	ctx := WithTransaction(context.Background(), txn)

	defer func() {
		if recover() == nil {
			t.Fatal("类型不一致时应该 panic")
		}
	}()

	GetCachedRecordAs[*testRecord](ctx, "t", 1)
}
