package migrate_test

import (
	"context"
	"errors"
	"hash/fnv"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gsoultan/storm/migrate"
	"github.com/gsoultan/storm/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Auto applies DDL to a live database, so its tests need one. Every assertion
// here is about what the SERVER holds afterwards, not about rendered text: the
// text is diff_test.go's job, and a migration runner that emits perfect SQL it
// cannot apply is the failure mode this file exists to catch.

func dsn(t *testing.T) string {
	t.Helper()
	d := os.Getenv("STORM_DSN")
	if d == "" {
		t.Skip("STORM_DSN unset")
	}
	return d
}

// conn gives a test its own session and its own namespace, so the file can run
// shuffled and in parallel without one test migrating another's tables.
func conn(t *testing.T, ns string) (context.Context, *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = c.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+ns+" CASCADE")
		_ = c.Close(context.Background())
	})
	if _, err := c.Exec(ctx, "DROP SCHEMA IF EXISTS "+ns+" CASCADE"); err != nil {
		t.Fatal(err)
	}
	return ctx, c
}

func users(extra ...*schema.Column) *schema.Schema {
	cols := append([]*schema.Column{
		col("id", "uuid", true),
		col("email", "text", true),
	}, extra...)
	return sch(tbl("users", cols...))
}

// tableExists asks the catalog, not the plan.
func tableExists(t *testing.T, ctx context.Context, c *pgx.Conn, ns, name string) bool {
	t.Helper()
	var n int
	if err := c.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema=$1 AND table_name=$2`,
		ns, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func columnExists(t *testing.T, ctx context.Context, c *pgx.Conn, ns, table, name string) bool {
	t.Helper()
	var n int
	if err := c.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_schema=$1 AND table_name=$2 AND column_name=$3`,
		ns, table, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func TestAuto_CreatesFromNothing(t *testing.T) {
	ns := "storm_auto_create"
	ctx, c := conn(t, ns)

	res, err := migrate.Auto(ctx, c, users(), migrate.AutoOptions{Schema: ns})
	if err != nil {
		t.Fatalf("Auto: %v", err)
	}
	if res.Empty() {
		t.Fatal("Auto reported nothing applied against an empty database")
	}
	if !tableExists(t, ctx, c, ns, "users") {
		t.Fatalf("users was not created; Auto said it applied:\n%s", res.SQL())
	}
}

// The property that makes Auto safe to call on every process start.
func TestAuto_Idempotent(t *testing.T) {
	ns := "storm_auto_idem"
	ctx, c := conn(t, ns)
	want := users()

	if _, err := migrate.Auto(ctx, c, want, migrate.AutoOptions{Schema: ns}); err != nil {
		t.Fatalf("first Auto: %v", err)
	}
	res, err := migrate.Auto(ctx, c, want, migrate.AutoOptions{Schema: ns})
	if err != nil {
		t.Fatalf("second Auto: %v", err)
	}
	if !res.Empty() {
		t.Fatalf("second Auto applied %d step(s); it must find nothing to do:\n%s",
			len(res.Applied), res.SQL())
	}
}

// Refusing has to mean nothing was applied, not that the destructive step was
// skipped: a plan applied in part is a schema in a state the model never named.
func TestAuto_RefusesDestructiveAndAppliesNothing(t *testing.T) {
	ns := "storm_auto_destructive"
	ctx, c := conn(t, ns)

	full := users(col("nickname", "text", false))
	if _, err := migrate.Auto(ctx, c, full, migrate.AutoOptions{Schema: ns}); err != nil {
		t.Fatalf("setup Auto: %v", err)
	}

	// Drop nickname from the model and add a column, so the plan holds one
	// destructive step AND one that is not.
	shrunk := users(col("added_later", "text", false))
	res, err := migrate.Auto(ctx, c, shrunk, migrate.AutoOptions{Schema: ns})
	var de *migrate.DestructiveError
	if !errors.As(err, &de) {
		t.Fatalf("dropping a column returned %v, want *DestructiveError", err)
	}
	if len(res.Applied) != 0 {
		t.Fatalf("refused plan still applied %d step(s):\n%s", len(res.Applied), res.SQL())
	}
	if !columnExists(t, ctx, c, ns, "users", "nickname") {
		t.Fatal("nickname was dropped by a plan that was refused")
	}
	if columnExists(t, ctx, c, ns, "users", "added_later") {
		t.Fatal("the non-destructive step of a refused plan was applied on its own")
	}
	if len(de.Destructive()) == 0 {
		t.Fatal("DestructiveError names no destructive step")
	}
	if !strings.Contains(de.Error(), "nothing was applied") {
		t.Fatalf("the error does not say nothing was applied:\n%s", de.Error())
	}
}

func TestAuto_AllowDestructiveApplies(t *testing.T) {
	ns := "storm_auto_allow"
	ctx, c := conn(t, ns)

	if _, err := migrate.Auto(ctx, c, users(col("nickname", "text", false)),
		migrate.AutoOptions{Schema: ns}); err != nil {
		t.Fatalf("setup Auto: %v", err)
	}
	if _, err := migrate.Auto(ctx, c, users(),
		migrate.AutoOptions{Schema: ns, AllowDestructive: true}); err != nil {
		t.Fatalf("Auto with AllowDestructive: %v", err)
	}
	if columnExists(t, ctx, c, ns, "users", "nickname") {
		t.Fatal("AllowDestructive did not drop the column")
	}
}

// The reason the transactional steps share one transaction. A plan that fails
// half way must leave the schema where it started, not somewhere in between.
func TestAuto_RollsBackTheWholePlanOnFailure(t *testing.T) {
	ns := "storm_auto_rollback"
	ctx, c := conn(t, ns)

	if _, err := migrate.Auto(ctx, c, users(), migrate.AutoOptions{Schema: ns}); err != nil {
		t.Fatalf("setup Auto: %v", err)
	}
	// Two rows that a UNIQUE index cannot accept.
	if _, err := c.Exec(ctx, "INSERT INTO "+ns+".users (id, email) VALUES "+
		"('11111111-1111-1111-1111-111111111111','dup@example.com'),"+
		"('22222222-2222-2222-2222-222222222222','dup@example.com')"); err != nil {
		t.Fatal(err)
	}

	// A plan holding a step that CANNOT succeed (unique over duplicate data)
	// and a step that easily could (a whole new table).
	u := tbl("users", col("id", "uuid", true), col("email", "text", true))
	u.Indexes = []*schema.Index{{
		Name:    "users_email_key",
		Columns: []schema.IndexColumn{{Name: "email"}},
		Unique:  true,
	}}
	want := sch(u, tbl("teams", col("id", "uuid", true), col("name", "text", true)))

	res, err := migrate.Auto(ctx, c, want, migrate.AutoOptions{Schema: ns})
	if err == nil {
		t.Fatalf("a unique index over duplicate rows succeeded; applied:\n%s", res.SQL())
	}
	if tableExists(t, ctx, c, ns, "teams") {
		t.Fatal("teams survived a failed migration: the plan was not one transaction")
	}
	if len(res.Applied) != 0 {
		t.Fatalf("a rolled-back plan reported %d applied step(s):\n%s", len(res.Applied), res.SQL())
	}
}

// N replicas starting together is the case Auto exists to survive. Without the
// advisory lock both sessions compute the same plan and the loser's CREATE
// TABLE fails; with it, the loser re-diffs and finds nothing to do.
func TestAuto_ConcurrentProcessesApplyOnce(t *testing.T) {
	ns := "storm_auto_race"
	ctx, c := conn(t, ns)
	_ = ctx

	const racers = 4
	type outcome struct {
		steps int
		err   error
	}
	out := make([]outcome, racers)
	conns := make([]*pgx.Conn, racers)
	for i := range conns {
		cc, err := pgx.Connect(context.Background(), dsn(t))
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = cc
		t.Cleanup(func() { _ = cc.Close(context.Background()) })
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := migrate.Auto(context.Background(), conns[i], users(),
				migrate.AutoOptions{Schema: ns, LockWait: 60 * time.Second})
			out[i] = outcome{len(res.Applied), err}
		}(i)
	}
	close(start)
	wg.Wait()

	applied := 0
	for i, o := range out {
		if o.err != nil {
			t.Errorf("racer %d failed: %v", i, o.err)
		}
		if o.steps > 0 {
			applied++
		}
	}
	if applied != 1 {
		t.Fatalf("%d of %d racers applied steps; exactly one must", applied, racers)
	}
	if !tableExists(t, context.Background(), c, ns, "users") {
		t.Fatal("the winning racer did not leave users behind")
	}
}

