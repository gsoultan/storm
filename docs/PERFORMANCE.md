---
tags: [storm, performance, slo]
updated: 2026-08-23
status: **M0 measured** — see `bench/RESULTS.md`; budgets below revised by it
---

# Performance: budgets, forbidden things, and the harness

> **Nothing in this file is a measured result.** Every number is a *target*.
> Measured results live in `bench/RESULTS.md` — re-run `make bench`, never
> quote from memory.

> **Revised by M0 (2026-08-23).** Allocation budgets are now stated **above the
> driver floor**. `pgx.Query` costs 5 allocs and 404 B before an ORM does
> anything, so an end-to-end budget of "≤3 allocs" was unmeetable by
> construction. M0 measured **3 allocs above the floor** for an 8-column row —
> the three unavoidable `string()` copies — against 13 for an idiomatic pgx
> scan.

## Budgets

Reference workload: **co-located Postgres over a unix socket**, warm pool,
prepared statements enabled, 8-column table. `raw pgx` means hand-written pgx
with a hand-written scanner.

**The socket matters.** M0 ran against Postgres in a VM: round-trip was 63.7 µs
and storm's entire CPU cost is ~45 ns, so 95.5% of wall time was network and
the wall-clock gate passed without testing much. Wall-clock budgets are only
meaningful on a low-latency connection.

| Operation | Wall-clock target | Allocation target |
|---|---|---|
| Single-row PK select | ≤ **1.15×** raw pgx | ≤ **3 allocs/op above the driver floor** |
| 1,000-row scan | ≤ **1.10×** raw pgx | **no per-row allocation beyond string columns** (M0: 3/row vs pgx 5/row) |
| Dynamic query, 6 optional filters, **warm** shape | ≤ **200 ns** builder overhead | **0 allocs** for SQL construction |
| Dynamic query, **cold** shape (first ever) | ≤ **25 µs**, once per shape per process | unbounded, once |
| Has-many, 1 parent + 50 children | — | **exactly 2 round trips** (1 with `LATERAL`), never 51 |
| Insert 1,000 rows | ≤ **2×** raw `COPY` | one `COPY` or one `pgx.Batch` |
| Dirty-set computation on update | — | **0 allocs** |

Two of these are load-bearing for the thesis. **0 allocs on the warm dynamic
path** is the claim no other ORM can make. **Exactly 2 round trips** is the
claim that makes N+1 structurally impossible rather than merely discouraged.

## Forbidden — perf vetoes

Each is CI-enforced, not review-enforced.

| Forbidden | Why | Gate |
|---|---|---|
| `reflect` anywhere under `runtime/` | one reflection path becomes *the* path | `scripts/check/no-reflect.sh` |
| `fmt.Sprintf` in query construction | formatting on a hot path, and an injection surface | `scripts/check/no-fmt-sql.sh` |
| `rows.Scan(&a, &b, …)` internally | boxes every column into `any` — one alloc per column per row | lint rule |
| a map lookup per query on the warm path | hashing a key you already computed as a bitmask | benchmark + review |
| a fresh arg slice per call where a pooled array fits | the allocation the whole design exists to remove | `-benchmem` regression gate |
| a query on a hot path without `EXPLAIN (ANALYZE, BUFFERS)` | plan regressions are invisible until production | `storm explain` in CI |
| a benchmark whose capacity/config differs between sides | *"a benchmark comparison must match capacity on both sides"* | review |
| quoting a number that was not re-measured this run | numbers rot | review |

## The harness

```
bench/
  storm_test.go     the subject
  pgx_test.go       the floor
  sqlc_test.go      the baseline to beat (current anubis tooling)
  bun_test.go       best-in-class runtime ORM
  ent_test.go       codegen rival
  gorm_test.go      the popularity baseline
  RESULTS.md        regenerated, never hand-edited
```

- `go test -bench . -benchmem -count=10` + `benchstat` for every comparison.
- Every rival runs the **same workload against the same schema on the same
  pool config**. Differing capacity invalidates the comparison entirely.
- Round-trip counts asserted by a counting decorator over `Executor`, not
  inferred from timings.
- Correctness runs with `-race -shuffle=on`; concurrency tests use
  `testing/synctest` for determinism.
- Allocation targets are asserted in tests via `testing.AllocsPerRun`, so a
  regression fails the build rather than showing up in a chart.

## Expected profile, and where it will hurt

Honest prediction, to be confirmed or falsified in **M0**:

- **Simple reads** — should land very close to raw pgx. Generated scanning is
  the same code a human would write. Low risk.
- **Warm dynamic reads** — should *beat* every rival by a wide margin, because
  rivals rebuild the string every call and storm does not build it at all.
- **Cold shapes** — worse than everyone on the first call of each shape. Only
  matters for pathological shape counts, which `storm lint` is there to catch.
- **Writes** — parity with raw pgx is realistic; the win is batching and
  ordering, not per-statement speed.
- **Relation loads** — the big win versus GORM `Preload` and Ent `With`, both of
  which fan out into extra queries. Two round trips versus N+1 is an
  order-of-magnitude difference at realistic fan-out, and it does not depend on
  micro-optimisation at all.

The riskiest number on this page is **≤ 3 allocs/op** for a single-row select.
If it lands at 8, the design still wins on dynamic and relational workloads. If
it lands at 40, the thesis is wrong — see the M0 kill criterion in [[PLAN]].
