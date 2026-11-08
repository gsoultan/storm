---
tags: [storm, comparison, adoption]
updated: 2026-11-06
status: current
---

# Why storm, and when not to use it

This page is for a team deciding whether to try storm. It compares storm with
sqlc, GORM, Ent, Bun and a raw driver, and it is just as specific about where
storm is weaker. For the design research behind it, read
[COMPARISON.md](COMPARISON.md). For what is proven and what is not, read
[PRODUCTION-READINESS.md](PRODUCTION-READINESS.md).

Each claim below is backed by code, a test or a benchmark in this repository.
Every number names its source. Re-run `make bench` on your own hardware
before you quote any of them.

## The short version

storm is a Go ORM that works like a compiler. You write one plain Go struct per
table. `storm generate` turns it into typed query code. The SQL text for every
query shape is written at build time, dynamic filters included, and rows are
decoded straight from the binary wire protocol into your structs. Nothing in
the request path reflects, builds SQL strings or boxes values.

What you get from that:

- **Fewer allocations and less GC work than any other Go option measured.**
  This is the main runtime gain. A single query is not noticeably faster,
  because the database round trip dominates.
- **No string for a value to escape into.** Values only ever travel as bound
  parameters.
- **Mistakes caught by the compiler.** That covers a misspelt column, a
  predicate that does not fit the column's type, and a relation you read
  without loading it.
- **Relations without N+1.** A relation load takes a fixed number of
  queries however many parents match, and tests assert the count. That
  includes "the newest N
  children of each parent", which GORM, Ent and Bun cannot express
  correctly.

What it costs you:

