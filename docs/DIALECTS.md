---
tags: [storm, dialects, portability]
updated: 2026-08-23
status: proposed
---

# Targets: SQL dialects and document stores

> **For an interpreter, multi-dialect is a runtime tax. For a compiler, it is a
> build-time cost.**

## Why this strengthens the thesis rather than diluting it

GORM, Ent, and Bun branch on dialect **per query, at runtime**, because they
build SQL per query at runtime. That branch sits on the hot path forever, and it
is a large part of why their builders allocate.

storm knows the target at `storm generate` time. Generated code contains SQL
text for exactly one dialect, already lowered, already interned. **There is no
dialect branch at runtime, because there is no dialect decision at runtime.**

Adding MySQL, SQL Server, and Oracle therefore costs a generator back end and a
test matrix — and costs the hot path nothing at all. Multi-dialect makes storm's
advantage over the interpreters *larger*, not smaller.

**One binary, several engines** — the on-prem case where a customer brings their
own database — is handled by generating for N targets and selecting the compiled
statement table once at init. That is one pointer indirection at startup, not a
branch per query.

## The capability model

Capabilities are negotiated at **build time**, never sniffed at runtime.

```go
type Capabilities struct {
    Placeholder   PlaceholderStyle // $1 | ? | @p1 | :1
    IdentQuote    QuoteStyle       // " | ` | [ ]
    MaxIdentLen   int
    Returning     ReturningStyle   // RETURNING | OUTPUT | RETURNING INTO | none
    ArrayBind     bool             // one placeholder for a list?
    Lateral       LateralStyle     // LATERAL | APPLY | none
    CTE, Recursive, Window bool
    Upsert        UpsertStyle      // ON CONFLICT | ON DUPLICATE KEY | MERGE | none
    BulkLoad      BulkStyle        // COPY | LOAD DATA | TVP | ARRAY DML | bulkWrite
    EmptyIsNull   bool             // Oracle
    NativeBool    bool             // Oracle < 23c: no
    ForeignKeys   bool             // Mongo: no
    MultiDocTx    bool             // Mongo: replica set only
}
```

A query using a capability the target lacks **fails generation**, naming the
feature, the target, and the source line. It never fails on a customer's install.

## Capability matrix

`✓` native · `~` emulated by a lowering pass · `✗` generation error

| | Postgres | MySQL 8 | MariaDB 10.6 | SQL Server | Oracle 19 | MongoDB 7 |
|---|---|---|---|---|---|---|
| Placeholder | `$1` | `?` | `?` | `@p1` | `:1` | BSON |
| Ident quote | `"` | `` ` `` | `` ` `` | `[ ]` | `"` | — |
| Max ident | 63 | 64 | 64 | 128 | 128 (30 pre-12.2) | — |
| `RETURNING` | ✓ | ✗ → `~` | ✓ insert/delete | ✓ `OUTPUT` | ✓ `RETURNING INTO` | `~` findAndModify |
| CTE | ✓ | ✓ | ✓ | ✓ | ✓ | ✗ |
| Recursive CTE | ✓ | ✓ | ✓ | ✓ | ✓ (+`CONNECT BY`) | `~` `$graphLookup` |
| Window fns | ✓ | ✓ | ✓ | ✓ | ✓ | `~` `$setWindowFields` |
| `LATERAL` | ✓ | ✓ 8.0.14+ | ✗ | ✓ `APPLY` | ✓ 12c+ | `~` `$lookup` pipeline |
| Array bind | ✓ `= ANY($1)` | `~` expand | `~` expand | `~` TVP/expand | `~` expand | ✓ `$in` |
| Upsert | ✓ `ON CONFLICT` | ✓ `ON DUP KEY` | ✓ | ✓ `MERGE` | ✓ `MERGE` | ✓ `upsert:true` |
| Bulk load | ✓ `COPY` | ✓ `LOAD DATA` | ✓ | ✓ TVP/bcp | ✓ array DML | ✓ `bulkWrite` |
| Non-equi join | ✓ | ✓ | ✓ | ✓ | ✓ | ✗ |
| Foreign keys | ✓ | ✓ | ✓ | ✓ | ✓ | ✗ |
| Multi-stmt tx | ✓ | ✓ | ✓ | ✓ | ✓ | `~` replica set |

## Lowering passes worth naming

**Array bind → arity bucketing.** Postgres binds a whole list to one placeholder
(`= ANY($1)`), so list length never changes the statement. Everywhere else the
list expands to `IN (?,?,?)` and **arity becomes part of the shape**. Left alone,
a 500-element list would mint 500 statements. So arity is bucketed to powers of
two (1, 2, 4, 8, 16, 32, …) and padded with repeats of the last value. Bounded
shape count, still one prepared statement per bucket. `storm lint` reports
bucket spread.

**No `RETURNING` → batched round trip.** On MySQL, `INSERT` plus
`LAST_INSERT_ID()` in one batch. This is exactly what the Unit of Work's deferred
value handles already exist for — the API in [[API]] §9 does not change shape,
only the number of statements underneath.

**Oracle's empty string is NULL.** A *semantic* difference, not a syntactic one:
`WHERE name = ''` can return different rows than on Postgres. Not emulatable.
Declaring `oracle` in `portability.assert` makes any `Eq("")` or non-null-
constrained text column a **declare-time error** with a pointer to the model line.

**Oracle has no native `BOOLEAN`** before 23c → `NUMBER(1)` plus a `CHECK`
constraint, generated from one `s.Bool(...)` declaration.

**SQL Server `OFFSET/FETCH` requires `ORDER BY`** → a `.Limit()` without
`.OrderBy()` is a generation error on that target, not a silently different
result set.

## MongoDB is a back end, not a dialect

This is the honest part.

storm's IR is a **logical query plan** (relational algebra), not a SQL AST. SQL
dialects lower the plan to SQL text; Mongo lowers it to an aggregation pipeline.
The runtime executes an opaque compiled artifact and does not care which.

What maps cleanly: filters → `$match`; projections → `$project`; sort/limit/skip;
aggregates → `$group`; equi-joins → `$lookup`; `IN` → `$in`; upsert; bulk write.

What does **not** map, and will be a generation error rather than a surprise:
non-equality joins, foreign keys and FK-ordered flush, cross-database joins,
arbitrary CTEs, and full transactional semantics outside a replica set.

**The modelling difference is the real one.** A document schema embeds where a
relational schema joins, and that is a design decision no translator can make
for you. Hence `OnDocument(storm.Embed(...))` in [[API]] §1 — required, not
defaulted. A relation with no document directive fails generation for a Mongo
target.

Consequence worth stating plainly: **do not expect one model to run unchanged on
Postgres and Mongo and be well-designed on both.** storm's contribution is
making the difference *visible at build time* and letting one model serve both
where that genuinely makes sense. `storm lint --portable` prints the intersection
of your configured targets, so "what will not port" is a command, not a wiki page.

## Sequencing

Ordered by **distance from Postgres**, because each step stresses a different
part of the seam. Nothing here starts before v1 ships on Postgres.

| Ver | Target | What it proves |
|---|---|---|
| v1.0 | Postgres | the thesis |
| v1.1 | MySQL 8 + MariaDB | placeholder style, no `RETURNING`, arity bucketing, `ON DUPLICATE KEY`. Close enough to be tractable, different enough to prove the seam is real |
| v1.2 | SQL Server | `OUTPUT`, `MERGE`, `[ ]` quoting, `@p1`, `ORDER BY`-required paging, TVP bulk |
| v1.3 | Oracle | the hardest SQL target — empty-string-is-NULL is *semantic*, plus `NUMBER` mapping, identifier limits, upper-case folding |
| v2.0 | MongoDB | the back-end seam, not the dialect seam |

**Gate before v1.1:** the Postgres back end must have zero dialect conditionals
outside `compile/`, proven by `scripts/check/import-boundary.sh`. If the seam
leaked while nobody was testing it, fix that before adding a second target.

**Gate before v2.0:** Oracle must ship first. Oracle already stresses semantic
divergence and capability gating; if the capability model cannot carry Oracle,
it certainly cannot carry a document store.
