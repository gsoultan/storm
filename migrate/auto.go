package migrate

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"github.com/gsoultan/storm/compile/pgddl"
	"github.com/gsoultan/storm/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Auto is the one path where storm applies DDL, and it is the exception ADR-0001
// did not have: the ADR banned runtime DDL to stop a schema changing silently,
// by a library, on a live database. That is a statement about the DANGER, not
// about the convenience. Auto keeps the convenience and answers the danger:
//
//   - it applies nothing unless you call it — there is no init(), no hook, and
//     no code path a query can reach;
//   - it refuses any plan that can lose data unless AllowDestructive says
//     otherwise, and refuses the WHOLE plan rather than the destructive steps,
//     because a half-applied schema is worse than an unapplied one;
//   - it takes a session advisory lock first, so twenty replicas starting
//     together apply the migration once;
//   - it computes the plan AFTER taking that lock, so the replicas that waited
//     re-diff against the winner's work and find nothing left to do;
//   - it bounds how long a step waits for its lock, because the outage an
//     ALTER TABLE causes is the WAIT, not the change — while it queues for its
//     ACCESS EXCLUSIVE lock, every query that arrives after it queues too; and
//   - it puts every transactional step in ONE transaction, which PostgreSQL
//     supports for DDL, so a failure half way leaves the schema where it began.
//
// `storm diff` is still the reviewed path and still the right one for a
// production database with data in it. Auto is for the schemas whose cost of
// being wrong is low: tests, local development, ephemeral environments, CI,
// and single-instance deployments.

// Default bounds for Auto. LockTimeout is deliberately short and
// StatementTimeout is deliberately absent: waiting for a lock blocks OTHER
// sessions, so it must be bounded, while a long-running rewrite only costs the
// migration itself, and cutting one off mid-way helps nobody.
const (
	DefaultLockTimeout = 3 * time.Second
	DefaultLockWait    = 30 * time.Second
)

// ErrLockBusy means another process held the migration lock for the whole of
// LockWait. Nothing was applied.
var ErrLockBusy = errors.New("another process is holding the storm migration lock")

// AutoOptions configures Auto. The zero value is the safe one: namespace
// "public", no destructive steps, no concurrent index builds, default bounds.
type AutoOptions struct {
	// Schema is the namespace to bring up to date. Empty means "public".
	// It is created if it does not exist.
	Schema string

	// AllowDestructive permits steps that can lose data — dropping an object,
	// narrowing a type, adding a NOT NULL column with no default. Without it
	// Auto returns *DestructiveError and applies nothing at all.
	AllowDestructive bool

	// Concurrently builds and drops indexes on tables that already exist with
	// CREATE/DROP INDEX CONCURRENTLY, which does not block the table's writers.
	// Those steps cannot share the transaction, so they run after it, one at a
	// time, and a failure there leaves the transactional steps applied.
	Concurrently bool

	// LockTimeout bounds how long any one step waits for a lock it needs.
	// Zero means DefaultLockTimeout; negative means no bound (PostgreSQL's own
	// default, which is to wait forever).
	LockTimeout time.Duration

	// LockWait bounds how long Auto waits for the advisory lock that serialises
	// migrating processes. Zero means DefaultLockWait; negative means do not
	// wait at all — return ErrLockBusy the moment the lock is held elsewhere.
	LockWait time.Duration

	// Logf, if set, is called once per applied step and once for the decisions
	// worth a line in a startup log.
	Logf func(format string, args ...any)
}

func (o AutoOptions) namespace() string {
	if o.Schema == "" {
		return "public"
	}
	return o.Schema
}

func (o AutoOptions) lockTimeout() time.Duration {
	if o.LockTimeout == 0 {
		return DefaultLockTimeout
	}
	return o.LockTimeout
}

func (o AutoOptions) lockWait() time.Duration {
	if o.LockWait == 0 {
		return DefaultLockWait
	}
	return o.LockWait
}

func (o AutoOptions) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// Result reports what Auto did.
type Result struct {
	// Applied is every step that ran, in the order it ran.
	Applied []Change
	// Waited is how long Auto spent waiting for the advisory lock.
	Waited time.Duration
}

// Empty reports whether the database already matched the model.
func (r Result) Empty() bool { return len(r.Applied) == 0 }

