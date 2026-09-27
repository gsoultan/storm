# ADR-0011 — A transaction contract beside the port, and a shard key in the type system

**Status:** Accepted · 2026-09-23 · amends [ADR-0005](0005-executor-port-width.md)

## Context

ADR-0005 decided the `Executor` port is four methods and closed with:

> **Transactions stay out of the port.** A transaction is an `Executor` you were
> given, not a method you call on one.

That sentence is still right and this ADR does not reverse it. What it got
wrong is what got built on it: "out of the port" was read as "out of the
library", and so each adapter grew its own transaction vocabulary.

| adapter | how you begin | what you get back |
|---|---|---|
| `pgxdrv` | `pool.Begin(ctx)` on the pgxpool, then wrap | `pgx.Tx` → `pgxdrv.Tx{T: tx}` |
| `mydrv` | `pool.Begin(ctx)` | `*mydrv.Tx` |
| `msdrv` | `pool.Begin(ctx)` | `*msdrv.Tx` |
| `sqldrv` | `db.BeginTx(ctx, nil)`, then wrap | `*sql.Tx` → `sqldrv.New(tx)` |

Four spellings, two requiring a conversion before generated code accepts the
result, and one — `pgxdrv` — putting the two lines that name pgx in the
adopter's request handler, which is the one place `AGENTS.md` says pgx must not
appear. A caller could not write

```go
func (r *Repo) Transfer(ctx context.Context, db ???) error
```

and have it compile against two databases, because there was no name for the
blank. Every adopter's own `withTx` helper was therefore adapter-specific, and
each one re-derived the same three endings — error, panic, commit — with the
panic case usually missing.

Separately: sharding had no representation at all. An adopter splitting a table
across databases routed by hand, and nothing in storm could tell a routed
executor from a pool.

## Decision

### 1. `Tx` and `DB` are interfaces in `runtime`, and the port is untouched

```go
type Tx interface {
    Executor
    Commit(ctx context.Context) error
    Rollback(ctx context.Context) error
}

type DB interface {
    Executor
    Begin(ctx context.Context) (Tx, error)
}
```

`Executor` is still four methods with a budget of five. A decorator still
implements four. What changes is that the thing a CALLER holds at the top of a
request now has a name.

The two are different positions in a program and that is the whole argument:
generated code takes an `Executor` because most of what storm is handed is not
a pool — a `Tx` is not, a pinned `Conn` is not, a `CountingExecutor` is not, a
test double should not have to be. `DB` is what a caller names on purpose.

**Nothing in storm type-asserts an `Executor` to a `DB`.** That is the runtime
capability sniff ADR-0005 rejected, and the reason `sqldrv` gained a `Pool`
type rather than teaching `Exec` to discover whether its handle can begin.

`runtime.InTx` handles the three endings once, including the panic.

### 2. A shard key is a column in the model and a TYPE in the generated code

```go
func (o *Order) Schema(t *storm.Table) { t.ShardKey(&o.TenantID) }
```

A sharded model's generated calls take `shard.Bound` — an `Executor` already
resolved to one shard — instead of `runtime.Executor`. `shard.Bound` has an
unexported method, so only `shard.Set` and `shard.Pin` can produce one, and

```go
os, err := order.New().StatusEq("open").All(ctx, pool, nil)
```

does not compile.

This is the only place the check can live. Every other storm rule has a server
behind it as a backstop: a bad index fails at `CREATE`, a bad predicate fails
at `PREPARE`, a violated constraint fails at `INSERT`. A query sent to the
wrong shard fails at nothing. Each shard holds an ordinary table whose every
constraint is satisfied, and the query returns rows — just not all of them, and
a write lands somewhere the row will never be found again. There is no error to
catch, no SQLSTATE to map, and no log line to alert on.

### 3. The escape hatch gets a sharded form

`storm.SQL` takes a `runtime.Executor`. A `shard.Bound` is one, so routing a
raw statement worked — and so is a pool, so the guarantee did not. The escape
hatch was the single door out of a check the type system otherwise made
everywhere, and a door out of a compile-time guarantee is the only part of it
anyone has to get right.

`storm.ShardedSQL[T]` and `storm.ShardedSQLExec` are the same declaration,
PREPAREd and pinned the same way, with a `shard.Bound` at the call. `RawDecl`'s
unexported `decl()` widened to report which form a declaration is — a method
every declaration answers, not a type assertion, for ADR-0005's reason — and
`storm generate` refuses a plain statement whose text names a sharded table.

That refusal is a whole-identifier scan, not a parse. storm would need a SQL
parser per dialect to know for certain which tables a statement touches, and
the failure modes are not symmetric: a false positive is a build error whose
suggested fix is correct anyway, a false negative is a raw query reporting one
shard's rows as all of them.

What it still cannot check is the statement's own `WHERE`. storm routes a raw
statement to the right database; it does not read the predicate, so a query
that omits the tenant filter returns that shard's other tenants. The typed
query API is the better answer wherever it reaches, and this is why.

### 4. What sharding does not do

- **No fan-out.** A correct scatter-gather has to re-apply `ORDER BY`, `LIMIT`
  and keyset pagination across streams, and has no correct answer at all for
  `AVG`, a window function, or `COUNT DISTINCT`. `Set.Each` hands the caller
  each shard and lets them decide what combining means, because the shape of
  the combination is what shows whether it is sound. Summing per-shard counts
  is sound; averaging per-shard averages is not, and writing it out is where
  you notice.
- **No cross-shard transaction.** `shard.Unit` refuses the second shard at
  `Add`, naming both — while the code that chose the keys is still on the
  stack and nothing has been sent.
- **No two-phase commit.** It needs a durable coordinator log and a recovery
  process for in-doubt transactions. Those are deployed things, and
  `docs/CONCEPT.md`'s scope line says storm is imported, not deployed.
- **No resharding.** Moving a tenant is a data migration with a cutover.
  `shard.Jump` makes that migration small — growing 4 shards to 5 moves about
  1/5 of the keys rather than 4/5 — but it does not make it unnecessary.

## Consequences

**Good.** A repository method can take a `storm.DB` and run on any target. The
panic path in `InTx` is written once instead of in every adopter. Generated
code is unchanged for unsharded models, and for sharded ones the compiler
enforces what no constraint could.

**Bad.** `mydrv.Pool.Begin` and `msdrv.Pool.Begin` now return `runtime.Tx`
rather than `*Tx`. Both concrete types had only unexported fields and both
already carried `Commit` and `Rollback` with these signatures, so a caller who
wrote `tx, err := pool.Begin(ctx)` is unaffected; one who wrote
`var t *mydrv.Tx` is not. Adapters gain a fifth and sixth method beyond the
port, which is a real widening of what a future adapter owes — bounded by the
fact that a driver with no transactions cannot satisfy `DB` and does not have
to.

Widening `decl()` is invisible to adopters: it is unexported, so `RawDecl`
was never implementable outside the package.

`shard.Bound`'s unexported method means an adopter cannot write their own
`Bound` implementation. `shard.Pin` is the sanctioned door and covers the two
cases that need it: a `CountingExecutor` in a round-trip test, and a sharded
model deployed on one database today.

**Reversible?** The interfaces are additive and could be deprecated. The
generated signature change for sharded models is not: it is the feature.
