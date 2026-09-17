package pgxdrv_test

import (
	"context"
	"os"
	"testing"

	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/pgxdrv"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A session-scoped advisory lock is the thing Pool cannot do.
//
// pg_advisory_lock is held by the CONNECTION that took it, so taking it
// through a pool and releasing it through the pool releases it on whichever
// connection came back next — which releases nothing, and leaks the lock until
// that connection is recycled. This asserts the lock is actually held (a
// second session cannot take it) and actually released.
func TestConnHoldsSessionState(t *testing.T) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		t.Skip("STORM_DSN unset")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	held, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	probe, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Release()

	const key = int64(7_449_001)
	ex := pgxdrv.Conn{C: held}
	other := pgxdrv.Conn{C: probe}

	rows, err := ex.Query(ctx, "SELECT pg_try_advisory_lock($1)", []any{key})
	if err != nil {
		t.Fatal(err)
	}
	if !scanBool(t, rows) {
		t.Fatal("could not take the lock on a free key")
	}

	// A DIFFERENT session must be refused while it is held.
	rows, err = other.Query(ctx, "SELECT pg_try_advisory_lock($1)", []any{key})
	if err != nil {
		t.Fatal(err)
	}
	if scanBool(t, rows) {
		t.Fatal("a second session took a lock that was already held")
	}

	rows, err = ex.Query(ctx, "SELECT pg_advisory_unlock($1)", []any{key})
	if err != nil {
		t.Fatal(err)
	}
	if !scanBool(t, rows) {
		t.Fatal("unlock reported the lock was not held by this session")
	}

	// And it is genuinely free again, which is what proves the unlock ran on
	// the same session as the lock.
	rows, err = other.Query(ctx, "SELECT pg_try_advisory_lock($1)", []any{key})
	if err != nil {
		t.Fatal(err)
	}
	if !scanBool(t, rows) {
		t.Fatal("the lock was not released")
	}
	if _, err := other.Exec(ctx, "SELECT pg_advisory_unlock($1)", []any{key}); err != nil {
		t.Fatal(err)
	}
}

func scanBool(t *testing.T, rows runtime.Rows) bool {
	t.Helper()
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("no row: %v", rows.Err())
	}
	v := rows.RawValues()
	if len(v) != 1 || len(v[0]) == 0 {
		t.Fatalf("unexpected row shape %v", v)
	}
	return v[0][0] == 't' || v[0][0] == 1
}
