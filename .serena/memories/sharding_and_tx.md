# storm — the transaction contract and sharding (2026-09-23)

Landed together because they share one decision: what belongs in the
`Executor` port, and what belongs *beside* it. ADR-0011 is the record;
this is what a reader needs before touching either.

## The correction to ADR-0005

ADR-0005 said "transactions stay out of the port" and that is still true —
the port is four methods, budget five. What went wrong is that **"out of the
port" got read as "out of the library"**: every adapter grew its own
transaction type, so a repository method could not name what it took. pgx
needed `pgxdrv.Tx{T: tx}` written in the adopter's handler, which put the two
lines naming pgx in the one place `AGENTS.md` forbids them.

`runtime.Tx` and `runtime.DB` name what a **caller** holds. Generated code
still takes `Executor`, because most of what storm is handed is not a pool.

**Nothing type-asserts an `Executor` to a `DB`.** That is why `sqldrv` gained
a `Pool` TYPE rather than teaching `Exec` to ask what handle it holds —
`sqldrv.DB` is an interface that `*sql.DB`, `*sql.Tx` and `*sql.Conn` all
satisfy, and only one can begin.

`InTx` exists for the ending hand-written helpers forget: a panicking `fn`
left the transaction open with its connection checked out. Proven against a
pool of ONE, where a leak hangs instead of failing.

## Why the shard key is a type

Every other storm rule has a server behind it as a backstop — a bad index
fails at `CREATE`, a bad predicate at `PREPARE`, a violated constraint at
`INSERT`. **A query sent to the wrong shard fails at nothing.** Each shard
holds an ordinary table whose constraints are all satisfied; the read returns
rows, just not all of them, and the write lands somewhere the row will never
be found again.

So `shard.Bound` is a parameter type, not a runtime check. `shard.Pin` is the
sanctioned door for a `CountingExecutor` in a round-trip test, and for a
sharded model deployed on one database today.

## Four places the guarantee leaked, found by re-reading the diff

All four shipped in the same session as the feature, which is the lesson: the
headline claim was true for the typed query path and false everywhere else.

1. `execType()`'s comment **claimed unions were refused** and nothing did it.
2. **Joins were unvalidated** — `Join.Tables` and `CTE.Table` reach tables the
   relation graph does not.
3. **`storm.SQL` took a plain `Executor`**, so the escape hatch un-checked
   every routed query. Fixed with `storm.ShardedSQL` + a generate-time refusal
   of a plain statement whose text names a sharded table.
4. Generated **`NewUnit()` returned the non-checking `runtime.Unit`**, so the
   cross-shard refusal existed in `runtime/shard` and never fired where the
   writes are.

## The fifth leak, found by probing rather than reading

An **implicit many-to-many** between sharded tables. The join table storm
synthesizes holds exactly `(author_id, book_id)` — there is no third column
and none could be inferred — so it was unsharded, its package took a plain
`Executor`, and a link row could be written to any database while both rows it
joins lived on one. On the table whose only job is to join two sharded tables.

Found by generating a fixture and printing `sharded=` per table, not by
reading the validator. The first probe used a shard key coincidentally named
`tenant_id`, the same as a link FK, which made the output ambiguous — the
second used `org_id` and settled it. Worth repeating: when a probe's fixture
shares a name with the thing under test, the probe is the thing to fix first.

Refused now, pointing at `t.Through`. storm will not invent the column: a
shard key says which VALUE decides where a row lives, and synthesizing one
means choosing that value from a row nobody declared.

## The sixth leak — and why an example is a test

`examples/tenants` was written as documentation and found a **compile error**
in the first minute: the context package's PLAN types took a plain
`runtime.Executor` while calling into methods needing a `shard.Bound`.

The cause is worth remembering more than the fix. `execType()` reads `g.t`,
the table being generated — and the CONTEXT file has no single table, so
`g.t` is nil there and every plan got the unsharded answer. The fix scopes it
to the plan's driving table for the whole emission (`g.inPlan`), because one
executor is threaded through the entire chain of member loaders.

**Why no test caught it:** the codegen fixture was a sharded model with NO
relation, and a plan type is only emitted when there is one. Six tests
asserted signatures on a model that could not exercise the code path. The
fixture now has a relation.

The general lesson: an example is not documentation that happens to compile,
it is the only test written from the adopter's side, and it exercises
combinations a fixture chosen for one feature never will.

## The measured mistake

