package sqldrv

// The transaction half of the adapter: Pool, which can start one, and Tx, which
// is one. The fake driver counts how each transaction ended, which is the thing
// a caller cannot see from outside and the thing that goes wrong.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/gsoultan/storm/runtime"
)

func TestPoolRunsThePortOnTheDB(t *testing.T) {
	d := &fakeDriver{cols: []string{"a"}, rows: [][]driver.Value{{int64(1)}}, affected: 1}
	db, done := open(t, d)
	defer done()
	p, ctx := NewPool(db), context.Background()

	rows, err := p.Query(ctx, "SELECT a FROM t", nil)
	if err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if n, err := p.Exec(ctx, "UPDATE t SET a = 1", nil); err != nil || n != 1 {
		t.Fatalf("Exec = %d, %v", n, err)
	}
	if _, err := p.CopyFrom(ctx, "t", []string{"a"}, &sliceSource{rows: [][]any{{int64(2)}}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Batch(ctx, []runtime.BatchOp{{SQL: "DELETE FROM t"}}, nil); err != nil {
		t.Fatal(err)
	}
	if len(d.stmts) != 4 {
		t.Errorf("the pool ran %d statements, want 4: %q", len(d.stmts), d.stmts)
	}
}

func TestStartTxCommitsOnTheSameTransaction(t *testing.T) {
	d := &fakeDriver{affected: 1}
	db, done := open(t, d)
	defer done()
	ctx := context.Background()

	tx, err := NewPool(db).StartTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO t VALUES (1)", nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if d.commits != 1 || d.rollbacks != 0 {
		t.Errorf("commits=%d rollbacks=%d, want 1 and 0", d.commits, d.rollbacks)
	}
}

// Committing twice is a bug and says so, in storm's words rather than
// database/sql's, so one errors.Is works for every adapter.
func TestASecondCommitIsErrTxDone(t *testing.T) {
	db, done := open(t, &fakeDriver{})
	defer done()
	ctx := context.Background()
	tx, err := NewPool(db).StartTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); !errors.Is(err, runtime.ErrTxDone) {
		t.Errorf("second commit = %v, want runtime.ErrTxDone", err)
	}
}

// And rolling back a finished transaction is not an error at all, which is what
// makes `defer tx.Rollback(ctx)` beside a commit the idiom.
func TestRollbackAfterCommitIsNil(t *testing.T) {
	d := &fakeDriver{}
	db, done := open(t, d)
	defer done()
	ctx := context.Background()
	tx, err := NewPool(db).StartTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Errorf("rollback after commit = %v, want nil", err)
	}
	if d.rollbacks != 0 {
		t.Errorf("a rollback reached the driver after the commit")
	}
}

// A refused start hands back a NIL interface, not an interface holding a nil
// Tx — the difference between an error the caller checks and a panic on the
// next line.
func TestStartTxRefusalIsANilTx(t *testing.T) {
	refused := errors.New("no transactions today")
	db, done := open(t, &fakeDriver{beginErr: refused})
	defer done()
	tx, err := NewPool(db).StartTx(context.Background())
	if !errors.Is(err, refused) {
		t.Errorf("err = %v, want the driver's", err)
	}
	if tx != nil {
		t.Errorf("tx = %#v, want a nil runtime.Tx", tx)
	}
}

// NewTx is the door for isolation levels: the caller begins with the options
// they want and wraps the result.
func TestNewTxWrapsACallersOwnTransaction(t *testing.T) {
	d := &fakeDriver{affected: 1}
	db, done := open(t, d)
	defer done()
	ctx := context.Background()
	st, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var tx runtime.Tx = NewTx(st)
	if _, err := tx.Exec(ctx, "SELECT 1", nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if d.rollbacks != 1 {
		t.Errorf("rollbacks=%d, want 1", d.rollbacks)
	}
	if !d.txOpts.ReadOnly {
		t.Error("the caller's read-only option did not reach the driver")
	}
}

// The reason Pool exists: runtime.InTx works over database/sql like any other
// adapter, committing on success and rolling back on failure.
func TestInTxOverDatabaseSQL(t *testing.T) {
	d := &fakeDriver{affected: 1}
	db, done := open(t, d)
	defer done()
	ctx := context.Background()

	if err := runtime.InTx(ctx, NewPool(db), func(ex runtime.Executor) error {
		_, err := ex.Exec(ctx, "INSERT INTO t VALUES (1)", nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if err := runtime.InTx(ctx, NewPool(db), func(runtime.Executor) error { return boom }); !errors.Is(err, boom) {
		t.Errorf("InTx = %v, want fn's error", err)
	}
	if d.commits != 1 || d.rollbacks != 1 {
		t.Errorf("commits=%d rollbacks=%d, want 1 and 1", d.commits, d.rollbacks)
	}
}
