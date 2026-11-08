package hooks

import (
	"context"
	"errors"
	"testing"

	"github.com/gsoultan/storm/runtime"
)

func TestApply(t *testing.T) {
	ctx := context.Background()
	v := 1
	if err := Apply[int](ctx, nil, nil, &v); err != nil || v != 1 {
		t.Fatalf("an unset hook changed something: v=%d err=%v", v, err)
	}
	boom := errors.New("boom")
	h := func(_ context.Context, _ runtime.Executor, p *int) error { *p = 2; return boom }
	if err := Apply(ctx, nil, h, &v); !errors.Is(err, boom) || v != 2 {
		t.Fatalf("v=%d err=%v; the hook's change and its error must both come back", v, err)
	}
}

// With no hook set, Apply must not move its argument to the heap — that is
// the reason it exists.
func TestApplyUnsetDoesNotAllocate(t *testing.T) {
	ctx := context.Background()
	var h func(context.Context, runtime.Executor, *[64]byte) error
	n := testing.AllocsPerRun(100, func() {
		var v [64]byte
		_ = Apply(ctx, nil, h, &v)
	})
	if n != 0 {
		t.Fatalf("Apply with no hook allocated %v times per call", n)
	}
}

func TestWrappedTxAndDBPassThrough(t *testing.T) {
	var rec recorder
	ctx := context.Background()
	inner := &fakeTx{fake: fake{rows: &fakeRows{n: 1, err: nil}}}
	tx := WrapTx(inner, rec.hooks())
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if inner.commits != 1 || inner.rollbacks != 1 {
		t.Fatalf("commits=%d rollbacks=%d", inner.commits, inner.rollbacks)
	}

	rows, err := tx.Query(ctx, "SELECT", nil)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		_ = rows.RawValues()
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if len(rec.after) != 1 {
		t.Fatalf("after = %+v", rec.after)
	}
}

type failingDB struct{ fakeDB }

var errStart = errors.New("no connection")

func (d *failingDB) StartTx(context.Context) (runtime.Tx, error) { return nil, errStart }

func TestStartTxErrorPassesThrough(t *testing.T) {
	var rec recorder
	d := WrapDB(&failingDB{}, rec.hooks())
	if _, err := d.StartTx(context.Background()); !errors.Is(err, errStart) {
		t.Fatalf("err = %v", err)
	}
}

func TestRowsErrReachesAfter(t *testing.T) {
	var rec recorder
	bad := errors.New("conn reset")
	ex := Wrap(&fake{rows: &fakeRows{n: 1, err: bad}}, rec.hooks())
	rows, err := ex.Query(context.Background(), "SELECT", nil)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	rows.Close()
	if len(rec.after) != 1 || !errors.Is(rec.after[0].Err, bad) || rec.after[0].Rows != 1 {
		t.Fatalf("after = %+v", rec.after)
	}
}