// SQL renders the steps that ran, for a log line or a test.
func (r Result) SQL() string { return Plan{Changes: r.Applied}.SQL() }

// DestructiveError is returned when the plan contains a step that can lose data
// and AutoOptions.AllowDestructive is false. Nothing was applied.
type DestructiveError struct {
	// Plan is the whole plan, so the message can show a destructive step in the
	// company of the steps that would have preceded it.
	Plan Plan
}

func (e *DestructiveError) Error() string {
	var steps []Change
	for _, c := range e.Plan.Changes {
		if c.Destructive {
			steps = append(steps, c)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "automigrate refused: %d step(s) can lose data, and nothing was applied.\n", len(steps))
	b.WriteString("Read them, then either set AutoOptions.AllowDestructive or take the\n")
	b.WriteString("reviewed path with `storm diff`:\n\n")
	for _, c := range steps {
		fmt.Fprintf(&b, "  %s\n      %s\n", c.SQL, c.Why)
	}
	return b.String()
}

// Destructive lets errors.As callers reach the steps without parsing the text.
func (e *DestructiveError) Destructive() []Change {
	var steps []Change
	for _, c := range e.Plan.Changes {
		if c.Destructive {
			steps = append(steps, c)
		}
	}
	return steps
}

// Auto brings the connection's database up to date with `want`, and reports
// what it applied. See the commentary at the top of this file for the safety
// properties it holds and the ones it deliberately does not.
//
// The connection must be a single session for the whole call — the advisory
// lock that serialises migrating processes is session-scoped, so a pool would
// release it on the first Acquire that lands elsewhere. Pass a pool to AutoPool
// instead and it will pin one for you.
//
// Auto restores search_path and lock_timeout before returning: applying a
// migration is not observable in the session afterwards.
//
// The connecting role needs CREATE on the database, not just on the target
// namespace. Diffing runs the model through PostgreSQL first — a scratch schema
// created, read back and dropped — because the server rewrites every expression
// it stores and only catalog form compares against catalog form. See the
// commentary in normalize.go.
func Auto(ctx context.Context, c *pgx.Conn, want *schema.Schema, o AutoOptions) (Result, error) {
	ns := o.namespace()
	if err := validIdent(ns); err != nil {
		return Result{}, err
	}
	// What the model says for another dialect's sake cannot be expressed here,
	// and finding that out from a failed ALTER on a live database — after the
	// steps before it committed — is the wrong end of the process.
	if err := pgddl.Check(want); err != nil {
		return Result{}, err
	}

	// Preserve the caller's session settings: the same rule Normalize holds for
	// search_path, extended to lock_timeout because the concurrent steps have no
	// transaction to scope a SET LOCAL to and must set it on the session. The
	// connection AutoPool borrows goes back to the pool and serves application
	// queries for the rest of its life; it must go back as it came.
	var prevPath, prevLock string
	if err := c.QueryRow(ctx, "SHOW search_path").Scan(&prevPath); err != nil {
		return Result{}, fmt.Errorf("read search_path: %w", err)
	}
	if err := c.QueryRow(ctx, "SHOW lock_timeout").Scan(&prevLock); err != nil {
		return Result{}, fmt.Errorf("read lock_timeout: %w", err)
	}
	defer func() {
		// NOT ctx. Cleanup must not depend on the context that may be the very
		// reason we are unwinding: a cancelled ctx fails every Exec on this
		// connection, so the restore silently does nothing and — through
		// AutoPool — the connection rejoins the pool still carrying storm's
		// search_path. Cleanup gets its own deadline for the same reason.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		_, _ = c.Exec(ctx, "SELECT set_config('search_path', $1, false)", prevPath)
		_, _ = c.Exec(ctx, "SELECT set_config('lock_timeout', $1, false)", prevLock)
	}()

	waited, err := lock(ctx, c, ns, o)
	if err != nil {
		return Result{Waited: waited}, err
	}
	res := Result{Waited: waited}
	defer func() {
		// NOT ctx, for the reason above and one worse: an advisory lock is held
		// by the SESSION, and AutoPool's session goes back into the pool. A
		// cancelled migration that failed to unlock would leave every later
		// caller waiting out LockWait against a lock nothing is using.
		//
		// The common case saves itself — pgx closes a connection whose query
		// was cancelled mid-flight, and PostgreSQL drops session advisory locks
		// on disconnect. This is for the case that does not: a deadline that
		// expires BETWEEN statements fails the next Exec client-side, without
		// touching a connection that is still perfectly healthy and about to be
		// released back into the pool holding the lock.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if _, err := c.Exec(ctx, "SELECT pg_advisory_unlock($1)", advisoryKey(ns)); err != nil {
			o.logf("storm: releasing the migration lock failed (it is released on disconnect): %v", err)
		}
	}()

	// Introspecting a namespace that does not exist reports an empty schema,
	// which would make the plan CREATE TABLE into nowhere. So it is created
	// first — and INSIDE the lock, which is not where it started out.
	//
	// CREATE SCHEMA IF NOT EXISTS is not atomic: it looks in the catalog and
	// then inserts, so two sessions arriving together both find it missing and
	// the loser fails on pg_namespace_nspname_index with a duplicate key. That
	// is not a hypothetical — it is what four processes starting at once did on
	// the first run of TestAuto_ConcurrentProcessesApplyOnce. "Idempotent" and
	// "safe to race" are different properties, and IF NOT EXISTS only claims
	// the first.
	if _, err := c.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+ns); err != nil {
		return res, fmt.Errorf("create schema %s: %w", ns, err)
	}

	// AFTER the lock. A replica that waited its turn must diff against what the
	// winner just applied, or it applies the same plan a second time — and
	// CREATE TABLE is not idempotent.
	plan, err := ForWith(ctx, c, ns, want, Options{Concurrently: o.Concurrently})
	if err != nil {
		return res, err
	}
	if plan.Empty() {
		o.logf("storm: schema %s already matches the model", ns)
		return res, nil
	}
	if plan.Destructive() && !o.AllowDestructive {
		return res, &DestructiveError{Plan: plan}
	}

	// The plan splits three ways, and the order of the three is the whole point.
	//
	//   1. enum label additions, committed FIRST and on their own. PostgreSQL
	//      runs ALTER TYPE ... ADD VALUE inside a transaction happily but
	//      refuses to let anything USE the new label until that transaction
	//      commits — SQLSTATE 55P04, "unsafe use of new value". A plan that
	//      adds a label and then defaults a column to it therefore cannot be
	//      one transaction, no matter how much one would prefer it. Committing
	//      the additions separately costs little: a label cannot be removed
	//      again anyway (PostgreSQL has no DROP VALUE), so there was never a
	//      rollback to give up.
	//   2. everything else, as ONE transaction.
	//   3. the steps that cannot be in a transaction at all — CREATE INDEX
	//      CONCURRENTLY. They go last, and safely: Plan.Concurrently never
	//      rewrites an index on a table the same plan creates.
	var enums, inTx, alone []Change
	for _, ch := range plan.Changes {
		switch {
		case ch.addsEnumValue:
			enums = append(enums, ch)
		case ch.NoTransaction:
			alone = append(alone, ch)
		default:
			inTx = append(inTx, ch)
		}
	}

	for _, group := range [][]Change{enums, inTx} {
		if len(group) == 0 {
			continue
		}
		applied, err := applyTx(ctx, c, ns, group, o)
		res.Applied = append(res.Applied, applied...)
		if err != nil {
			return res, err
		}
	}
	for _, ch := range alone {
		if err := applyAlone(ctx, c, ns, ch, o); err != nil {
			return res, err
		}
		res.Applied = append(res.Applied, ch)
	}
	o.logf("storm: applied %d step(s) to schema %s", len(res.Applied), ns)
	return res, nil
}

