---
tags: [storm, architecture, design-patterns]
updated: 2026-08-23
status: proposed
---

# Architecture

storm is structured as a **compiler with a thin runtime**, not as a library with
a builder. Front end → IR → back end, plus an execution layer small enough to
audit in an afternoon.

## Data flow

```
  model/*.go (truth)┐
  migrations/*.sql ─┤─► schema front end ─► SCHEMA IR ─┤─► migration diff
  live pg_catalog ──┘        (import)                  │      ↓ reviewed .sql
                                                       │   (storm never applies)
                                                       ▼
  your Go source ──► call-site scanner ──► QUERY IR ─► compile/ ─► codegen/
   (.With chains,      (AST level)          (rel.        │            │
    typed builders)                        algebra)      │            ▼
                                                         │      generated Go:
                                                         │      · SQL fragments
                                                         │      · shape tables
                                                         │      · scanners
                                                         │      · projection types
                                                         ▼
                                              storm explain / lint (CI gate)

  ── runtime ───────────────────────────────────────────────────────────────
  call ─► shape mask (uint64) ─► shape cache ─► [hit] bind args ─► executor
                                     │                                 │
                                     └► [miss] splice fragments ───────┘
                                                                       ▼
                                                        pgxdrv ─► pgx ─► Postgres
                                                                       │
                                            generated scanner ◄────────┘
                                            (binary protocol → fields)
```

The **same builder API** is used at build time and at runtime. When the
generator can see the query statically it evaluates the builder at build time
and emits the result; when it cannot, the identical code runs at runtime against
a pooled buffer. This is partial evaluation / staged computation — one API, two
evaluation times, no duplicated semantics to keep in sync.

## Package map

Folder names state the layer. Core packages import **stdlib only**; the single
third-party dependency lives in exactly one adapter package.

```
storm/
  cmd/storm/          CLI: generate · explain · lint · verify
  schema/             schema IR: tables, columns, types, FKs, indexes   [stdlib]
  schema/pg/          Postgres introspection front end
  schema/sqlfile/     migration-file front end
  query/              typed query IR + builder (relational algebra)     [stdlib]
  model/              schema declaration DSL (s.Text, s.HasMany, …)     [stdlib]
  compile/            plan lowering, shape enumeration, capabilities    [stdlib]
  compile/sql/{pg,mysql,mssql,oracle}    SQL back ends
  compile/mongo/      aggregation-pipeline back end (v2.0, ADR-0004)
  migrate/            model ↔ schema diff → reviewable migration files  [stdlib]
  codegen/            Go emission, deterministic output                 [stdlib]
  runtime/            executor port, shape cache, buffer pool, binder   [stdlib]
  runtime/pgxdrv/     pgx/v5 adapter — the ONLY third-party import
  internal/cost/      relation-loading strategy cost model
  internal/scan/      call-site scanner (go/ast, no type-check needed)
  bench/              harness + benchstat baselines + rival suites
```

Conventions carried over from `anubis`: ≤ 10 Go files per folder, ≤ 15 methods
per interface, one interface per file, one struct per file, package clauses
prefixed and unique (`schema/pg` → `package schemapg`).

## The executor port

The entire driver seam. Five methods, so pgx API churn cannot reach the rest of
the tree, and so tests can count round trips.

```go
type Executor interface {
    Query(ctx context.Context, sql string, args []any) (Rows, error)
    Exec(ctx context.Context, sql string, args []any) (int64, error)
    Batch(ctx context.Context, b *Batch) error
    CopyFrom(ctx context.Context, table string, cols []string, src RowSource) (int64, error)
    Tx(ctx context.Context, opts TxOptions, fn func(Executor) error) error
}
```

`pgx` types never cross out of `runtime/pgxdrv`. A counting decorator over this
interface is how M3's "exactly 2 round trips" gate is proven.

## Design patterns, named

Chosen deliberately; each earns its place by removing a runtime cost or a class
of bug.

| Pattern | Where | What it buys |
|---|---|---|
| **Front end / IR / back end** | whole generator | dialect neutrality without a runtime dialect switch; the IR is a *logical plan*, so Mongo is a back end rather than a stretch |
| **Partial evaluation (staging)** | `query` + `compile` | one builder API evaluated at build time when static |
| **Flyweight** | SQL fragments, `unique.Handle[string]` | fragments interned once; comparison and cache keys are pointer-cheap |
| **Type state / phantom types** | query builder stages, fetch plans | illegal chains and unloaded-relation reads are compile errors |
| **Specification** | typed predicates | composable `And`/`Or`/`Not` without string concatenation |
| **Repository** | generated per aggregate | hand-written repos disappear; the boundary stays |
| **Strategy** | relation loading | two-query `= ANY` vs `LATERAL` vs `jsonb_agg`, picked by `internal/cost` and *measured* |
| **Unit of Work + Identity Map** | `storm.Unit` | batching and FK ordering, explicitly scoped — never ambient |
| **Object pool / arena** | buffers, arg arrays, row slabs | the difference between 3 allocs/op and 300 |
| **Ports & adapters** | `Executor` | pgx isolated; round-trip counting in tests |
| **Decorator** | executor middleware | tracing, query counting, slow-query capture, without touching the core |

Two anti-patterns are named and banned: **Active Record** (couples entities to
sessions) and **ambient Session/UoW** (makes I/O invisible at the call site).

## Shape cache, concretely

```
shape mask   uint64, one bit per optional predicate/order/limit slot
             computed by the generated call wrapper — no hashing, no strings

lookup       mask < 64 bits  →  [1<<n]atomic.Pointer[stmt] indexed array
             wider           →  sharded map, cache-line padded (see Gpool)

entry        stmt { sql string (interned) ; argPlan []binder ; scanPlan *scanner }
```

Cold path splices fragments once and CASes the entry in; a lost race just frees
the loser. Warm path is one atomic load, then argument binding straight into a
pooled `[]any` sized at build time. Nothing on the warm path can allocate.

## Row mapping

pgx binary result → generated decoder → struct fields, direct. No `any`, no
`driver.Value`, no `reflect`. Per column, the generator knows the OID, the Go
type, and nullability, so it emits a straight-line decode. Strings and `[]byte`
are the only necessary copies, and `RawValues` keeps those to one each.

## Where the layering rules bite

- `schema/`, `query/`, `compile/`, `codegen/`, `runtime/` — **stdlib only**.
- No package outside `compile/` may contain a dialect conditional.
- No package outside `runtime/pgxdrv` may name a pgx type.
- Generated code imports `runtime/` and the user's domain types. Nothing else.
- Generated output is **byte-deterministic** across runs and machines, so
  `storm verify` can fail CI on stale generated code with a plain diff.
