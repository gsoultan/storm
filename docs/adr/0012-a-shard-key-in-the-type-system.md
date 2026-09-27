# ADR-0012 — A shard key in the type system

**Status:** Accepted · 2026-09-27 · depends on [ADR-0011](0011-a-transaction-contract-beside-the-port.md)

Split from ADR-0011's first draft, which decided transactions and sharding
together. Sharding depends on the transaction contract (a `shard.Set` holds
`runtime.DB` values and starts transactions with `StartTx`) and not the
reverse, so it is decided and landed second.

## Context

Sharding had no representation at all. An adopter splitting a table across
databases routed by hand, and nothing in storm could tell a routed executor
from a pool. Every other storm rule has a server behind it as a backstop — a
bad index fails at `CREATE`, a bad predicate at `PREPARE`, a violated
constraint at `INSERT`. A query sent to the wrong shard fails at nothing: each
shard holds an ordinary table whose constraints are all satisfied, the read
returns rows, and the write lands where the row will never be found again. So
the check has to live before run time, or nowhere.

## Decision

### 1. A shard key is a column in the model and a TYPE in the generated code

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

### 2. The escape hatch gets a sharded form

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

### 3. What sharding does not do

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

**Good.** Generated code is unchanged for unsharded models. For sharded ones
the compiler enforces what no constraint could, and `examples/tenants/testdata/compilefail`
asserts that it still does: a query on a pool does not build, and neither does
a hand-made type posing as a `Bound`.

**Bad.** `shard.Bound`'s unexported method means an adopter cannot write their
own `Bound`. `shard.Pin` is the sanctioned door and covers the two cases that
need it: a `CountingExecutor` in a round-trip test, and a sharded model deployed
on one database today.

The folder ratchet shaped where the code lives. `internal/archcheck` held the
root at 17 files and `codegen/` at 27, so the sharded escape hatch is two files
(one struct each), the declaration-time checks share one `validate.go`, and
`codegen`'s refusal of plain SQL on a sharded table sits in `rawscan.go` beside
the helpers it reuses.

It is experimental in v1.3 (docs/STABILITY.md), with one commitment that is not:
widening a sharded model's calls back to `runtime.Executor` would silently
un-check every routed query, and will not happen.

**Reversible?** The package is additive. The generated signature change for
sharded models is not: it is the feature.
