# storm — automigrate (`migrate.Auto`, 2026-09-07)

storm applies its own DDL now. `migrate.Auto(ctx, *pgx.Conn, *schema.Schema,
AutoOptions)` and `migrate.AutoPool(ctx, *pgxpool.Pool, ...)` in
`migrate/auto.go` + `migrate/autopool.go`. ADR-0001 forbade this by name and
carries a dated **amendment** section explaining the reversal.

## Why the ban did not survive contact

ADR-0001 said "silent production schema change" and the load-bearing word was
**silent**. The first draft of that ADR conflated *where the schema is declared*
with *who applies DDL*; the second conflated *who applies it* with *how
dangerous that is*. Applying DDL is not the danger. Applying it implicitly,
unserialised, in part, or without a gate on data loss is — and each is
separately addressable. That reasoning is the reusable part.

## The five properties (each is a test in `migrate/auto_test.go`)

1. **Never implicit** — no `init`, no hook, no path a query reaches.
2. **Never destructive by accident** — `*DestructiveError` and **nothing**
   applied, not "the safe steps applied"; a half-applied schema is worse than an
   unapplied one. `AllowDestructive` opts in.
3. **Never twice** — session advisory lock, key `fnv64a("storm:automigrate:"+ns)`.
   That key is a **wire format**, not an implementation detail: two replicas on
   different storm versions during a rolling deploy must agree on it, so the
   test recomputes it from the rule rather than asking the package.
4. **Never in part** — one transaction for all transactional steps.
   `CREATE INDEX CONCURRENTLY` cannot join it and runs after, one at a time.
5. **Never a lock queue** — `lock_timeout` 3s default. **No default
   `statement_timeout`, deliberately**: waiting blocks *other* sessions, running
   long only costs the migration. That asymmetry is the whole rule.

**The plan is computed AFTER the lock is taken.** Replicas that queued re-diff
against the winner's work and find nothing to do. Computing it before the lock
would make every loser apply a plan that had already been applied.

## Two defects the concurrency tests found — both worth remembering

- **`CREATE SCHEMA IF NOT EXISTS` is not atomic.** It reads the catalog and then
  inserts; four processes arriving together all saw it missing and three died on
  `pg_namespace_nspname_index`. It had been put *before* the lock because it is
  idempotent — and it is. **"Idempotent" and "safe to race" are different
  properties, and `IF NOT EXISTS` only claims the first.** Same trap applies to
  `CREATE TABLE IF NOT EXISTS`.
- **Session settings ride a pooled connection back into the pool.** The
  concurrent-index path must set `lock_timeout` on the SESSION (a
  `CREATE INDEX CONCURRENTLY` has no transaction to scope `SET LOCAL` to), and
  that setting stayed on the connection after `AutoPool` released it. `Auto`
  now reads and restores both `search_path` and `lock_timeout`.

## The enum trap (worth remembering beyond storm)

`ALTER TYPE ... ADD VALUE` runs inside a transaction fine, but PostgreSQL
refuses to let anything **use** the new label until that transaction commits —
SQLSTATE **55P04**, "unsafe use of new value". So "add a status and default a
column to it" cannot be one transaction. Auto applies the plan in **three**
groups, and the order is load-bearing: enum additions (own transaction, first) →
everything else (one transaction) → `CREATE INDEX CONCURRENTLY` (no transaction,
last). Committing labels separately costs nothing that existed: there is no
`DROP VALUE`, so there was never a rollback to give up.

**`storm diff` has the same defect and it is NOT fixed** (as of 2026-09-07): it
writes `ADD VALUE` and the step using it into one `.up.sql`, and golang-migrate
wraps a file in a transaction. `Change.addsEnumValue` carries the fact; the fix
is a file of its own, exactly as `NoTransaction` steps already get one.

## Verified against schemas storm did not write

- **argus** — its 10 hand-written SQL migrations applied to a scratch database,
  introspected into the IR, and `Auto` rebuilt all 10 tables from nothing. Round
  trip clean, idempotent. 12 steps, 40ms.
- **anubis** — the real model shipping on v0.6.3: applies, verifies clean,
  idempotent.

Trap when writing such a harness: use `migrate.For`, **not**
`Diff(Introspect(live), model)`. The server rewrites what it stores (`''` →
`''::text`), so raw model form never compares equal to catalog form and the
harness reports drift that is not there. `For` normalises the model through
PostgreSQL first. This cost a false "3 pending changes" against anubis.

## Measured, not assumed

- **pgx CLOSES a connection whose query is cancelled mid-flight**, and
  PostgreSQL drops session advisory locks on disconnect. So the cancellation
  test passes with or without the detached-context cleanup — it is kept for the
  user-visible property, and the comment says so rather than overclaiming. The
  window the detaching really covers is a deadline expiring *between*
  statements: a client-side failure on a connection that is still healthy and
  about to be pooled.
- `set_config(name, value, is_local)` takes **bind parameters**, where `SET`
  does not. Used throughout so the namespace never reaches the server as SQL
  text — better than the interpolate-after-`validIdent` that `Normalize` does.

## The boundary that had to be rewritten

`migrate/` was exempt from "pgx lives in `runtime/pgxdrv`" *because it was
build-time code*, and it is not any more. The narrower property that replaced it
is machine-checked, not asserted: `scripts/check/boundaries.sh` fails if pgx
reaches the **root package's** dependency closure. So `import
"github.com/gsoultan/storm"` links no driver and automigrate stays opt-in.
Verified to trip both ways. Note the older file-level grep would NOT have caught
this — a root file importing `storm/migrate` names no pgx and slips past it;
only the `go list -deps` check sees the transitive edge.

`storm diff` is unchanged and remains the recommended path for a production
database with data in it. Auto is for tests, laptops, ephemeral environments,
CI, single-instance deployments.

Auto needs **CREATE on the database**, not just on the namespace: diffing runs
the model through a scratch schema first (see `normalize.go` — the server
rewrites every expression it stores, so only catalog form compares to catalog
form).

Related: [[decisions]] (ADR-0001 and the rejected list), [[core]], [[boundaries]].
