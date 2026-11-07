package hooks

import (
	"context"
	"errors"
	"testing"

	"github.com/gsoultan/storm/runtime"
)

// fakeRows yields n empty rows.
type fakeRows struct {
	n, i   int
	closed int
	err    error
}

func (r *fakeRows) Next() bool          { r.i++; return r.i <= r.n }
func (r *fakeRows) RawValues() [][]byte { return nil }
func (r *fakeRows) Close()              { r.closed++ }
func (r *fakeRows) Err() error          { return r.err }

type fakeValueRows struct{ fakeRows }

func (r *fakeValueRows) Values() []any { return []any{int64(1)} }

type ctxKey struct{}

// fake is the port, recording the context each call ran with.
type fake struct {
	rows    runtime.Rows
	err     error
	sawCtx  []any
	queries int
}

func (f *fake) Query(ctx context.Context, sql string, args []any) (runtime.Rows, error) {
	f.queries++
	f.sawCtx = append(f.sawCtx, ctx.Value(ctxKey{}))
	return f.rows, f.err
}
func (f *fake) Exec(ctx context.Context, sql string, args []any) (int64, error) {
	f.sawCtx = append(f.sawCtx, ctx.Value(ctxKey{}))
	return 3, f.err
}
func (f *fake) CopyFrom(ctx context.Context, table string, cols []string, src runtime.CopySource) (int64, error) {
	return 7, f.err
}
func (f *fake) Batch(ctx context.Context, ops []runtime.BatchOp, each func(int, runtime.Rows, int64, error) error) error {
	return f.err
}

type fakeTx struct {
	fake
	commits, rollbacks int
}

func (t *fakeTx) Commit(context.Context) error   { t.commits++; return nil }
func (t *fakeTx) Rollback(context.Context) error { t.rollbacks++; return nil }

type fakeDB struct {
	fake
	tx *fakeTx
}

func (d *fakeDB) StartTx(context.Context) (runtime.Tx, error) { return d.tx, nil }

// recorder collects what the hooks saw.
type recorder struct {
	before, after []Statement
}

func (r *recorder) hooks() Hooks {
	return Hooks{
		Before: func(ctx context.Context, s *Statement) (context.Context, error) {
			r.before = append(r.before, *s)
			return context.WithValue(ctx, ctxKey{}, "span"), nil
		},
		After: func(ctx context.Context, s *Statement) {
			if ctx.Value(ctxKey{}) != "span" {
				panic("After did not get Before's context")
			}
			r.after = append(r.after, *s)
		},
	}
}

func TestWrapWithNoHooksIsTheExecutorItself(t *testing.T) {
	f := &fake{}
	if got := Wrap(f, Hooks{}); got != runtime.Executor(f) {
		t.Fatalf("Wrap with no hooks returned %T, want the executor unchanged", got)
	}
	d := &fakeDB{}
	if got := WrapDB(d, Hooks{}); got != runtime.DB(d) {
		t.Fatalf("WrapDB with no hooks returned %T", got)
	}
	tx := &fakeTx{}
	if got := WrapTx(tx, Hooks{}); got != runtime.Tx(tx) {
		t.Fatalf("WrapTx with no hooks returned %T", got)
	}
}

func TestQueryAfterRunsOnCloseWithTheRowCount(t *testing.T) {
	var rec recorder
	f := &fake{rows: &fakeRows{n: 3}}
	ex := Wrap(f, rec.hooks())

	rows, err := ex.Query(context.Background(), "SELECT 1", []any{1})
	if err != nil {
		t.Fatal(err)
	}
	if f.sawCtx[0] != "span" {
		t.Fatal("the driver did not run with Before's context")
	}
	if len(rec.before) != 1 || rec.before[0].Kind != KindQuery || rec.before[0].SQL != "SELECT 1" {
		t.Fatalf("before = %+v", rec.before)
	}
	for rows.Next() {
	}
	if len(rec.after) != 0 {
		t.Fatal("After ran before the result was closed")
	}
	rows.Close()
	rows.Close()
	if len(rec.after) != 1 {
		t.Fatalf("After ran %d times, want once however often Close is called", len(rec.after))
	}
	if a := rec.after[0]; a.Rows != 3 || a.Err != nil || a.Start.IsZero() {
		t.Fatalf("after = %+v", a)
	}
}

func TestQueryKeepsTheValueShape(t *testing.T) {
	var rec recorder
	ex := Wrap(&fake{rows: &fakeValueRows{fakeRows{n: 1}}}, rec.hooks())
	vr, err := runtime.AsValueRows(ex.Query(context.Background(), "SELECT 1", nil))
	if err != nil {
		t.Fatalf("a wrapped value-shaped result lost its shape: %v", err)
	}
	vr.Next()
	if v := vr.Values(); len(v) != 1 {
		t.Fatalf("Values = %v", v)
	}
	vr.Close()
	if len(rec.after) != 1 || rec.after[0].Rows != 1 {
		t.Fatalf("after = %+v", rec.after)
	}
}

