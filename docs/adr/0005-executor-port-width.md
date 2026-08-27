# ADR-0005 — The Executor port is four methods: Query, Exec, CopyFrom, Batch

**Status:** Proposed · 2026-08-24

## Context

The port shipped with two methods. `AGENTS.md` budgets it at five and says it
"stays five" — the number is a ceiling on how much of a driver's surface storm
is allowed to depend on, because everything the port names is something every
future adapter must implement and every driver upgrade can break.

M4's four remaining features all want more than `Query` and `Exec`:

| feature | what it needs |
|---|---|
| 1,000 inserts as one `COPY` | the COPY protocol — a different wire path, not a statement |
| 1,000 mixed statements in one round trip | pipelining several statements before reading any result |
| `storm.Unit` | both of the above, plus ordering |
| `ON CONFLICT` upserts | **nothing** — it is SQL on the existing insert path |

So the question is real for three of them, and the answer decides whether M4
finishes.

## Rejected: capability interfaces sniffed at run time

The idiomatic Go answer is to keep `Executor` at two methods and define optional
`Copier` / `Batcher` interfaces, type-asserting at the call site the way
`io.Copy` looks for `WriterTo`.

It is rejected, and not on taste. `docs/CONCEPT.md` and `AGENTS.md` both state
that **capabilities are negotiated at build time and never sniffed at run time**,
and that an unsupported construct is a *generation* error naming the target and
the source line. A type assertion is exactly the runtime sniff that rule
forbids, and it brings the failure mode the rule exists to prevent: a bulk load
that silently degrades to 1,000 round trips because an adapter did not implement
an interface nobody was told about. The performance claim would then be a
property of which adapter you happened to pass.

## Rejected: one general method

Collapsing COPY and batching into a single `Send([]Statement)` loses the thing
that makes COPY worth having. COPY is a different protocol, not a faster loop —
it skips statement parsing and per-row protocol overhead entirely. A design
where "1,000 inserts = one COPY" cannot be *stated* cannot be *asserted*, and
that assertion is an M4 exit gate.

## Decision

The port grows to **four** methods, leaving one of the budget unspent:

```go
type Executor interface {
    Query(ctx, sql string, args []any) (Rows, error)
    Exec(ctx, sql string, args []any) (int64, error)
    CopyFrom(ctx, table string, cols []string, src CopySource) (int64, error)
    Batch(ctx, ops []BatchOp, each func(int, Rows, int64, error) error) error
}
```

Constraints held:

- **No driver type crosses the port.** `CopySource` mirrors
  `pgx.CopyFromSource` structurally without importing it, exactly as `Rows`
  already mirrors `pgx.Rows`.
- **`Batch` is a callback rather than a `[]Result`.** pgx's batch results are
  streamed and each is valid only until the next is requested, so materialising
  them all would either copy every row or hand back handles that are already
  invalid. The callback shape is the one the driver actually supports.
- **Transactions stay out of the port.** A transaction is an `Executor` you were
  given, not a method you call on one. This keeps `Unit` composable with any
  ownership model the caller already has, and keeps three more methods
  (`Begin`, `Commit`, `Rollback`) out of the budget.

## Consequences

**Good.** Bulk load and batching are properties of the port, so they are
assertable for every adapter rather than for whichever one happens to implement
an optional interface. The remaining budget of one is a real constraint that
future features have to argue against.

**Bad.** Every adapter must now implement four methods, including two that are
more work than a passthrough. A driver without a COPY equivalent has to emulate
it with multi-row `INSERT`, and that emulation must be *stated* by the adapter,
not discovered from a benchmark. `runtime.CountingExecutor` and any user test
double get wider too — mitigated by embedding, since a decorator only overrides
what it counts.

**Reversible?** Yes, cheaply, while `pgxdrv` is the only adapter. It gets
expensive at M9 when MySQL arrives, which is the argument for deciding it now
rather than letting the first feature that needs a method add one.
