package pgstat_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/gsoultan/storm/pgstat"
	"github.com/gsoultan/storm/runtime/pgxdrv"
	"github.com/jackc/pgx/v5/pgxpool"
)

// One session holds a row, another is made to wait for it, and the reader has
// to see exactly that: who waits, behind whom, for which table. Then the
// waiter is ended and has to be gone.
func TestSessionsSeeAWaitAndTerminateEndsIt(t *testing.T) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		t.Skip("STORM_DSN unset")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	const ns = "storm_pgstat"
	for _, stmt := range []string{
		"DROP SCHEMA IF EXISTS " + ns + " CASCADE",
		"CREATE SCHEMA " + ns,
		"CREATE TABLE " + ns + ".held (id int PRIMARY KEY)",
		"INSERT INTO " + ns + ".held VALUES (1)",
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+ns+" CASCADE") })

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, "SELECT id FROM "+ns+".held WHERE id = 1 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	holderPID, _, err := pgstat.Backend(ctx, pgxdrv.Tx{T: holder})
	if err != nil {
		t.Fatal(err)
	}

	waiter, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = waiter.Rollback(context.Background()) }()
	waiterPID, _, err := pgstat.Backend(ctx, pgxdrv.Tx{T: waiter})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := waiter.Exec(ctx, "UPDATE "+ns+".held SET id = 1 WHERE id = 1")
		done <- err
	}()

	var seen pgstat.Session
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && seen.PID == 0; time.Sleep(10 * time.Millisecond) {
		sessions, err := pgstat.Sessions(ctx, pgxdrv.Pool{P: pool})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range sessions {
			if s.WaitsBehind(holderPID) {
				seen = s
			}
		}
	}
	if seen.PID != waiterPID {
		t.Fatalf("the session waiting behind %d is %d, want %d", holderPID, seen.PID, waiterPID)
	}
	if seen.WaitEventType != "Lock" || !seen.LocksOn("tuple", ns, "held") {
		t.Fatalf("the waiter's wait is %q and its locks %+v; want it queued for a row of %s.held", seen.WaitEventType, seen.Locks, ns)
	}
	if seen.LocksOn("", ns, "elsewhere") {
		t.Fatal("LocksOn matched a table the waiter has no lock on")
	}

	ended, err := pgstat.Terminate(ctx, pgxdrv.Pool{P: pool}, waiterPID)
	if err != nil || !ended {
		t.Fatalf("terminate: %v, %v", ended, err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the terminated session's statement succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the terminated session's statement never returned")
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		sessions, err := pgstat.Sessions(ctx, pgxdrv.Pool{P: pool})
		if err != nil {
			t.Fatal(err)
		}
		gone := true
		for _, s := range sessions {
			if s.PID == waiterPID {
				gone = false
			}
		}
		if gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %d is still there after it was terminated", waiterPID)
		}
	}
}

// A session queued for a table lock waits for it ungranted, which WaitsFor
// tells apart from a lock it already holds.
func TestWaitsForSeesAnUngrantedTableLock(t *testing.T) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		t.Skip("STORM_DSN unset")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	const ns = "storm_pgstat_table"
	for _, stmt := range []string{
		"DROP SCHEMA IF EXISTS " + ns + " CASCADE",
		"CREATE SCHEMA " + ns,
		"CREATE TABLE " + ns + ".held (id int PRIMARY KEY)",
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+ns+" CASCADE") })

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if _, err := holder.Exec(ctx, "LOCK TABLE "+ns+".held IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = pool.Exec(waitCtx, "SELECT count(*) FROM "+ns+".held")
	}()
	t.Cleanup(func() { cancel(); <-done })

	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		sessions, err := pgstat.Sessions(ctx, pgxdrv.Pool{P: pool})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range sessions {
			if s.WaitsFor("relation", ns, "held") {
				return
			}
		}
	}
	t.Fatal("no session was seen waiting for the table lock")
}
