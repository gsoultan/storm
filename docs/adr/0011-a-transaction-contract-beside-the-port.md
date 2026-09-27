# ADR-0011 — A transaction contract beside the port

**Status:** Accepted · 2026-09-27 · amends [ADR-0005](0005-executor-port-width.md)

Sharding was decided in this ADR's first draft too. It depends on this
decision and not the reverse, so it is its own, ADR-0012, and lands after.

## Context

ADR-0005 decided the `Executor` port is four methods and closed with:

> **Transactions stay out of the port.** A transaction is an `Executor` you were
> given, not a method you call on one.

That sentence is still right and this ADR does not reverse it. What it got
wrong is what got built on it: "out of the port" was read as "out of the
library", and so each adapter grew its own transaction vocabulary. Before
this ADR:

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
    StartTx(ctx context.Context) (Tx, error)
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

`runtime.InTx` handles the three endings once, including the panic: fn's error
wins over the rollback's, and a panic rolls back and continues.

### 2. The method is `StartTx`, and the name is a compatibility decision

The first draft called it `Begin` and changed `mydrv.Pool.Begin` and
`msdrv.Pool.Begin` to return `runtime.Tx` instead of the `*Tx` they shipped
with in v1.1.0. That is an incompatible change to a v1 API, and
`scripts/check/apicompat.sh` rejected it the day it was written. Go has no
covariant returns, so no method named `Begin` on `DB` can be satisfied by a
pool whose own `Begin` returns a concrete type.

So `Begin` stays exactly as v1.1.0 shipped it, and every adapter gains
`StartTx`. It is not `BeginTx` either, because inside `runtime/sqldrv` that
would sit beside database/sql's `BeginTx(ctx, *TxOptions)`, a different method
with the same name one layer down.

| adapter | start a transaction | notes |
|---|---|---|
| `pgxdrv` | `pool.StartTx(ctx)` | the pgx lines stay in the adapter |
| `mydrv` | `pool.StartTx(ctx)` | `Begin` → `*Tx` unchanged |
| `msdrv` | `pool.StartTx(ctx)` | `Begin` → `*Tx` unchanged |
| `sqldrv` | `sqldrv.NewPool(db).StartTx(ctx)` | `NewTx(tx)` for a caller's own `*sql.TxOptions` |

### 3. One `ErrTxDone`

Every adapter's `ErrTxDone` IS `runtime.ErrTxDone`, so `errors.Is` against the
shared one holds for every adapter. A second commit is `ErrTxDone`. A rollback
of a finished transaction is nil, which is what makes `defer tx.Rollback(ctx)`
beside a commit the idiom.

### 4. The root package names them too

`storm.Tx`, `storm.DB`, `storm.Executor`, `storm.ErrTxDone`, `storm.InTx` and
`storm.Retryable` are aliases and forwarders, in `types.go` beside `Decimal` and
`JSON`, for the same reason those are there: an adopter declaring a model
already imports `storm`.

## Consequences

**Good.** A repository method can take a `storm.DB` and run on any target. The
panic path in `InTx` is written once instead of in every adopter.

**Bad.** Adapters gain a method beyond the port. That is a real widening of
what a future adapter owes, bounded by the fact that a driver with no
transactions cannot satisfy `DB` and does not have to. mydrv and msdrv now
have two ways to start a transaction — `Begin`, returning their own `*Tx`, and
`StartTx` — which is the price of keeping v1.1.0 code compiling. `Begin` can be
deprecated in favour of `StartTx` only in a major.

The message of mydrv's and msdrv's `ErrTxDone` changed, from "mydrv: …" and
"msdrv: …" to runtime's "storm: …". Comparing against the variable, or
`errors.Is`, is unaffected. apidiff does not read messages, so this one is
recorded here and in the CHANGELOG instead.

**Reversible?** The interfaces are additive and could be deprecated.
