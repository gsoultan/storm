package pgxdrv_test

// The transaction contract, against a real server.
//
// Every claim runtime.Tx makes about endings is a claim about what pgx does:
// that a second commit reports ErrTxClosed, that a rollback after a commit
// reports it too, that both are distinguishable from a connection that broke.
// Those are not properties this package can assert against a stub, because the
// stub would be built from the same belief the adapter was.

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/pgxdrv"
	"github.com/jackc/pgx/v5/pgxpool"
)

func livePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		t.Skip("STORM_DSN unset")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// A scratch table per test, so a failure leaves nothing behind for the next
// one to trip over.
func scratch(t *testing.T, ctx context.Context, ex runtime.Executor, name string) {
	t.Helper()
	if _, err := ex.Exec(ctx, "drop table if exists "+name, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Exec(ctx, "create table "+name+" (n int)", nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = ex.Exec(context.Background(), "drop table if exists "+name, nil)
	})
}

// Counts the ROWS the table has, by reading them. `select count(*)` would
// count the one row the count itself comes back in, which is 1 whatever the
// table holds — and would have made a rollback that did nothing look correct.
func count(t *testing.T, ctx context.Context, ex runtime.Executor, name string) int {
	t.Helper()
	rows, err := ex.Query(ctx, "select n from "+name, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPoolStartTxReturnsAWorkingTx(t *testing.T) {
	ctx := t.Context()
	db := pgxdrv.Pool{P: livePool(t)}
	scratch(t, ctx, db, "storm_tx_begin")

	tx, err := db.StartTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "insert into storm_tx_begin values (1)", nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(t, ctx, db, "storm_tx_begin"); n != 1 {
		t.Errorf("%d rows after commit, want 1", n)
	}
}

func TestRollbackUndoesTheWrite(t *testing.T) {
	ctx := t.Context()
	db := pgxdrv.Pool{P: livePool(t)}
	scratch(t, ctx, db, "storm_tx_rollback")

	tx, err := db.StartTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "insert into storm_tx_rollback values (1)", nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(t, ctx, db, "storm_tx_rollback"); n != 0 {
		t.Errorf("%d rows after rollback, want 0", n)
	}
}

// `defer tx.Rollback(ctx)` beside a commit is the idiom the runtime.Tx
// contract promises. pgx answers that second ending with ErrTxClosed, and the
// adapter turns it into nil — if it ever stopped, every deferred rollback in
// every adopter would start returning an error nobody can act on.
func TestRollbackAfterCommitIsNil(t *testing.T) {
	ctx := t.Context()
	db := pgxdrv.Pool{P: livePool(t)}

	tx, err := db.StartTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Errorf("rollback after commit = %v, want nil", err)
	}
}

// Committing twice is a bug in every shape, so it keeps an error — the shared
// one, so a caller holding a runtime.Tx can test for it without naming pgx.
func TestSecondCommitIsErrTxDone(t *testing.T) {
	ctx := t.Context()
	db := pgxdrv.Pool{P: livePool(t)}

	tx, err := db.StartTx(ctx)
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

// InTx over the real adapter: the helper is only worth having if its three
// endings hold against a server, not against a stub that agrees with it.
func TestInTxCommitsAndRollsBackForReal(t *testing.T) {
	ctx := t.Context()
	db := pgxdrv.Pool{P: livePool(t)}
	scratch(t, ctx, db, "storm_intx")

	if err := runtime.InTx(ctx, db, func(ex runtime.Executor) error {
		_, err := ex.Exec(ctx, "insert into storm_intx values (1)", nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, ctx, db, "storm_intx"); n != 1 {
		t.Fatalf("%d rows after a committed InTx, want 1", n)
	}

	boom := errors.New("boom")
	err := runtime.InTx(ctx, db, func(ex runtime.Executor) error {
		if _, err := ex.Exec(ctx, "insert into storm_intx values (2)", nil); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if n := count(t, ctx, db, "storm_intx"); n != 1 {
		t.Errorf("%d rows after a failed InTx, want 1 — the rollback did not happen", n)
	}
}

// A panic must not leave the transaction open with its connection checked
// out. A pool of one makes that visible: if the connection leaked, the next
// StartTx would block until the context died.
func TestInTxPanicReleasesTheConnection(t *testing.T) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		t.Skip("STORM_DSN unset")
	}
	ctx := t.Context()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	db := pgxdrv.Pool{P: pool}

	func() {
		defer func() { _ = recover() }()
		_ = runtime.InTx(ctx, db, func(runtime.Executor) error { panic("kaboom") })
	}()

	// The only connection must be back. Without the deferred rollback this
	// hangs until the context deadline rather than failing.
	done := make(chan error, 1)
	go func() {
		tx, err := db.StartTx(ctx)
		if err == nil {
			err = tx.Rollback(ctx)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("the connection was never returned: a panicking InTx leaked its transaction")
	}
}

var _ runtime.DB = pgxdrv.Pool{}
