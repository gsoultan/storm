package planspike_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gsoultan/storm/internal/planspike/store/org"
	"github.com/gsoultan/storm/internal/planspike/store/user"
	"github.com/gsoultan/storm/runtime"
	"github.com/gsoultan/storm/runtime/pgxdrv"
)

// lockFixture gives a test its own org and a known number of users in it —
// its own, because this package's fixture is shared in both directions and a
// test that writes into a seeded org breaks every test that counts one.
func lockFixture(t *testing.T, ctx context.Context, name string, n int) ([16]byte, []user.Row) {
	t.Helper()
	ex, _ := db(t)
	no := org.Create()
	no.SetName("lock-" + name)
	o, err := no.Insert(ctx, ex)
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]user.Row, 0, n)
	for i := 0; i < n; i++ {
		nu := user.Create()
		nu.SetEmail(string(rune('a'+i)) + "@lock-" + name + ".invalid")
		nu.SetName("locked")
		nu.SetStatus("pending")
		nu.SetOrgID(o.ID)
		r, err := nu.Insert(ctx, ex)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, r)
	}
	t.Cleanup(func() {
		for _, r := range rows {
			_ = user.Delete(ctx, ex, r.ID)
		}
		_ = org.Delete(ctx, ex, o.ID)
	})
	return o.ID, rows
}

