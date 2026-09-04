# Query expressiveness — where the declared surface ends

Read with [[production_readiness]] and `docs/COMPLEX-QUERIES.md`, which is the
as-built map (10 scenarios, and a table of what is deliberately not declarable).

## Closed in v0.5.0 (2026-09-05)

**Top-N by a measure.** `AggregateBuilder.OrderAsc/OrderDesc(Out)` →
`schema.Aggregate.OrderBy` → `pgsql.AggregateSuffix`. Ordering names an OUTPUT
ALIAS, because PostgreSQL resolves a bare ORDER BY name against the select list
first — same plan as repeating the aggregate, and a grouping set's subtotal
NULLs stay visible. **The grouping columns are appended as a tiebreak**: a
measure is not unique and a top-N report is the query that pages.

**That tiebreak is asserted on the SQL, not on a live page walk.** The live
walk was written first and does NOT trip when the tiebreak is removed —
PostgreSQL's sort is stable at fixture size. Verified by deleting the tiebreak
and watching the live test stay green three times. The lowering test does trip.
General lesson for this repo: an ordering property needs a string assertion,
because the server will hide it at small N.

**`And` + `AnyOf`** on the generated Query: `(a AND b) OR (c AND d)`, which
`Any` (ORs single Preds) could not say. **The runtime needed nothing** — the
postfix stream's KAnd/KOr already carry arity and nest; only the builder could
not emit it. `unwrapOuter` strips the outermost parens, so the expected SQL is
`WHERE (a AND b) OR (c AND d)`, NOT double-wrapped.

**`codegen.Budgets{Scale}`** scales every per-query buffer together (16 toks,
4 order terms, 6 strs/nums, 3 lists at Scale 1). Zero value is byte-identical.
Query size is pinned by `bench.TestQuerySize_HasATripwire`, so raising Scale is
a deliberate trade.

**`reservedNames` in codegen/gen.go** refuses a column whose exported name is
one the generated package declares. Adding `And` created the hazard; `Query`
and `Row` always had it and failed as a compile error against generated code.

## Still not declarable, and why (all cost a declaration, not type safety —
## `storm.SQL[T]`/`SQLExec` stay PREPARE-verified at generate time)

- **set-based `UPDATE`/`DELETE … WHERE`** — writes are per row or batched per row
- **row locking** (`FOR UPDATE`, `SKIP LOCKED`) — the version column covers the
  lost update, not the queue-worker claim
- **streaming/`Iter`** — `All` materialises; an export pages with `After`
- **jsonb path extraction** (`->>`, jsonpath) — containment/key tests only
- **probes across two DIFFERENT relations** — one child column range; the split
  is deferred deliberately (see the `MaxCols` entry in CHANGELOG v0.5.0)
- **set-returning `FROM`** / gap-filled series — ADR-0009, Proposed only
- **faceted search in one round trip** — branches would need a widest-common row

## Environment trap (2026-09-05)

`make check` needs `DSN=…@127.0.0.1:5434/storm` on this machine — see
[[production_readiness]]. The Makefile now lets an exported `STORM_DSN` win.