// applyTx runs every transactional step in one transaction. PostgreSQL's DDL is
// transactional, so this is all-or-nothing: the schema either moves to the model
// or does not move at all. The returned changes are the ones that COMMITTED —
// on failure that is none of them, which is the point.
func applyTx(ctx context.Context, c *pgx.Conn, ns string, steps []Change, o AutoOptions) ([]Change, error) {
	tx, err := c.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := bound(ctx, tx, ns, o, true); err != nil {
		return nil, err
	}
	for _, ch := range steps {
		if _, err := tx.Exec(ctx, ch.SQL); err != nil {
			return nil, fmt.Errorf("%w\n  while applying: %s\n  nothing was applied: the transaction rolled back", err, ch.SQL)
		}
		o.logf("storm: %s", ch.SQL)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit migration: %w", err)
	}
	return steps, nil
}

// applyAlone runs one CREATE/DROP INDEX CONCURRENTLY, which refuses to run
// inside a transaction block. There is no rollback here — that is the trade the
// Concurrently option makes, and why a failure leaves the earlier steps applied.
func applyAlone(ctx context.Context, c *pgx.Conn, ns string, ch Change, o AutoOptions) error {
	if err := bound(ctx, c, ns, o, false); err != nil {
		return err
	}
	if _, err := c.Exec(ctx, ch.SQL); err != nil {
		return fmt.Errorf("%w\n  while applying (outside a transaction): %s\n  the steps before it are applied and stay applied", err, ch.SQL)
	}
	o.logf("storm: %s", ch.SQL)
	return nil
}

