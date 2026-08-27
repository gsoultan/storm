---
tags: [storm, comparison, research]
updated: 2026-08-23
status: baseline
---

# The field: Ent, GORM, Bun, JPA/Hibernate

> Read this before CONCEPT.md. The design of storm is a direct response to
> a gap this table makes visible.

All architectural claims below are about *mechanism* and are stable across
versions. **No performance number is quoted here.** Per project convention,
numbers come from `bench/` and are re-measured, never remembered.

## 1. The question that separates them

Every ORM answers two questions. *When is the SQL text produced?* and *when is
the row-to-struct mapping decided?* Everything else — ergonomics, speed,
expressiveness — follows from those two answers.

| Tool | SQL text produced | Row mapping decided | Consequence |
|---|---|---|---|
| **GORM** | runtime, per call | runtime reflection, per row | maximum flexibility, maximum cost |
| **Ent** | runtime, per call (`dialect/sql`) | generated assigners over `database/sql` | typed at the edge, interpreted in the middle |
| **Bun** | runtime, per call | runtime reflection w/ cached struct meta | SQL-shaped and quick, still interpreted |
| **Hibernate** | runtime, cached query plan | runtime reflection / bytecode enhancement | strongest semantics, heaviest per-entity cost |
| **sqlc** | **build time** | **build time** | fastest and safest — but static only |
| **go-jet** | runtime builder, types from DB | runtime reflection scan | good types, interpreted execution |
| **sqlboiler** | build time (+ runtime qm) | build time | fast; API ages poorly; relations are extra queries |
| **raw pgx** | you write it | you write it | the floor every benchmark is measured against |

The whole industry sits on the "runtime" rows. **sqlc proves the "build time"
row is achievable in Go — and then refuses to do dynamic queries or relations.**
That refusal is the opening.

## 2. GORM — the reflection interpreter

**Model.** Struct tags, a chainable `*gorm.DB` whose `Statement` is cloned per
call, and a registered callback pipeline that walks processors to assemble
clauses.

**Developer friendliness — best in class, and it is not close.** Ten minutes to
first query. `AutoMigrate`, hooks, associations, soft delete, a plugin ecosystem,
and answers to every question already on Stack Overflow. This is the bar for
onboarding and storm should be measured against it.

**Performance — the weakest architecture of the four.** Cost is structural, not
incidental: a statement clone per chained call, a clause map assembled and
key-sorted per query, SQL rebuilt from scratch every single execution, and
`reflect`-driven field assignment per column per row through `database/sql`'s
`driver.Value` boxing. Nothing is memoised across calls because nothing is
identified as repeatable.

**Query complexity — the ceiling arrives early.** `Preload` is N+1 by
construction (an extra query per association, multiplying for nested preloads).
`Joins` into non-entity shapes gets awkward fast. Window functions, CTEs,
`LATERAL`, grouping sets, and recursive queries all mean dropping to raw SQL and
losing typed scanning. Column references are strings, so a rename is a runtime
error found in production.

**Steal:** the onboarding curve, `WithContext`/`Session` ergonomics.
**Reject:** `AutoMigrate` in production, soft-delete-by-default, `any`-typed
conditions, string column names.

## 3. Ent — codegen at the edges, interpretation in the middle

**Model.** Schema declared in Go (`ent/schema/*.go`), `ent generate` emits typed
builders, predicates, and edge traversals.

**Developer friendliness — genuinely good types, genuinely painful volume.**
Field and predicate typos are compile errors, which is a real step up. Edge
traversal reads well. Atlas-backed migrations are the best DDL story in Go.
The interceptor / hook / privacy layer is the strongest extensibility model in
any Go ORM — worth studying on its own.

The costs are equally real: thousands of generated lines per entity set,
regeneration churn dominating review diffs, a steep first week, and one specific
footgun — `u.Edges.Posts` is simply empty if you forgot `WithPosts()`. **The
N+1 mistake and the forgot-to-load mistake are both silent.**

**Performance — better than GORM, structurally short of the ceiling.** The
builders are typed but still *assembled per call* into `dialect/sql`, then
executed through `database/sql`. Eager loading issues additional queries rather
than one shaped query. Type safety was moved to build time; execution was not.

**Query complexity — good for graphs, weak for analytics.** Predicate composition
and traversal are pleasant. Aggregation and `GroupBy` are clunky. Anything
genuinely analytical needs `.Modify()` or a hand-built `sql.Selector`, and at
that moment you are back to strings and untyped scanning.

**Steal:** schema-as-code → codegen; typed predicates; the interceptor/privacy
layer; Atlas for DDL.
**Reject:** runtime builder assembly, `database/sql` in the path, the silently
empty `Edges` field, unbounded generated volume.

## 4. Bun — the SQL-shaped builder

**Model.** Struct tags plus a builder that deliberately mirrors SQL:
`db.NewSelect().Model(&u).Join(...).Where(...)`.

**Developer friendliness — the best *API shape* of the four.** If you know SQL,
you are productive in an hour, because the API is SQL with parentheses moved.
Minimal magic, so minimal surprise. Weaker documentation and a smaller community
than GORM; that is a resourcing problem, not a design one.

**Performance — the best of the three Go runtime ORMs.** Struct metadata is
reflected once and cached, which removes GORM's worst per-row cost. What remains
is still structural: builder allocation per query, reflection-driven assignment
per row, and `database/sql`'s interface hops and `driver.Value` boxing between
the wire and your fields.