func TestQueryErrorRunsAfterAtOnce(t *testing.T) {
	var rec recorder
	boom := errors.New("boom")
	ex := Wrap(&fake{err: boom}, rec.hooks())
	if _, err := ex.Query(context.Background(), "SELECT 1", nil); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if len(rec.after) != 1 || !errors.Is(rec.after[0].Err, boom) {
		t.Fatalf("after = %+v", rec.after)
	}
}

func TestBeforeErrorSendsNothing(t *testing.T) {
	refuse := errors.New("read-only replica")
	f := &fake{rows: &fakeRows{}}
	afters := 0
	ex := Wrap(f, Hooks{
		Before: func(ctx context.Context, s *Statement) (context.Context, error) { return ctx, refuse },
		After:  func(context.Context, *Statement) { afters++ },
	})
	if _, err := ex.Query(context.Background(), "SELECT 1", nil); !errors.Is(err, refuse) {
		t.Fatalf("err = %v", err)
	}
	if _, err := ex.Exec(context.Background(), "DELETE", nil); !errors.Is(err, refuse) {
		t.Fatalf("err = %v", err)
	}
	if _, err := ex.CopyFrom(context.Background(), "t", nil, nil); !errors.Is(err, refuse) {
		t.Fatalf("err = %v", err)
	}
	if err := ex.Batch(context.Background(), nil, nil); !errors.Is(err, refuse) {
		t.Fatalf("err = %v", err)
	}
	if f.queries != 0 || len(f.sawCtx) != 0 || afters != 0 {
		t.Fatalf("a refused statement reached the driver (%d) or After (%d)", len(f.sawCtx), afters)
	}
}

func TestExecCopyBatchReportOutcome(t *testing.T) {
	var rec recorder
	ex := Wrap(&fake{}, rec.hooks())
	ctx := context.Background()
	ops := []runtime.BatchOp{{SQL: "A"}, {SQL: "B"}}
	if _, err := ex.Exec(ctx, "UPDATE", []any{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.CopyFrom(ctx, "t", []string{"a"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := ex.Batch(ctx, ops, nil); err != nil {
		t.Fatal(err)
	}
	want := []struct {
		k    Kind
		rows int64
	}{{KindExec, 3}, {KindCopy, 7}, {KindBatch, 0}}
	if len(rec.after) != len(want) {
		t.Fatalf("after = %+v", rec.after)
	}
	for i, w := range want {
		if a := rec.after[i]; a.Kind != w.k || a.Rows != w.rows {
			t.Errorf("after[%d] = %v/%d, want %v/%d", i, a.Kind, a.Rows, w.k, w.rows)
		}
	}
	if rec.after[1].Table != "t" || len(rec.after[2].Ops) != 2 {
		t.Fatalf("copy/batch fields missing: %+v", rec.after)
	}
}

func TestWrapKeepsTxAndDBShape(t *testing.T) {
	var rec recorder
	tx := &fakeTx{}
	d := &fakeDB{tx: tx}

	if _, ok := Wrap(d, rec.hooks()).(runtime.DB); !ok {
		t.Fatal("a wrapped DB is no longer a DB")
	}
	if _, ok := Wrap(tx, rec.hooks()).(runtime.Tx); !ok {
		t.Fatal("a wrapped Tx is no longer a Tx")
	}

	err := runtime.InTx(context.Background(), WrapDB(d, rec.hooks()), func(ex runtime.Executor) error {
		_, err := ex.Exec(context.Background(), "INSERT", nil)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if tx.commits != 1 {
		t.Fatalf("commits = %d", tx.commits)
	}
	if len(rec.after) != 1 || rec.after[0].SQL != "INSERT" {
		t.Fatalf("a statement inside the transaction was not seen: %+v", rec.after)
	}
}

func TestOnlyAfter(t *testing.T) {
	var got []Statement
	ex := Wrap(&fake{rows: &fakeRows{n: 2}}, Hooks{After: func(_ context.Context, s *Statement) { got = append(got, *s) }})
	rows, err := ex.Query(context.Background(), "SELECT", nil)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	rows.Close()
	if len(got) != 1 || got[0].Rows != 2 {
		t.Fatalf("got = %+v", got)
	}
}

func TestOnlyBeforeDoesNotWrapRows(t *testing.T) {
	inner := &fakeRows{}
	ex := Wrap(&fake{rows: inner}, Hooks{Before: func(ctx context.Context, _ *Statement) (context.Context, error) { return nil, nil }})
	rows, err := ex.Query(context.Background(), "SELECT", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rows != runtime.Rows(inner) {
		t.Fatalf("with no After the result should be the driver's own, got %T", rows)
	}
}

func TestKindString(t *testing.T) {
	for k, want := range map[Kind]string{KindQuery: "query", KindExec: "exec", KindCopy: "copy", KindBatch: "batch", 0: "unknown", 99: "unknown"} {
		if got := k.String(); got != want {
			t.Errorf("Kind(%d) = %q, want %q", k, got, want)
		}
	}
}