- **A generate step.**
- **Query shapes declared on the model** instead of at the call site.
- **A large volume of generated code.**
- **A young project with one adopter.** Read [Where storm is
  weaker](#where-storm-is-weaker) before you decide.

## Side by side

How each tool works, not how fast it is. The tools are as of November 2026.

| | storm | sqlc | GORM | Ent | Bun | raw pgx |
|---|---|---|---|---|---|---|
| SQL text written | build time, dynamic queries too | build time, static queries only | at run time, every call | at run time, by a builder | at run time, by a builder | by you |
| Row mapping | generated, binary wire | generated, over your driver | reflection per row | generated, via `database/sql` | reflection, metadata cached | by you |
| Dynamic filters | yes, one compiled statement per shape, bounded cache | no | yes | yes | yes | by you |
| Relations | a fixed number of queries per load, asserted in tests | none | `Preload`: one extra query per association | extra queries; edge is silently empty if not loaded | extra queries | by you |
| Newest N children per parent | `Latest`, `LatestN` | no | limit applies to the whole set | same | same | by you |
| Where a value can reach SQL text | nowhere; the escape hatch only runs text it PREPAREd at generate time | nowhere | string conditions, `any` arguments | `.Modify()`, raw selectors | string identifiers in some clauses | your code |
| Schema changes | reviewable migration files; `migrate.Auto` is opt-in | your DDL | `AutoMigrate` at startup | Atlas | your DDL or Bun's migrator | your DDL |

## The numbers

These come from [`bench/RESULTS.md`](../bench/RESULTS.md), the v1.2.0
re-measurement of 2026-09-25: Go 1.27.1, PostgreSQL 17.11, pgx v5.11.0,
median of 10 runs. Every library ran on the same table and the same pool.

| Allocations per query | 1 row by primary key | 1,000 rows |
|---|---:|---:|
| **storm, generated** | **6** · 441 B | **6** · 41 KB |
| raw pgx | 18 · 926 B | 5,012 · 185 KB |
| sqlc | 13 · 1,006 B | 5,022 · 912 KB |
| Bun | 73 · 7.7 KB | 13,898 · 447 KB |
| Ent | 158 · 5.9 KB | 23,015 · 805 KB |
| GORM | 109 · 5.7 KB | 23,932 · 751 KB |

The same counts were measured again on 2026-11-06, on different hardware
(Linux, Xeon, PostgreSQL 16) against v1.12. storm matched them to within four
bytes, and each rival to within one or two allocations.

Under load (`TestGCPressure`: 4,000 queries of 500 rows across 16 workers),
storm made **249 times fewer allocations** than pgx's `Query` and `Scan`. It
allocated 74 MiB against 356 MiB, and ran 17 GC cycles against 81. The wall
clock was the same: 6.23 s for storm and 6.22 s for pgx.

**What these numbers do not show is a faster single query.** On a real
network every library here spends most of its time waiting for the database.
storm's wall clock sits within noise of raw pgx. Where storm helps is the
memory and GC cost of a busy service: steadier tail latency, smaller heaps,
and less CPU spent collecting garbage.

## Where storm is weaker

Read this section before the rest of the docs.

- **One adopter, on PostgreSQL.** One team runs storm, across nine bounded
  contexts and about 300 queries, and it replaced sqlc there. That migration found real bugs, all now fixed with tests (the
  [README](../README.md#status) lists them). A second adopter would exercise
  query shapes the first never writes.
- **MySQL and MariaDB run in nobody's production yet.** Both are tested on
  every commit against MySQL 8.4 and MariaDB 11.4. The driver was also
  stress-tested against real servers before v1.10. Still, every wrong-answer
  bug in this project so far was found by a path no test reached. See
  [MySQL in particular](#mysql-in-particular).
- **The API has been stable only since v1.0** (September 2026). It now
  follows semver ([STABILITY.md](STABILITY.md)), but the record is short.
- **Generated code is large.** Expect thousands of lines per table. An
  18-column table generates about 4,200. It is code you will see in review
  diffs.
- **No ad-hoc shapes at the call site.** There is no equivalent of GORM's
  `Select("a, b")` or `Joins(...)`. Projections, aggregations and joins are
  declared on the model, because the scanner is generated. The `storm.SQL`
  escape hatch covers the rest (it is refused on MySQL and MariaDB).
- **Hooks are narrower than GORM's.** Lifecycle hooks (`BeforeCreate`,
  `AfterUpdate`, `AfterFind`, ...) run around the single-row generated calls,
  not around batches, bulk loads or set-based writes; statement hooks see
  every statement. See [API.md § 8d](API.md).
- **Every query starts with `LIMIT 1000`.** This includes `Each` and `Rows`.
  A read that matches more rows returns the first thousand without an error,
  so set `Limit` yourself for a larger read ([API.md](API.md)).
- **A small community.** GORM has a decade of answers on Stack Overflow.
  storm has its docs and its tests.

## MySQL in particular

storm generates MySQL and MariaDB code with `storm generate -dialect mysql`
(or `mariadb`). It connects through `runtime/mydrv`, its own driver, which
uses only the standard library. The driver speaks the binary protocol
directly and supports TLS and `caching_sha2_password`. It has a bounded pool
and pins a connection for each transaction. Cancelling a statement sends
`KILL QUERY` from a second connection, capped by `Config.MaxKillConns`, so a
burst of timeouts cannot become a burst of new connections. Constraint
violations arrive as the same `runtime.ConstraintError` as on PostgreSQL.

What has been checked against real servers (v1.8.0 to v1.10.0, in the
[CHANGELOG](../CHANGELOG.md)):

- Values across every protocol size boundary, up to and past 16 MiB.
- Clients and servers in time zones other than UTC.
- Statements killed on the server.
- A server restart under 16 concurrent goroutines.

That testing found five bugs, and every one is fixed with a test that fails
on the previous release. One of them stored any value of 64 KiB or more cut
short, with no error. If you tried storm on MySQL before v1.10, upgrade. In
the driver, a 200-row query now costs one allocation
(`BenchmarkQuery200x8`, held as a CI budget).

What is **not** measured on MySQL is a comparison with GORM or
`go-sql-driver/mysql`. The allocation table above is PostgreSQL only.

What you give up on MySQL, by design ([DIALECTS.md](DIALECTS.md)):

| Missing on MySQL | Why | What to do instead |
|---|---|---|
| `storm import` | it reads PostgreSQL, SQL Server and Oracle catalogues only | write the model by hand. The structs are plain Go, but an existing schema is real work |
| `storm diff`, `verify`, `explain`, `watch` | the same | apply `storm ddl -dialect mysql` with your own migration tool |
| `migrate.Auto` | MySQL DDL is not transactional, so a plan could be half applied | the same |
| upsert | `ON DUPLICATE KEY UPDATE` fires on any unique key, so a typed `OnConflictEmail()` would lie | calling one is a compile error that names what is missing |
| partial unique indexes | MySQL has none. Widening one would change answers | soft-delete tables cannot keep uniqueness scoped to live rows |
| the `storm.SQL` escape hatch | its safety check PREPAREs against a real PostgreSQL | declare the shape on the model |
| `RETURNING` on insert (MySQL; MariaDB has it) | the engine lacks it | `Insert` leaves your row unchanged, and the doc comment says so |
| fast bulk load | `LOAD DATA LOCAL INFILE` lets a server read client files | `CopyFrom` is emulated, and `Batch` is one round trip per statement |

If you run MySQL in production and want to try storm, the best first step is
one read-heavy table that you model by hand. Then compare its allocation
profile against your current code. Please open an issue with what you find,
whether it works or not. Nobody has run storm on MySQL outside this
repository, so either result is useful.

## Who storm fits

- A Go service on PostgreSQL that wants sqlc's safety and speed but has
  outgrown static queries: optional filters, sorting and paging chosen at run
  time, or relations.
- A GORM codebase where reflection and GC show up in profiles, or where
  `Preload` and N+1 queries are the bottleneck. See
  [MIGRATING-FROM-GORM.md](MIGRATING-FROM-GORM.md). Its `storm import` step
  needs PostgreSQL.
- A team that wants migrations it can review, and wants the compiler to catch
  query mistakes.

## Who it does not fit

- A prototype where onboarding time matters more than anything. GORM gets you
  to a first query faster.
- A codebase that builds most of its query shapes at the call site, at run
  time.
- A team that needs a large ecosystem of plugins and answers today.
- A MySQL shop with a large existing schema and no time to model it by hand,
  at least until `storm import` has a MySQL form.

## Try it

[GETTING-STARTED.md](GETTING-STARTED.md) goes from an empty module to a
running query, with migrations and CI checks. storm needs Go 1.27.3 or later. On that toolchain,
`govulncheck ./...` reports no reachable vulnerabilities (checked
2026-11-06).