// The queue claim: two workers take disjoint work from one table without
// blocking each other. This is the whole reason SKIP LOCKED exists, and it
// cannot be shown with one connection — the second reader has to be a real
// concurrent transaction or the test proves nothing.
func TestLock_SkipLockedGivesTwoWorkersDisjointRows(t *testing.T) {
	ctx := context.Background()
	orgID, rows := lockFixture(t, ctx, "queue", 4)

	txA, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback(ctx)
	txB, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer txB.Rollback(ctx)

	claim := func(tx pgxdrv.Tx, n int64) []user.Row {
		t.Helper()
		got, err := user.New().
			Where(user.OrgID.Eq(orgID)).
			Order(user.Email.Asc()).
			Limit(n).
			ForUpdateSkipLocked().
			All(ctx, tx, nil)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	first := claim(pgxdrv.Tx{T: txA}, 2)
	if len(first) != 2 {
		t.Fatalf("worker A claimed %d rows, want 2", len(first))
	}
	// B must not block, and must not see A's rows.
	second := claim(pgxdrv.Tx{T: txB}, 2)
	if len(second) != 2 {
		t.Fatalf("worker B claimed %d rows, want 2", len(second))
	}
	seen := map[[16]byte]bool{}
	for _, r := range append(append([]user.Row{}, first...), second...) {
		if seen[r.ID] {
			t.Fatalf("both workers claimed %x — SKIP LOCKED did not skip", r.ID)
		}
		seen[r.ID] = true
	}
	if len(seen) != 4 {
		t.Fatalf("the two workers together claimed %d of %d rows", len(seen), len(rows))
	}

	// And the third worker gets nothing rather than waiting: every row is
	// held. Fewer rows than Limit asked for is the point of the form.
	txC, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer txC.Rollback(ctx)
	if got := claim(pgxdrv.Tx{T: txC}, 2); len(got) != 0 {
		t.Fatalf("worker C claimed %d rows from a fully locked table", len(got))
	}
}

// NOWAIT is the other answer to a held lock: be told at once. The error is
// classified, because a caller has to tell it from a bug — and it is
// deliberately not Retryable, since retrying in a loop is a spin.
func TestLock_NoWaitFailsAtOnceWithAClassifiedError(t *testing.T) {
	ctx := context.Background()
	orgID, _ := lockFixture(t, ctx, "nowait", 1)

	txA, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer txA.Rollback(ctx)
	if _, err := user.New().Where(user.OrgID.Eq(orgID)).ForUpdate().All(ctx, pgxdrv.Tx{T: txA}, nil); err != nil {
		t.Fatal(err)
	}

	txB, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer txB.Rollback(ctx)
	_, err = user.New().Where(user.OrgID.Eq(orgID)).ForUpdateNoWait().All(ctx, pgxdrv.Tx{T: txB}, nil)
	if !errors.Is(err, runtime.ErrLockNotAvailable) {
		t.Fatalf("err = %v, want runtime.ErrLockNotAvailable", err)
	}
	if runtime.Retryable(err) {
		t.Error("a held lock is reported as retryable; a retry loop on it spins")
	}
}

// The clause goes at the very end — after LIMIT and OFFSET, which is what the
// grammar requires — and the lock is part of the statement, so two modes must
// not share a compiled statement.
func TestLock_ClauseIsASuffixAndKeysTheCache(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	sql := map[string]string{}
	for _, c := range []struct {
		name string
		q    user.Query
		want string
	}{
		{"none", user.New().Limit(1), ""},
		{"update", user.New().Limit(1).ForUpdate(), " FOR UPDATE"},
		{"nowait", user.New().Limit(1).ForUpdateNoWait(), " FOR UPDATE NOWAIT"},
		{"skip", user.New().Limit(1).ForUpdateSkipLocked(), " FOR UPDATE SKIP LOCKED"},
		{"share", user.New().Limit(1).ForShare(), " FOR SHARE"},
		{"offset", user.New().Limit(1).Offset(1).ForUpdate(), " FOR UPDATE"},
	} {
		cap := &capture{Executor: ex}
		if _, err := c.q.All(ctx, cap, nil); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := cap.sqls[len(cap.sqls)-1]
		sql[c.name] = got
		if c.want == "" {
			if strings.Contains(got, "FOR ") {
				t.Errorf("%s: an unlocked read grew a lock clause:\n%s", c.name, got)
			}
			continue
		}
		if !strings.HasSuffix(got, c.want) {
			t.Errorf("%s: want the statement to END with %q:\n%s", c.name, c.want, got)
		}
	}
	// Distinct modes must compile distinct statements, or the cache would
	// hand one call site another's lock.
	seen := map[string]string{}
	for name, s := range sql {
		if prev, ok := seen[s]; ok {
			t.Errorf("%s and %s share a compiled statement:\n%s", prev, name, s)
		}
		seen[s] = name
	}
}

// The server refuses a row lock with an aggregate, a grouping or an outer
// join. Refusing at the call site names the rule; the server names a SQLSTATE
// on a query that had already been written and shipped.
func TestLock_RefusedWhereTheServerWouldRefuse(t *testing.T) {
	ctx := context.Background()
	ex, _ := db(t)

	if _, err := user.New().ForUpdate().Count(ctx, ex); err == nil {
		t.Error("a locked Count was accepted; the server refuses it")
	} else if !strings.Contains(err.Error(), "aggregate") {
		t.Errorf("the refusal does not name the rule: %v", err)
	}
	if _, err := user.New().ForUpdate().Exists(ctx, ex); err == nil {
		t.Error("a locked Exists was accepted")
	} else if !strings.Contains(err.Error(), "One()") {
		t.Errorf("the refusal does not name the alternative: %v", err)
	}
	// And the unlocked forms still work, so the guard is not refusing
	// everything.
	if _, err := user.New().Count(ctx, ex); err != nil {
		t.Fatalf("an unlocked Count was refused: %v", err)
	}
	if _, err := user.New().Exists(ctx, ex); err != nil {
		t.Fatalf("an unlocked Exists was refused: %v", err)
	}
}

// Composing a lock must not allocate, for the same reason composing a
// predicate must not.
func TestLock_IsZeroAlloc(t *testing.T) {
	if n := testing.AllocsPerRun(1000, func() {
		q := user.New().Where(user.Status.Eq("pending")).Limit(10).ForUpdateSkipLocked()
		if q.Err() != nil {
			t.Fatal(q.Err())
		}
	}); n != 0 {
		t.Errorf("composing a locked query allocates %v times, want 0", n)
	}
}

// FOR NO KEY UPDATE is what lets a locked parent take children. A user row
// points at its org, and inserting one checks the org row with FOR KEY SHARE:
// under ForUpdate that check waits for the lock, so a transaction that holds an
// org and then waits on the inserter deadlocks with it. Under ForNoKeyUpdate
// the insert goes through — and a second writer still waits, which is the lock
// doing its job.
func TestLock_NoKeyUpdateLetsAChildInWhileWritersStillWait(t *testing.T) {
	ctx := context.Background()
	orgID, _ := lockFixture(t, ctx, "nokey", 0)

	held, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback(ctx)
	if _, err := org.New().IDEq(orgID).ForNoKeyUpdate().All(ctx, pgxdrv.Tx{T: held}, nil); err != nil {
		t.Fatal(err)
	}

	child, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Rollback(ctx)
	nu := user.Create()
	nu.SetEmail("child@lock-nokey.invalid")
	nu.SetName("child")
	nu.SetStatus("pending")
	nu.SetOrgID(orgID)
	quick, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := nu.Insert(quick, pgxdrv.Tx{T: child}); err != nil {
		t.Fatalf("a child of the locked org was made to wait or refused: %v", err)
	}

	writer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(ctx)
	_, err = org.New().IDEq(orgID).ForUpdateNoWait().All(ctx, pgxdrv.Tx{T: writer}, nil)
	if !errors.Is(err, runtime.ErrLockNotAvailable) {
		t.Fatalf("a second writer got %v; want it held off by the lock", err)
	}
}