// The busy path, and with it the advisory key itself. The key is a compatibility
// contract — two replicas on DIFFERENT storm versions during a rolling deploy
// must agree on it — so the test recomputes it from the documented rule rather
// than asking the package. Changing advisoryKey breaks this test, which is the
// intent: it is a wire format, not an implementation detail.
func TestAuto_LockBusyIsReportedAndAppliesNothing(t *testing.T) {
	ns := "storm_auto_busy"
	ctx, c := conn(t, ns)

	h := fnv.New64a()
	_, _ = h.Write([]byte("storm:automigrate:" + ns))
	key := int64(h.Sum64())

	holder, err := pgx.Connect(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close(ctx)
	var got bool
	if err := holder.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("could not take the lock to hold it")
	}

	started := time.Now()
	res, err := migrate.Auto(ctx, c, users(), migrate.AutoOptions{Schema: ns, LockWait: -1})
	if !errors.Is(err, migrate.ErrLockBusy) {
		t.Fatalf("Auto with the lock held returned %v, want ErrLockBusy", err)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Errorf("LockWait<0 waited %s; it must not wait at all", took)
	}
	if len(res.Applied) != 0 {
		t.Fatalf("a call that never got the lock applied %d step(s)", len(res.Applied))
	}
	if tableExists(t, ctx, c, ns, "users") {
		t.Fatal("a call that never got the lock created a table")
	}

	// Released, the same call goes through.
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_unlock($1)", key); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Auto(ctx, c, users(), migrate.AutoOptions{Schema: ns, LockWait: -1}); err != nil {
		t.Fatalf("Auto once the lock was free: %v", err)
	}
	if !tableExists(t, ctx, c, ns, "users") {
		t.Fatal("users was not created once the lock was free")
	}
}