**Query complexity — the strongest in Go.** `With()` CTEs, `ColumnExpr` for
window functions, subqueries as models, `UNION`, `ON CONFLICT` upserts, bulk
operations, and real relation support. You are rarely *blocked*; you are only
ever unsafe, because the exotic parts are strings.

**Steal:** the SQL-shaped API surface — this is the DX model storm should copy
outright; CTE and window as first-class nodes; bulk operations.
**Reject:** strings for identifiers, reflection in the scan path.

## 5. JPA / Hibernate — the persistence context

**Model.** Entities live inside a `Session`/`EntityManager` that maintains an
identity map, dirty-checks at flush, and materialises lazy associations through
proxies. Queries via JPQL, the Criteria API, or native SQL.

**Developer friendliness — spectacular at the top, brutal underneath.** Spring
Data JPA's derived repository methods are still the fastest CRUD authoring
experience in any ecosystem. Then the abstraction leaks:
`LazyInitializationException`, `MultipleBagFetchException`, flush-ordering
surprises, cascade semantics, detached-entity merges, and `@Transactional`
proxy subtleties. **Hibernate is easy for a week and requires an expert
thereafter.** That failure curve is the single most important lesson in this
document.

**Performance — highest per-entity overhead of the four, with the best recovery
tools.** N+1 is the *default* behaviour, not an edge case. The mitigations —
`@BatchSize`, `@EntityGraph`, `join fetch`, `StatelessSession`, second-level
cache, statement batching — are excellent, and every one of them must be
consciously applied by someone who knows they exist.

**Query complexity — the strongest of the four, by a distance.** JPQL, a Criteria
API made type-safe by a *generated metamodel* (the idea Ent later borrowed),
`@EntityGraph` fetch plans, native queries with result-set mapping, and DTO
projections. No Go ORM is close.

**Steal:** `@EntityGraph` — declarative fetch plans are the right answer to N+1;
metamodel codegen; `@Version` optimistic locking; statement batching with
insert ordering; the DTO-projection discipline.
**Reject, emphatically:** the ambient persistence context and lazy loading.
Nearly every Hibernate pathology — the exceptions, the surprise queries, the
memory profile, the "why did this flush" debugging session — traces back to
*implicit* I/O triggered by a field access. **A field access must never be a
query.**

## 6. The three worth knowing that are not ORMs

- **sqlc** — SQL in, typed Go out, at build time. Zero reflection, effectively
  raw-driver speed, and impossible to typo. Two hard walls: **no dynamic
  queries** (the `WHERE (@f::text IS NULL OR col = @f)` workaround wrecks index
  selectivity and plan quality) and **no relation loading**. This is the current
  tool in `anubis`, and it is the right baseline to beat.
- **go-jet** — types generated from the live DB, composable typed builder. The
  closest existing thing to storm's front half; execution is still a runtime
  builder with reflection scanning.
- **sqlboiler** — DB-first codegen, genuinely fast. The generated API has aged
  poorly and eager loading is separate queries.

## 6b. The query they all get wrong

Worth naming on its own, because it is the same bug in four codebases:
**a `Limit` inside an eager load applies to the whole loaded set, not per
parent.** `Preload("Posts")` with a limit of 1 across 50 users returns one post
in total, not one each. GORM, Ent, and Bun all behave this way; Hibernate needs
a windowed subquery or `@Subselect` to avoid it.

So "the newest post for each user" — greatest-n-per-group, an ordinary
requirement — has no correct expression in any of them. The workarounds are
loading every child row and slicing in Go, or raw SQL with untyped scanning.

storm's answer is `.Latest(col)` / `.Earliest(col)` / `.LatestN(n, col)`, per
parent, lowered to
`DISTINCT ON`, `LATERAL`, or `row_number()` by a cost model, still in one round
trip — and `.Limit()` inside a relation is a **generation error** that asks
which of the two you meant. [[REFERENCE]] §4.9.

## 7. The gap, stated precisely

Five properties. Nothing has all five.

| | Build-time SQL | Build-time scan | Dynamic composition | Relations w/o N+1 | Full analytical SQL |
|---|---|---|---|---|---|
| GORM | ✗ | ✗ | ✓ | ✗ | ✗ (raw) |
| Ent | ✗ | ~ | ✓ | ~ (extra queries) | ✗ (escape hatch) |
| Bun | ✗ | ✗ | ✓ | ~ (extra queries) | ~ (strings) |
| Hibernate | ~ (plan cache) | ✗ | ✓ | ~ (must opt in) | ✓ |
| sqlc | ✓ | ✓ | **✗** | **✗** | ✓ |
| **storm** | **✓** | **✓** | **✓** | **✓** | **✓** |

The bet is that the last row is reachable, and that the reason nobody has built
it is that each pair of properties is individually easy and the combination
requires treating the ORM as a **compiler** rather than a library.

## 8. What the field teaches

1. **GORM**: onboarding is a feature. Budget for it explicitly.
2. **Ent**: type safety at the edge is wasted if execution stays interpreted —
   and generated volume is a DX tax that compounds every review.
3. **Bun**: the closer the API is to SQL, the less there is to learn. Copy this.
4. **Hibernate**: implicit I/O is the original sin. Every one of its famous
   failure modes is a consequence of a field access that might be a query.
5. **sqlc**: build-time is not a compromise on power — it is a compromise on
   *dynamism*. Remove that one limitation and the tradeoff disappears.
