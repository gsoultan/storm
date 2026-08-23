---
tags: [raorm, concept, thesis]
updated: 2026-08-23
status: proposed
---

# raorm — the thesis

> **Every other Go ORM builds SQL at runtime. raorm builds it at compile time —
> including the dynamic queries.**

An embeddable Postgres ORM for Go. Generated, not reflected. Compiled, not
interpreted. Module `github.com/gsoultan/raorm`, Go 1.26.

Read [[COMPARISON]] first — this document only makes sense as a response to it.

## The one-paragraph version

`sqlc` proved that build-time SQL and build-time row mapping give you raw-driver
performance with total type safety. It then stopped, because dynamic queries and
relation loading appear to require a runtime builder. They do not. A dynamic
query has a **bounded set of shapes**; each shape can be compiled once and cached
forever. A relation is a **fetch plan**; a fetch plan can be part of the type
rather than a runtime flag. Do both and you get Bun's expressiveness, Ent's type
safety, Hibernate's fetch-plan power, and sqlc's performance, in one tool.

## The eight concepts

### 1. Compile the query, do not interpret it

A dynamic filter set with *n* optional predicates has at most 2ⁿ **shapes**, and
in practice a handful are ever used. So:

- Every predicate is lowered **at build time** into an immutable SQL fragment
  with known placeholder arity.
- At runtime, "building" a query is appending precomputed fragments into a
  pooled `[]byte` and binding args into a fixed-size array. No `fmt`, no
  `reflect`, no per-node allocation.
- The assembled statement is cached under a **shape mask** (`uint64`). For masks
  under 64 bits the cache is an indexed array of `atomic.Pointer[stmt]` — not a
  map, so no hashing on the hot path.
- A warm shape does zero SQL construction, ever. It also maps 1:1 onto a
  server-side prepared statement in the pgx statement cache.

GORM, Ent, and Bun rebuild the same string on every call for the life of the
process. This is the largest single win available, and it is free after the
first call.

*This is gwaf's compiler thesis applied to SQL. It worked there.*

### 2. The fetch plan is part of the type

`With()` does not set a flag. It returns a **different generated type**.

```go
u, _ := user.ByID(ctx, db, id)              // type User        — u.Posts does not exist
p, _ := user.ByID(ctx, db, id).With(user.Posts)  // type UserWithPosts — p.Posts is []Post
```

Reading an unloaded relation is a **compile error**. Not a lazy load, not a nil
slice, not an empty `Edges` struct, not a `LazyInitializationException`.

**The N+1 problem becomes unrepresentable.** This is Hibernate's `@EntityGraph`
moved from runtime configuration into the type system, and it is the single
biggest combined DX-and-performance win in the design. It is also strictly
better than Ent, where forgetting `WithPosts()` yields a silently empty slice.

### 3. The escape hatch is first-class, not a defeat

Every ORM eventually meets a window function over a CTE with a lateral join.
Ent and Bun answer with untyped strings; Hibernate answers with a native query
that leaves entity management behind. raorm answers with sqlc, embedded:

```go
var TopEarners = raorm.SQL[EarnerRow](`
    WITH ranked AS (SELECT ..., row_number() OVER (PARTITION BY ...) rn FROM ...)
    SELECT ... FROM ranked JOIN LATERAL (...) l ON true WHERE rn <= $1`)
```

Validated at build time by `PREPARE`ing against a dev database — so the column
list, types, and placeholder arity are checked before the binary exists — with
a generated scanner for `EarnerRow`. Raw fragments compose *into* typed queries
as join sources. **You lose nothing by escaping.**

### 4. Zero reflection in the runtime path

Generated decoders read Postgres binary-protocol bytes directly into struct
fields. `rows.Scan(&a, &b, …)` is banned internally: it boxes every column into
`any`, which is an allocation per column per row in every ORM that uses it.

`reflect` may not be imported anywhere under `runtime/`. CI enforces it.

### 5. The dialect is a compile-time parameter, not a runtime branch

Multi-dialect support is the tax that forces every other ORM onto a
lowest-common-denominator **runtime** builder. A compiler does not pay it: raorm
knows the target at `raorm generate` time, so generated code carries lowered SQL
for exactly one dialect and the hot path contains no dialect decision at all.