// execer is the slice of pgx that bound needs, so it can set the bounds on a
// transaction (SET LOCAL) and on the session (SET) alike.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// bound sets search_path and lock_timeout for the steps that follow.
//
// set_config rather than SET, because SET takes no placeholders and set_config
// does: the namespace reaches the server as a bind parameter and never as SQL
// text. `local` chooses between the transaction (SET LOCAL) and the session
// (SET) — the concurrent steps have no transaction to scope a setting to, which
// is why Auto restores both settings on the way out.
func bound(ctx context.Context, e execer, ns string, o AutoOptions, local bool) error {
	if _, err := e.Exec(ctx, "SELECT set_config('search_path', $1, $2)", ns, local); err != nil {
		return fmt.Errorf("set search_path to %s: %w", ns, err)
	}
	if d := o.lockTimeout(); d > 0 {
		ms := d.Milliseconds()
		if ms == 0 {
			ms = 1 // a sub-millisecond bound is not "no bound"
		}
		if _, err := e.Exec(ctx, "SELECT set_config('lock_timeout', $1, $2)",
			strconv.FormatInt(ms, 10), local); err != nil {
			return fmt.Errorf("set lock_timeout: %w", err)
		}
	}
	return nil
}

// lock serialises migrating processes on a session advisory lock, and reports
// how long it waited.
//
// pg_try_advisory_lock in a loop rather than pg_advisory_lock, because the
// blocking form has no timeout of its own: lock_timeout does not apply to it,
// so the only bound available would be cancelling the context, and a caller who
// passed context.Background() would wait for ever.
func lock(ctx context.Context, c *pgx.Conn, ns string, o AutoOptions) (time.Duration, error) {
	key := advisoryKey(ns)
	start := time.Now()
	deadline := start.Add(o.lockWait())
	for attempt := 0; ; attempt++ {
		var got bool
		if err := c.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&got); err != nil {
			return time.Since(start), fmt.Errorf("take the migration lock: %w", err)
		}
		if got {
			if attempt > 0 {
				o.logf("storm: took the migration lock after %s", time.Since(start).Round(time.Millisecond))
			}
			return time.Since(start), nil
		}
		if !time.Now().Before(deadline) {
			return time.Since(start), fmt.Errorf("%w: waited %s for schema %s",
				ErrLockBusy, time.Since(start).Round(time.Millisecond), ns)
		}
		if attempt == 0 {
			o.logf("storm: another process is migrating schema %s; waiting up to %s", ns, o.lockWait())
		}
		select {
		case <-ctx.Done():
			return time.Since(start), ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

const (
	pollInterval = 100 * time.Millisecond
	// cleanupTimeout bounds the unlock and the session restore. They run on a
	// context detached from the caller's, so something has to stop them.
	cleanupTimeout = 5 * time.Second
)

// advisoryKey is the lock every storm process migrating this namespace agrees
// on. It must be stable across builds and versions — two replicas on different
// storm versions during a rolling deploy are exactly the case the lock exists
// for — so it is a fixed hash of a fixed string, never anything derived from
// the model.
func advisoryKey(ns string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("storm:automigrate:" + ns))
	return int64(h.Sum64())
}
