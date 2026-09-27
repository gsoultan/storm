package mydrv_test

// StartTx, against a real server: the runtime.DB spelling of Begin.
//
// Begin kept the *Tx it shipped with in v1.1.0, and StartTx wraps it so that a
// caller holding a runtime.DB never names this package. These tests pin that
// the wrapper is the same transaction, not a lookalike, and that it fails the
// way the contract says.

import (
	"context"
	"errors"
	"testing"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/mydrv"
)

func startTxPool(t *testing.T) (*mydrv.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()
	p, err := mydrv.NewPool(ctx, config(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	if _, err := p.Exec(ctx, `CREATE TABLE IF NOT EXISTS mydrv_starttx (id INT NOT NULL PRIMARY KEY)`, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "DELETE FROM mydrv_starttx", nil); err != nil {
		t.Fatal(err)
	}
	return p, ctx
}

// rows counts the table by deleting it: the affected count IS the row count,
// and it leaves the table empty for the next step.
func rows(t *testing.T, p *mydrv.Pool, ctx context.Context) int64 {
	t.Helper()
	n, err := p.Exec(ctx, "DELETE FROM mydrv_starttx", nil)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStartTxIsARealTransactionOnMySQL(t *testing.T) {
	p, ctx := startTxPool(t)

	tx, err := p.StartTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO mydrv_starttx (id) VALUES (1)", nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if n := rows(t, p, ctx); n != 0 {
		t.Fatalf("a rolled-back insert left %d row(s)", n)
	}

	tx, err = p.StartTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO mydrv_starttx (id) VALUES (1)", nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// Committing twice is ErrTxDone, and the adapter's own ErrTxDone IS the
	// shared one, so both spellings of the check hold.
	if err := tx.Commit(ctx); !errors.Is(err, runtime.ErrTxDone) || !errors.Is(err, mydrv.ErrTxDone) {
		t.Errorf("second commit = %v, want ErrTxDone under both names", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Errorf("rollback after commit = %v, want nil", err)
	}
	if n := rows(t, p, ctx); n != 1 {
		t.Fatalf("a committed insert left %d row(s), want 1", n)
	}
}

// A failed start is a NIL runtime.Tx. Passing Begin's (*Tx)(nil) through the
// interface would have made it a non-nil one holding a nil pointer.
func TestStartTxFailureIsANilTxOnMySQL(t *testing.T) {
	p, _ := startTxPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tx, err := p.StartTx(ctx)
	if err == nil {
		t.Fatal("StartTx on a cancelled context succeeded")
	}
	if tx != nil {
		t.Errorf("tx = %#v on error, want a nil runtime.Tx", tx)
	}
}

func TestInTxOnMySQL(t *testing.T) {
	p, ctx := startTxPool(t)
	boom := errors.New("boom")
	if err := runtime.InTx(ctx, p, func(ex runtime.Executor) error {
		if _, err := ex.Exec(ctx, "INSERT INTO mydrv_starttx (id) VALUES (1)", nil); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("InTx = %v, want fn's error", err)
	}
	if n := rows(t, p, ctx); n != 0 {
		t.Fatalf("InTx rolled back and left %d row(s) on MySQL", n)
	}
	if err := runtime.InTx(ctx, p, func(ex runtime.Executor) error {
		_, err := ex.Exec(ctx, "INSERT INTO mydrv_starttx (id) VALUES (1)", nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := rows(t, p, ctx); n != 1 {
		t.Fatalf("InTx committed and left %d row(s) on MySQL, want 1", n)
	}
}