Postgres ships first — the thesis must be proven once before it is generalised
five times — and it buys `LATERAL`, `= ANY($1)` array binds, `COPY`,
`RETURNING`, deferred constraints, the binary protocol, and a statement cache
that actually hits. MySQL/MariaDB, SQL Server, Oracle, then MongoDB follow, each
sequenced by how hard it stresses the seam.

Capabilities are negotiated at build time, so an unsupported construct is a
**generation error naming the target and the source line** — never a surprise on
a customer's install. See [[DIALECTS]] and ADR-0002.

### 6. Explicit unit of work, O(1) dirty tracking

No ambient persistence context. `raorm.Unit` is a scoped object you put things
into deliberately. Generated setters flip a bit in a fixed-width dirty mask, so
computing the changed-column set is a bitmask read — no allocation, no
comparison against a loaded snapshot. Flush emits ordered, batched statements
through `pgx.Batch`, with FK-correct ordering.

Hibernate's value (batching, insert ordering, identity map for graph writes)
without Hibernate's cost (dirty-checking every loaded entity at every flush).

### 7. The ORM ships the performance gate

`raorm explain` runs `EXPLAIN (ANALYZE, BUFFERS)` for every named query in CI.
`raorm lint` fails the build on a sequential scan over a table above a
configured row count, on a query shape count that has exploded, or on a relation
load that is not provably bounded in round trips.

`anubis` already has the rule *"no query on the hot path without EXPLAIN
(ANALYZE, BUFFERS)"* as a **perf veto** enforced by review. An ORM that enforces
it mechanically is worth more than one that is 5% faster.

### 8. The model is the source of truth; raorm never applies DDL

Schema is a plain Go struct (ADR-0001) — no DSL; `*T` means nullable,
`OrgID`+`Org *Org` means a foreign key, `[]Post` means has-many. From one model
raorm generates the query API
*and* a numbered, forward-only, **reviewable** migration file — which your
existing runner applies. There is no `AutoMigrate` and no library code path that
can alter a schema.

One canonical model is the only workable answer once five SQL dialects and a
document store are targets: five hand-maintained migration sets never stay in
sync, and MongoDB has no schema to introspect at all.

Drift is caught by verification rather than by making the database authoritative
— `raorm verify --stale | --drift | --pending`. The third turns *"you changed
the model and did not generate a migration"* into a CI failure.

Database-first survives as the **on-ramp**: `raorm import` writes the model from
an existing schema.

## Rejected — do not rebuild these

The reasoning matters more than the verdicts.

**Runtime DDL / AutoMigrate.** The failure mode is silent production schema
change. raorm *emits* migrations from the model but never applies one — the
distinction ADR-0001's first draft missed, and the whole of the danger.

**Ambient persistence context with lazy loading.** The root cause of nearly all
Hibernate pain. A field access that might issue I/O makes performance
unreviewable: you cannot tell by reading the code how many queries it runs.
Concept 2 replaces it with something a compiler can check.

**A runtime dialect branch.** Not multi-dialect itself — that was the first
draft's error. What forces the slow builder is *deciding the dialect at query
time*. Deciding it at generation time costs the hot path nothing (concept 5).

**`jsonb_agg` nested materialisation as the default.** Convenient, and it is
what Prisma and Drizzle reach for, but it moves cost onto the database and pays
a JSON parse on the client. Keep it as a measured *strategy option* under
concept 2, never the default.

**A bytecode VM for the query planner.** gwaf already measured this in Go:
closures beat a VM. Do not re-litigate it — read the gwaf negative result.

**A reflection fallback "just for convenience".** One reflection path becomes
the path everyone uses, and then the SLOs are fiction. There is no fallback.

**Active Record (`user.Save()`).** Couples the entity to a session, makes every
unit test need a database, and hides which statements run. Entities are data;
`Unit` and generated functions do the I/O.

**Soft delete by default.** A correctness landmine — every query that forgets
the predicate returns wrong rows, and unique indexes stop meaning what they say.
Available as an explicit, opt-in, per-table decision.

## Scope line

raorm is a **library**, imported, not deployed. No daemon, no UI, no applied
DDL, no lazy loading, no runtime dialect branch. Driver dependencies are
isolated behind a five-method port, one adapter package each (`pgx/v5` today).
Everything else is stdlib.

Each "no" above is a year of maintenance not spent.
