# raorm

> **Every other Go ORM builds SQL at runtime. raorm builds it at compile time —
> including the dynamic queries.**

An embeddable ORM for Go. Generated, not reflected. Imported, not deployed.
Zero CGO; driver dependencies isolated behind a five-method port.

**Postgres first**, then MySQL/MariaDB, SQL Server, Oracle, and MongoDB — the
dialect is a *compile-time parameter*, so multi-target costs the hot path
nothing. Model-first: one Go schema, generated query API, generated migrations
raorm never applies.

**Status: design. No code yet.** Milestone M0 is a two-week spike that will
either prove the thesis or kill the project — see [PLAN.md](docs/PLAN.md).

## Why

`sqlc` proved Go can have build-time SQL and build-time row mapping — raw-driver
speed, total type safety. It then stopped, because dynamic queries and relation
loading look like they need a runtime builder. They do not:

- a dynamic query has a **bounded set of shapes**; compile each shape once,
  cache it under a `uint64` mask, and a warm call allocates **nothing** to build
  its SQL;
- a relation is a **fetch plan**; put the plan in the *type* and reading an
  unloaded relation becomes a **compile error** — N+1 stops being a mistake you
  can make.

## The five properties, and who has them

| | Build-time SQL | Build-time scan | Dynamic composition | Relations w/o N+1 | Full analytical SQL |
|---|---|---|---|---|---|
| GORM | ✗ | ✗ | ✓ | ✗ | ✗ |
| Ent | ✗ | ~ | ✓ | ~ | ✗ |
| Bun | ✗ | ✗ | ✓ | ~ | ~ |
| Hibernate | ~ | ✗ | ✓ | ~ | ✓ |
| sqlc | ✓ | ✓ | ✗ | ✗ | ✓ |
| **raorm** | **✓** | **✓** | **✓** | **✓** | **✓** |

Full reasoning in [COMPARISON.md](docs/COMPARISON.md).

## Status — what is built, and what is not

**This is a working prototype with measured results, not a usable ORM yet.**
The docs below describe the intended design in full; the table says how much of
it exists. Every performance claim is measured and reproducible
(`make db && make bench`), and nothing in `bench/RESULTS.md` is quoted from
memory.

| | state |
|---|---|
| **Thesis** — compile-time SQL, zero-alloc warm path | ✅ proven; 0.95× raw pgx, 0 allocs building SQL |
| **Model → schema IR → DDL → introspection → migration diff** | ✅ round-trip is a fixpoint; migrations converge against a live database |
| **Read codegen** — Row, typed predicates, scanner, `Count`/`Exists` | ✅ matches a hand-written runtime; 6 allocs per 1,000 rows against GORM's 23,937 |
| **`= ANY($1)` + two-round-trip relation loading** | ✅ 50 parents + 25,000 children in exactly 2 round trips, asserted |
| **`Or` / `Not` / `NotAny`** | ✅ expression-tree IR; precedence cross-checked against Postgres |
| **joins, CTEs, windows, `GROUPING SETS`, `UNION`** | ❌ the tree now carries them structurally; the emitters are unwritten |
| **Fetch-plan types** (`user.Plan("Feed")`, compile error on unloaded relation) | ❌ mechanism proven, type generation unwritten |
| **Writes, unit of work, escape hatch, `explain`/`lint`** | ❌ not started |
| **MySQL, MariaDB, SQL Server, Oracle, MongoDB** | ❌ not started; the IR is dialect-neutral by construction |

Read [PLAN.md](docs/PLAN.md) for milestone state and
[bench/RESULTS.md](bench/RESULTS.md) for every measurement, including the ones
that falsified an assumption.

## Read in this order

| Doc | What |
|---|---|
| [COMPARISON.md](docs/COMPARISON.md) | Ent, GORM, Bun, JPA/Hibernate — mechanism by mechanism, and the gap |
| [API.md](docs/API.md) | The code design in brief — model, queries, fetch plans, writes, errors |
| [EXAMPLE.md](docs/EXAMPLE.md) | **Start here for code.** The whole surface in one page: struct → generate → query |
| [REFERENCE.md](docs/REFERENCE.md) | Lookup: full type table, cascade rules, M:N with payload, polymorphic, unit of work |
| [COMPLEX-QUERIES.md](docs/COMPLEX-QUERIES.md) | **Eight real queries as objects** — MRR dashboards, churn cohorts, anti-joins, exclusion constraints, facets, recursive trees |
| [DIALECTS.md](docs/DIALECTS.md) | Capability matrix, lowering passes, why Mongo is a back end |
| [CONCEPT.md](docs/CONCEPT.md) | The thesis. Eight concepts, **including the rejected ones** |
| [ARCHITECTURE.md](docs/ARCHITECTURE.md) | Package map, IR, design patterns, the shape cache |
| [PERFORMANCE.md](docs/PERFORMANCE.md) | Budgets, forbidden things, the harness |
| [PLAN.md](docs/PLAN.md) | Milestones, exit gates, **kill criteria**, risk register |
| [adr/](docs/adr/) | The four load-bearing decisions |
| [AGENTS.md](AGENTS.md) | Conventions and the profile roster |

## Scope line

No applied DDL. No lazy loading. No runtime dialect branch. No daemon, no UI,
no Active Record, no soft-delete-by-default, no reflection fallback.

Each "no" is a year of maintenance not spent.