// A migration must not be observable in the session afterwards — least of all
// through a pool, where the connection goes on serving application queries.
func TestAuto_RestoresSessionSettings(t *testing.T) {
	ns := "storm_auto_session"
	ctx, c := conn(t, ns)

	if _, err := c.Exec(ctx, "SET search_path TO pg_catalog"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "SET lock_timeout TO '7s'"); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Auto(ctx, c, users(), migrate.AutoOptions{
		Schema: ns, Concurrently: true, LockTimeout: 250 * time.Millisecond,
	}); err != nil {
		t.Fatalf("Auto: %v", err)
	}

	var path, lock string
	if err := c.QueryRow(ctx, "SHOW search_path").Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := c.QueryRow(ctx, "SHOW lock_timeout").Scan(&lock); err != nil {
		t.Fatal(err)
	}
	if path != "pg_catalog" {
		t.Errorf("search_path is %q after Auto, want pg_catalog", path)
	}
	if lock != "7s" {
		t.Errorf("lock_timeout is %q after Auto, want 7s", lock)
	}
}

// The Concurrently path, which is the only thing that produces a step Auto has
// to run OUTSIDE the transaction. The index has to be added to a table that
// already exists: Plan.Concurrently deliberately leaves alone an index on a
// table the same plan creates, because nothing is writing to it yet.
func TestAuto_ConcurrentIndexRunsOutsideTheTransaction(t *testing.T) {
	ns := "storm_auto_concurrent"
	ctx, c := conn(t, ns)

	if _, err := migrate.Auto(ctx, c, users(), migrate.AutoOptions{Schema: ns}); err != nil {
		t.Fatalf("setup Auto: %v", err)
	}

	u := tbl("users", col("id", "uuid", true), col("email", "text", true))
	u.Indexes = []*schema.Index{{
		Name:    "users_email_idx",
		Columns: []schema.IndexColumn{{Name: "email"}},
	}}
	res, err := migrate.Auto(ctx, c, sch(u), migrate.AutoOptions{Schema: ns, Concurrently: true})
	if err != nil {
		t.Fatalf("Auto with Concurrently: %v", err)
	}
	if !strings.Contains(res.SQL(), "CONCURRENTLY") {
		t.Fatalf("Concurrently did not produce a concurrent build:\n%s", res.SQL())
	}
	var alone int
	for _, ch := range res.Applied {
		if ch.NoTransaction {
			alone++
		}
	}
	if alone == 0 {
		t.Fatal("no step was marked NoTransaction, so applyAlone never ran")
	}

	var n int
	if err := c.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE schemaname=$1 AND indexname=$2`,
		ns, "users_email_idx").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("the concurrent index was reported applied but is not in the catalog")
	}
	// And it is still idempotent with the concurrent form in play.
	again, err := migrate.Auto(ctx, c, sch(u), migrate.AutoOptions{Schema: ns, Concurrently: true})
	if err != nil {
		t.Fatalf("second Auto: %v", err)
	}
	if !again.Empty() {
		t.Fatalf("re-running a concurrent plan applied %d step(s):\n%s", len(again.Applied), again.SQL())
	}
}

// AutoPool is what an application calls, and the connection it borrows goes
// back into the pool to serve queries. A setting left behind on it is a bug
// that shows up nowhere near here — so the pool is pinned to one connection and
// the test asks that exact connection what it is carrying afterwards.
func TestAutoPool_LeavesTheBorrowedConnectionAsItFoundIt(t *testing.T) {
	ns := "storm_auto_pool"
	ctx, c := conn(t, ns)
	_ = c

	cfg, err := pgxpool.ParseConfig(dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1 // so the connection Auto borrowed is the one we interrogate
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// The table first, in its own call: Plan.Concurrently leaves alone an index
	// on a table the same plan creates, so a single call here would produce no
	// NoTransaction step, never reach applyAlone, and set nothing on the
	// session — the test would pass without testing anything, which is exactly
	// what the first draft of it did.
	if _, err := migrate.AutoPool(ctx, pool, users(), migrate.AutoOptions{Schema: ns}); err != nil {
		t.Fatalf("setup AutoPool: %v", err)
	}
	u := tbl("users", col("id", "uuid", true), col("email", "text", true))
	u.Indexes = []*schema.Index{{
		Name:    "users_email_idx",
		Columns: []schema.IndexColumn{{Name: "email"}},
	}}
	// Concurrently, because that is the path that sets lock_timeout on the
	// SESSION rather than the transaction — the one that can leak.
	res, err := migrate.AutoPool(ctx, pool, sch(u), migrate.AutoOptions{
		Schema: ns, Concurrently: true, LockTimeout: 1500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("AutoPool: %v", err)
	}
	var alone int
	for _, ch := range res.Applied {
		if ch.NoTransaction {
			alone++
		}
	}
	if alone == 0 {
		t.Fatalf("no NoTransaction step ran, so nothing could have leaked and this test proves nothing:\n%s", res.SQL())
	}
	if !tableExists(t, ctx, c, ns, "users") {
		t.Fatal("AutoPool did not create the table")
	}

	var lock, path string
	if err := pool.QueryRow(ctx, "SHOW lock_timeout").Scan(&lock); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SHOW search_path").Scan(&path); err != nil {
		t.Fatal(err)
	}
	if lock != "0" {
		t.Errorf("the pooled connection carries lock_timeout=%q after AutoPool; want 0", lock)
	}
	if strings.Contains(path, ns) {
		t.Errorf("the pooled connection carries search_path=%q after AutoPool", path)
	}
}

// A cancelled migration must not leave the advisory lock held: it is held by
// the SESSION, and AutoPool's session rejoins the pool, so a leaked one would
// make every later caller wait out LockWait against a lock nothing is using.
//
// What this test does NOT prove is that Auto's detached-context cleanup is what
// delivers that. Measured: pgx CLOSES a connection whose query is cancelled
// mid-flight, and PostgreSQL drops session advisory locks on disconnect — so
// this passes with the detaching removed. The window the detaching actually
// covers is the one where the deadline expires BETWEEN statements, leaving a
// healthy connection and a client-side failure that never reaches the server;
// it is real, it is reachable through a pool, and it is too narrow to hit from
// outside the package. The test is kept for the user-visible property, not as
// proof of the mechanism.
func TestAuto_CancelledMigrationReleasesTheLock(t *testing.T) {
	ns := "storm_auto_cancel"
	ctx, c := conn(t, ns)

	short, cancel := context.WithTimeout(ctx, 15*time.Millisecond)
	defer cancel()
	if _, err := migrate.Auto(short, c, users(), migrate.AutoOptions{Schema: ns}); err == nil {
		t.Skip("the migration beat the timeout; nothing to assert")
	}

	// Ask from another session, because pg_advisory_unlock only reports on the
	// caller's own locks and pg_locks is the only honest witness.
	other, err := pgx.Connect(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(ctx)

	h := fnv.New64a()
	_, _ = h.Write([]byte("storm:automigrate:" + ns))
	key := int64(h.Sum64())

	deadline := time.Now().Add(5 * time.Second)
	for {
		var held int
		if err := other.QueryRow(ctx,
			`SELECT count(*) FROM pg_locks
			 WHERE locktype='advisory' AND granted
			   AND ((classid::bigint << 32) | objid::bigint) = $1`, key).Scan(&held); err != nil {
			t.Fatal(err)
		}
		if held == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the advisory lock is still held after the migration was cancelled")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// lock_timeout is the headline production property and the one most easily
// claimed without being true. An ALTER TABLE needs ACCESS EXCLUSIVE; while it
// waits for one, every query arriving behind it waits too, so an unbounded wait
// is not a slow migration but an outage. Here another session holds the table
// open and Auto has to give up rather than join the queue.
func TestAuto_LockTimeoutBoundsABlockedAlter(t *testing.T) {
	ns := "storm_auto_lock_timeout"
	ctx, c := conn(t, ns)

	if _, err := migrate.Auto(ctx, c, users(), migrate.AutoOptions{Schema: ns}); err != nil {
		t.Fatalf("setup Auto: %v", err)
	}

	// Another session reads the table inside an open transaction: ACCESS SHARE,
	// held until it ends, which is enough to make ACCESS EXCLUSIVE wait.
	blocker, err := pgx.Connect(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close(ctx)
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT count(*) FROM "+ns+".users"); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	res, err := migrate.Auto(ctx, c, users(col("nickname", "text", false)),
		migrate.AutoOptions{Schema: ns, LockTimeout: 300 * time.Millisecond})
	took := time.Since(start)

	if err == nil {
		t.Fatalf("the blocked ALTER succeeded; lock_timeout did nothing:\n%s", res.SQL())
	}
	// It has to be the TIMEOUT that stopped it, not some unrelated failure that
	// would make this test pass for the wrong reason.
	if !strings.Contains(err.Error(), "lock timeout") {
		t.Fatalf("Auto failed, but not on the lock timeout: %v", err)
	}
	if took > 10*time.Second {
		t.Fatalf("Auto waited %s for a lock bounded at 300ms", took)
	}
	if len(res.Applied) != 0 {
		t.Fatalf("a timed-out migration reported %d applied step(s)", len(res.Applied))
	}
	if columnExists(t, ctx, c, ns, "users", "nickname") {
		t.Fatal("the column was added by a migration that timed out")
	}

	// Once the blocker lets go, the same call goes through — the timeout is a
	// bound on waiting, not a refusal to work.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Auto(ctx, c, users(col("nickname", "text", false)),
		migrate.AutoOptions{Schema: ns, LockTimeout: 300 * time.Millisecond}); err != nil {
		t.Fatalf("Auto once the blocker released: %v", err)
	}
	if !columnExists(t, ctx, c, ns, "users", "nickname") {
		t.Fatal("the column was not added after the blocker released")
	}
}

// PostgreSQL runs ALTER TYPE ... ADD VALUE inside a transaction but refuses to
// let anything USE the new label until that transaction commits (SQLSTATE
// 55P04). So a plan that adds a label and then defaults a column to it cannot
// be one transaction — which is what Auto did until this test existed, and it
// failed every time on a model change as ordinary as "add a status".
func TestAuto_AddsAnEnumLabelAndUsesItInOneCall(t *testing.T) {
	ns := "storm_auto_enum"
	ctx, c := conn(t, ns)

	statuses := func(labels []string, dflt string) *schema.Schema {
		status := col("status", "status", true)
		status.Default = "'" + dflt + "'"
		u := tbl("users", col("id", "uuid", true), status)
		return &schema.Schema{
			Enums:  []*schema.Enum{{Name: "status", Labels: labels}},
			Tables: []*schema.Table{u},
		}
	}

	if _, err := migrate.Auto(ctx, c, statuses([]string{"active", "inactive"}, "active"),
		migrate.AutoOptions{Schema: ns}); err != nil {
		t.Fatalf("setup Auto: %v", err)
	}

	// Add the label AND use it, in one call, as an adopter would.
	want := statuses([]string{"active", "inactive", "pending"}, "pending")
	res, err := migrate.Auto(ctx, c, want, migrate.AutoOptions{Schema: ns})
	if err != nil {
		t.Fatalf("adding an enum label and defaulting a column to it: %v", err)
	}
	if len(res.Applied) < 2 {
		t.Fatalf("expected the label and the default; applied:\n%s", res.SQL())
	}

	var dflt string
	if err := c.QueryRow(ctx,
		`SELECT column_default FROM information_schema.columns
		 WHERE table_schema=$1 AND table_name='users' AND column_name='status'`,
		ns).Scan(&dflt); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dflt, "pending") {
		t.Fatalf("column default is %q, want it to use the new label", dflt)
	}
	// And still idempotent with the enum split across two transactions.
	again, err := migrate.Auto(ctx, c, want, migrate.AutoOptions{Schema: ns})
	if err != nil {
		t.Fatalf("second Auto: %v", err)
	}
	if !again.Empty() {
		t.Fatalf("second Auto applied %d step(s):\n%s", len(again.Applied), again.SQL())
	}
}
