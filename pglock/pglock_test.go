package pglock_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/gsoultan/storm/pglock"
	"github.com/gsoultan/storm/runtime/pgxdrv"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Two sessions and one key: whoever holds it keeps the other out, a session
// that does not hold it lets go of nothing, and once let go the other gets it.
// A transaction's lock goes with its transaction.
func TestTheLockIsTheSessionsThatTookIt(t *testing.T) {
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
	session := func() pgxdrv.Conn {
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Release)
		return pgxdrv.Conn{C: c}
	}
	owner, rival := session(), session()
	const key = int64(0x5701_0c4) // storm's own: no other test takes it

	if got, err := pglock.TryLock(ctx, owner, key); err != nil || !got {
		t.Fatalf("a free lock: %v, %v; want it taken", got, err)
	}
	if got, err := pglock.TryLock(ctx, rival, key); err != nil || got {
		t.Fatalf("a held lock from another session: %v, %v; want it refused", got, err)
	}
	if let, err := pglock.Unlock(ctx, rival, key); err != nil || let {
		t.Fatalf("a session that does not hold it let go: %v, %v; want nothing let go", let, err)
	}
	taken := make(chan error, 1)
	go func() { taken <- pglock.Lock(ctx, rival, key) }()
	select {
	case err := <-taken:
		t.Fatalf("Lock on a held key returned (%v) before its holder let go", err)
	case <-time.After(200 * time.Millisecond):
	}
	if let, err := pglock.Unlock(ctx, owner, key); err != nil || !let {
		t.Fatalf("the holder let go: %v, %v; want it let go", let, err)
	}
	select {
	case err := <-taken:
		if err != nil {
			t.Fatalf("a lock waited for: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Lock did not return once its holder let go")
	}
	if err := pglock.UnlockAll(ctx, rival); err != nil {
		t.Fatalf("let go of every lock: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := pglock.TryLockXact(ctx, pgxdrv.Tx{T: tx}, key); err != nil || !got {
		t.Fatalf("a free transaction lock: %v, %v; want it taken", got, err)
	}
	if got, err := pglock.TryLock(ctx, owner, key); err != nil || got {
		t.Fatalf("a key a transaction holds: %v, %v; want it refused", got, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := pglock.TryLock(ctx, owner, key); err != nil || !got {
		t.Fatalf("a key whose transaction ended: %v, %v; want it taken", got, err)
	}
	if let, err := pglock.Unlock(ctx, owner, key); err != nil || !let {
		t.Fatalf("let go: %v, %v", let, err)
	}
}

// A statement that fails is said as the lock it was for.
func TestAFailedStatementSaysWhichLock(t *testing.T) {
	dsn := os.Getenv("STORM_DSN")
	if dsn == "" {
		t.Skip("STORM_DSN unset")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	ex := pgxdrv.Pool{P: pool}
	if _, err := pglock.TryLock(ctx, ex, 1); err == nil {
		t.Fatal("a closed pool took a lock")
	}
	if _, err := pglock.Unlock(ctx, ex, 1); err == nil {
		t.Fatal("a closed pool let go of a lock")
	}
}