`Set.For` allocated **24 B / 1 alloc per call** — on the path of every query
against every sharded table — while the package doc already claimed routing
allocated nothing. Returning a `Bound` boxes a struct wider than a word.
A `Set` now builds one `Bound` per shard at `New`: 48.9 ns → 25.7 ns, 1 → 0
allocs, asserted by `TestRoutingDoesNotAllocate`.

**The claim was in a doc comment before it was ever measured.** That is the
shape to watch for, not this instance of it.

## Refused, and why

- **Fan-out.** A correct merge re-applies `ORDER BY`, `LIMIT` and keyset
  pagination across streams, and has no correct answer for `AVG`, a window
  function or `COUNT DISTINCT`. `Set.Each` makes the caller say what
  combining means — summing per-shard counts is sound, averaging per-shard
  averages is not, and writing it out is where you notice.
- **Cross-shard transactions.** `shard.Unit` refuses at `Add`, while the code
  that chose the keys is still on the stack.
- **Two-phase commit.** Needs a durable coordinator and recovery: deployed,
  and storm is imported.
- **`Set.Migrate`.** `migrate.AutoPool` takes a `*pgxpool.Pool`; migration
  happens before a `Set` exists. Nothing makes N shards atomic with each
  other, and storm says so rather than pretending.

## Testing note that generalises

The sharding suite ran green on stubs and proved nothing about routing. The
live test (two real databases) was **mutation-checked twice**: an
everything-to-shard-0 mutant, and a subtler shift-by-one that the first
version of the test did NOT catch, because it only proved `For` was
self-consistent. The fix was to assert the Bound against the locator computed
independently. A stub catches a wrong call; only two databases catch a wrong
answer.

See [[write_path]] for the Unit, [[boundaries]] for the import rules,
[[automigrate]] for what `Auto` guarantees per database.

## Landed, 2026-09-27 (storm-7a, from the parked wip/sharding-tx f2332a2)

Two PRs, transactions first (ADR-0011) and sharding second (ADR-0012), because
sharding depends on runtime.DB and not the reverse.

- **`Begin` → `StartTx` on runtime.DB and on shard.Set.** mydrv's and msdrv's
  v1.1.0 `Begin(ctx) (*Tx, error)` stays, and Go has no covariant returns;
  apicompat.sh rejected changing it. `BeginTx` was avoided because it reads as
  database/sql's `BeginTx(ctx, *TxOptions)` inside sqldrv. The StartTx wrappers
  return a literal nil on error: `(*Tx)(nil)` through an interface is non-nil.
- **The archcheck ratchet decided file placement:**
  - runtime/tx.go and db.go, with room made by moving inet's decoders into
    decode.go/array.go and expandRowCmp into tree.go
  - root re-exports in types.go
  - aggvalidate, indexvalidate and shardvalidate merged into validate.go;
    reflectstr.go into term.go
  - shardedsql.go and shardedsqlexec.go, one struct each
  - codegen/shardsql.go into rawscan.go
  - runtime/shard/bound.go and boundtx.go
- **Tests added while landing**, closing storm-2e's handoff list:
  - compile-fail fixtures in examples/tenants/testdata/compilefail: a pool is
    not a shard, and a Bound cannot be forged
  - sqldrv tx tests: a fake driver counts endings and records TxOptions
  - live StartTx tests on mydrv and msdrv
  - discovery of ShardedSQL in every declaration form (fails against the old
    discovery)
  - a live successful Unit flush
  - live SQL Server routing, with its own CI step
  - an end-to-end `storm generate` refusal of plain SQL on a sharded table
- **A flaky test, fixed.** TestShardKeyOfFollowsTheColumnType kept whichever
  .gen.go a map range saw LAST (Go randomises it). 10 of 40 runs failed; it
  now selects by package name, and the names it carried had been wrong but
  never read.
- **The MySQL and SQL Server shard harnesses now t.Fatal when their address
  is set and the server refuses.** A Skip there reports a broken suite as
  passed.
- **Sharding is experimental in v1.3** (STABILITY.md), except that sharded
  calls will never widen back to runtime.Executor.
- **Still unexplained (reported by storm-2e): one `make check` failure on
  2026-09-23.** It happened BEFORE the Currency change that made the ShardKeyOf
  test flaky, so that fix does not explain it. It did not reproduce in 8 later
  runs. Capping the live-test pools at MaxConns=2 was a guessed mitigation, not
  a diagnosis. The best unverified guess is a concurrent edit in the shared
  checkout during the run, the hazard [[shared-working-tree]] names. If a
  live sharding test fails intermittently again, start here, and do not file
  it under the ShardKeyOf fix.
