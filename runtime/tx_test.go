package runtime

// InTx has three ways out and hand-written versions of it usually handle two.
// These are the three, plus the two failures that happen at the edges of the
// transaction rather than inside it.

import (
	"context"
	"errors"
	"testing"
)

// txStub records what was called on it. It is the whole port plus the two
// endings, because runtime.Tx is the whole port plus the two endings.
type txStub struct {
	commits   int
	rollbacks int
	commitErr error
	rollErr   error
	done      bool
}

func (t *txStub) Query(context.Context, string, []any) (Rows, error) { return nil, nil }
func (t *txStub) Exec(context.Context, string, []any) (int64, error) { return 0, nil }
func (t *txStub) CopyFrom(context.Context, string, []string, CopySource) (int64, error) {
	return 0, nil
}
func (t *txStub) Batch(context.Context, []BatchOp, func(int, Rows, int64, error) error) error {
	return nil
}

func (t *txStub) Commit(context.Context) error {
	t.commits++
	if t.done {
		return ErrTxDone
	}
	t.done = true
	return t.commitErr
}

func (t *txStub) Rollback(context.Context) error {
	t.rollbacks++
	if t.done {
		return nil
	}
	t.done = true
	return t.rollErr
}

type dbStub struct {
	tx       *txStub
	beginErr error
	begins   int
}

func (d *dbStub) Query(context.Context, string, []any) (Rows, error) { return nil, nil }
func (d *dbStub) Exec(context.Context, string, []any) (int64, error) { return 0, nil }
func (d *dbStub) CopyFrom(context.Context, string, []string, CopySource) (int64, error) {
	return 0, nil
}
func (d *dbStub) Batch(context.Context, []BatchOp, func(int, Rows, int64, error) error) error {
	return nil
}

func (d *dbStub) Begin(context.Context) (Tx, error) {
	d.begins++
	if d.beginErr != nil {
		return nil, d.beginErr
	}
	return d.tx, nil
}

var (
	_ Tx = (*txStub)(nil)
	_ DB = (*dbStub)(nil)
)

func TestInTxCommitsOnSuccess(t *testing.T) {
	tx := &txStub{}
	db := &dbStub{tx: tx}

	var got Executor
	if err := InTx(t.Context(), db, func(ex Executor) error {
		got = ex
		return nil
	}); err != nil {
		t.Fatalf("InTx: %v", err)
	}

	if tx.commits != 1 {
		t.Errorf("commits = %d, want 1", tx.commits)
	}
	// Not "rollbacks == 0 or 1". A rollback after a successful commit is a
	// second round trip on a connection that is finished with, and the
	// committed flag exists to prevent exactly that.
	if tx.rollbacks != 0 {
		t.Errorf("rollbacks = %d, want 0 — the deferred rollback fired after a good commit", tx.rollbacks)
	}
	if got != Tx(tx) {
		t.Error("fn was handed something other than the transaction")
	}
}

func TestInTxRollsBackOnError(t *testing.T) {
	tx := &txStub{}
	db := &dbStub{tx: tx}
	boom := errors.New("boom")

	err := InTx(t.Context(), db, func(Executor) error { return boom })

	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}
	if tx.rollbacks != 1 {
		t.Errorf("rollbacks = %d, want 1", tx.rollbacks)
	}
	if tx.commits != 0 {
		t.Errorf("commits = %d, want 0", tx.commits)
	}
}

// The failing rollback is the case that decides whether a caller reading the
// error finds the cause or the symptom. A broken connection reports both.
func TestInTxRollbackErrorDoesNotMaskTheCause(t *testing.T) {
	cause := errors.New("the constraint that actually failed")
	tx := &txStub{rollErr: errors.New("connection reset")}
	db := &dbStub{tx: tx}

	err := InTx(t.Context(), db, func(Executor) error { return cause })

	if !errors.Is(err, cause) {
		t.Errorf("err = %v, want the cause %v", err, cause)
	}
}

// A panicking fn must not leave the transaction open with its connection
// checked out, and must not have its panic value swallowed either.
func TestInTxRollsBackOnPanicAndRepanics(t *testing.T) {
	tx := &txStub{}
	db := &dbStub{tx: tx}

	func() {
		defer func() {
			p := recover()
			if p != "the original value" {
				t.Errorf("recovered %v, want the original panic value", p)
			}
		}()
		_ = InTx(t.Context(), db, func(Executor) error { panic("the original value") })
		t.Error("InTx returned instead of re-panicking")
	}()

	if tx.rollbacks != 1 {
		t.Errorf("rollbacks = %d, want 1 — a panic left the transaction open", tx.rollbacks)
	}
	if tx.commits != 0 {
		t.Errorf("commits = %d, want 0", tx.commits)
	}
}

func TestInTxReturnsTheBeginError(t *testing.T) {
	refused := errors.New("pool exhausted")
	db := &dbStub{beginErr: refused}

	called := false
	err := InTx(t.Context(), db, func(Executor) error { called = true; return nil })

	if !errors.Is(err, refused) {
		t.Errorf("err = %v, want %v", err, refused)
	}
	if called {
		t.Error("fn ran without a transaction")
	}
}

// A commit that fails has still ENDED the transaction, so the deferred
// rollback must not fire behind it.
func TestInTxReturnsTheCommitError(t *testing.T) {
	failed := errors.New("could not serialize access")
	tx := &txStub{commitErr: failed}
	db := &dbStub{tx: tx}

	err := InTx(t.Context(), db, func(Executor) error { return nil })

	if !errors.Is(err, failed) {
		t.Errorf("err = %v, want %v", err, failed)
	}
	if tx.rollbacks != 0 {
		t.Errorf("rollbacks = %d, want 0 after a failed commit", tx.rollbacks)
	}
}
