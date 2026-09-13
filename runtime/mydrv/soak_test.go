package mydrv_test

// Sustained concurrent load, which is where a service differs from a test.
//
// Every other live test here runs one thing at a time and stops. A pool's real
// failures need time and contention: a connection leaked on an error path, a
// statement cache that never evicts, a cancellation watcher that outlives its
// statement and kills someone else's, a connection returned to the pool in a
// state the next caller inherits. None of those show up in a test that makes
// one query.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/storm/runtime/mydrv"
)

func TestSoakUnderConcurrentLoadWithCancellations(t *testing.T) {
	if testing.Short() {
		t.Skip("soak")
	}
	cfg := config(t)
	cfg.MaxConns = 4
	cfg.MaxPreparedStmts = 8
	// Short, so a leaked connection surfaces as a named error inside the run
	// rather than as a test that hangs until the harness kills it.
	cfg.AcquireTimeout = 15 * time.Second
	p, err := mydrv.NewPool(context.Background(), cfg)
	if err != nil {
		t.Skipf("no server: %v", err)
	}
	defer p.Close()

	ctx := context.Background()
	mustPool(t, p, "DROP TABLE IF EXISTS `soak_probe`")
	mustPool(t, p, "CREATE TABLE `soak_probe` (`id` BIGINT PRIMARY KEY, "+
		"`s` VARCHAR(40) NOT NULL, KEY `ix_s` (`s`)) ENGINE=InnoDB")
	t.Cleanup(func() { _, _ = p.Exec(ctx, "DROP TABLE IF EXISTS `soak_probe`", nil) })
	for i := 0; i < 200; i++ {
		if _, err := p.Exec(ctx, "INSERT INTO `soak_probe` VALUES (?, ?)",
			[]any{int64(1_000_000 + i), "row"}); err != nil {
			t.Fatal(err)
		}
	}

	const workers, rounds = 12, 60
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		errs   []error
		killed int
	)
	record := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		errs = append(errs, err)
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				switch (w + r) % 4 {
				case 0:
					// A read whose statement TEXT varies, so the bounded cache
					// evicts throughout: eviction closes a server-side handle,
					// and closing the wrong one breaks a statement in flight.
					sql := "SELECT `id` FROM `soak_probe` WHERE `id` > ? /* " +
						itoa(r) + " */ ORDER BY `id` LIMIT 5"
					if err := drainErr(p.Query(ctx, sql, []any{int64(1_000_000)})); err != nil {
						record(err)
					}
				case 1:
					// A write, so the pool carries both kinds at once.
					if _, err := p.Exec(ctx, "UPDATE `soak_probe` SET `s` = ? WHERE `id` = ?",
						[]any{"w" + itoa(w), int64(1_000_000 + r)}); err != nil {
						record(err)
					}
				case 2:
					// A CANCELLED read. Its watcher kills the statement on a
					// second connection, and the connection it was using goes
					// back to the pool — where the next case picks it up.
					c, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
					err := drainErr(p.Query(c, "SELECT SLEEP(2)", nil))
					cancel()
					if errors.Is(err, context.DeadlineExceeded) {
						mu.Lock()
						killed++
						mu.Unlock()
					} else if err != nil {
						record(err)
					}
				case 3:
					// A transaction, which PINS a connection for its duration.
					tx, err := p.Begin(ctx)
					if err != nil {
						record(err)
						continue
					}
					if _, err := tx.Exec(ctx, "UPDATE `soak_probe` SET `s` = ? WHERE `id` = ?",
						[]any{"tx", int64(1_000_000 + r)}); err != nil {
						record(err)
					}
					if err := tx.Commit(ctx); err != nil {
						record(err)
					}
				}
			}
		}(w)
	}
	wg.Wait()

	for _, err := range errs {
		t.Errorf("under load: %v", err)
	}
	if killed == 0 {
		t.Error("no read was cancelled — the cancellation path was not exercised")
	}

	// The pool still works, which a leak would have taken away: every
	// connection out and none coming back is ErrPoolExhausted, not a hang.
	if got := one(t, p, "SELECT CAST(COUNT(*) AS CHAR) FROM `soak_probe`"); got != "200" {
		t.Errorf("after the soak the table holds %s rows, want 200", got)
	}

	// And the server is not holding statements the client forgot. The cache is
	// bounded at 8 per connection and there are at most 4 connections, so
	// anything near the number of distinct texts means eviction stopped.
	if n := atoi(one(t, p, "SHOW GLOBAL STATUS LIKE 'Prepared_stmt_count'")); n > 200 {
		t.Errorf("the server holds %d prepared statements after the soak", n)
	}

}
