---
tags: [storm, dialects, portability]
updated: 2026-09-12
status: implemented for PostgreSQL, MySQL 8 and MariaDB
---

# Targets: SQL dialects and document stores

> **For an interpreter, multi-dialect is a runtime tax. For a compiler, it is a
> build-time cost.**

> **Built, as of v0.3.0.** `compile/myddl` renders MySQL 8 DDL, and
> `storm portable mysql` reports every construct that does not cross — by
> column, with what to do instead. Verified by applying the emitted DDL to a
> real MySQL 8 (`scripts/check/mysql.sh`).
>
> **And storm now GENERATES for MySQL and MariaDB.** What
> [ADR-0007](adr/0007-mysql-runtime-needs-a-second-decoder-family.md) said was
> the real cost — a second decoder family, because the `Executor` port hands
> back raw wire bytes and every scanner decoded PostgreSQL big-endian — is
> built: `runtime/mydec` decodes the little-endian binary protocol,
> `codegen` takes the dialect as a build-time parameter, `compile/mysql` and
> `compile/mariadb` hold the SQL, and `runtime/mydrv` is a stdlib-only adapter
> that speaks the wire directly rather than wrapping a `database/sql` driver
> (which would hand back values already decoded, and force re-encoding).
> A generated package does real CRUD against both engines, through a pool,
> over TLS. SQL Server, Oracle and Mongo remain design.

## What differs on MySQL and MariaDB, and why

Every item is a decision, not an omission. A construct storm cannot express on a
target is REFUSED at generate time; a construct whose cost differs but whose
answers do not is widened, and named here.

| Construct | PostgreSQL | MySQL / MariaDB | Why |
|---|---|---|---|
| uuid primary key | `DEFAULT gen_random_uuid()` | generated **client-side**, v7 | `UUID()` is version 1: it embeds the SERVER'S MAC ADDRESS in a value that ends up in URLs, and it is a different version from the one the model asked for. v7 keeps the key time-ordered, which matters more here because the primary key IS the clustered index |
| partial UNIQUE index | native | **refused** | Widening it changes ANSWERS: rows the predicate excluded could coexist and now conflict. This is soft delete's live-scoped uniqueness |
| partial non-unique index | native | widened to a plain index | Costs storage and a little scan time; changes no answer. Refusing it would refuse `storm.OneOfN` entirely, whose per-variant lookup indexes are partial |
| `INSERT … RETURNING` | yes | MySQL no, MariaDB yes | On MySQL `Insert` leaves the caller's row unchanged and the doc comment says so at the call site |
| upsert | `ON CONFLICT <target>` | **not generated** | `ON DUPLICATE KEY UPDATE` names no target — it fires on ANY unique key — so `OnConflictEmail()` would be a lie about which index it watched. Calling it is a compile error naming what is missing |
| recursive cycle guard | an array of visited keys | a HEX string with `FIND_IN_SET`, bounded | No array type. The path column has a fixed width, so a traversal deeper than it can hold is refused rather than left to the server's `sql_mode` to turn an overflow into an error rather than a truncation |
| bound key list | one array parameter, unnested | one JSON document, `JSON_TABLE` | No array parameter. A BINARY key travels as hex and returns through `UNHEX`, which keeps the comparison on the key's index |
| arc exactly-one CHECK | `(…)::int + (…)::int = 1` | `(…) + (…) = 1` | A boolean is already 1 or 0 in arithmetic here. storm wrote this expression, so storm respells it; a check the MODEL declared is passed through untouched |
| `migrate.Auto` | yes | **PostgreSQL-only** | MySQL DDL is not transactional, so the one-transaction guarantee automigrate is built on does not exist. Use `storm ddl -dialect …` with your own tool |
| `storm.AnyRef` discriminator | `varchar(64)` | `varchar(64)` | It holds a table name, which is bounded — and an unbounded one cannot be indexed here without a key length, while PostgreSQL refuses a prefix index |
| `HasAnyKey` / `HasAllKeys` | `?\|` and `?&` | `JSON_OVERLAPS` / `JSON_CONTAINS` over `JSON_KEYS` | No such operators. One bound value either way, so the statement's shape does not depend on how many keys were asked for. The value is not cast: MariaDB has no `CAST(… AS JSON)` |
| `storm.SQL` escape hatch | validated by PREPARE | **refused** | The allow-list is built by PREPAREing against a real PostgreSQL. Generating for MySQL with raw queries registered would ship them unchecked |

## What differs on SQL Server, and why

Measured 2026-09-20 against Azure SQL Edge 15.0 (arm64) and gated in CI against
`mcr.microsoft.com/mssql/server`. The lowering is `compile/mssql` and
`compile/msddl`; `scripts/check/mssql.sh` runs every statement of it.

The shape of the list is different from MySQL's. MySQL's differences are mostly
things it LACKS; SQL Server's are mostly things it POSITIONS differently — and
three of the entries below are capabilities MySQL had to refuse.

| Construct | PostgreSQL | SQL Server | Why |
|---|---|---|---|
| placeholder | `$1` | `@p1` | Parameters are NAMED, and a name is an identifier, so `@1` is a syntax error rather than a terse `@p1`. Because they are names, a reused ordinal binds ONCE — which MySQL's positional `?` cannot do, and which is what makes the two rows below possible |
| keyset row comparison | `(a, b) > ($1, $2)` | expanded to an OR-chain | There is no row constructor. The expansion mentions each name twice and binds each value once |
| declared parameter in two union branches | one value | one value | Named, so this crosses unchanged. `compile/mysql` refuses it outright |
| row cap | `LIMIT n OFFSET m` | `OFFSET m ROWS FETCH NEXT n ROWS ONLY` | A clause OF `ORDER BY`, so a capped read with no ordering is a syntax error — `ORDER BY (SELECT NULL)` is the fallback. The operands are also REVERSED, so the paging arguments bind offset first |
| existence probe cap | `LIMIT 1` | `SELECT TOP 1` | `TOP` needs no ordering, which is exactly what a probe has none of |
| `RETURNING` | a trailing clause | `OUTPUT`, **positional** | It sits between the assignments and the predicate. At the end it is a syntax error, so the insert carries it in its punctuation and the update hands it to a splicer that knows where it goes |
| row lock | `FOR UPDATE` suffix | `WITH (UPDLOCK, ROWLOCK)` **table hint** | Attached to the table reference in `FROM`, not the end of the statement. `SKIP LOCKED` is `READPAST`; `FOR SHARE` is `REPEATABLEREAD`, not `HOLDLOCK`, which would be SERIALIZABLE and take range locks nobody asked for |
| uuid primary key | `DEFAULT gen_random_uuid()` | `DEFAULT NEWID()` | Server-side, and a real v4. **Not** client-side, unlike MySQL — with a default and an `OUTPUT` clause, an insert that names no key comes back carrying one. `uuidv7()` is refused: `NEWID()` is v4 and `NEWSEQUENTIALID()` derives from the server's MAC |
| partial UNIQUE index | native | **native** | Filtered indexes exist, so soft delete's live-scoped uniqueness ports. This is the refusal MySQL cannot avoid |
| covering index | `INCLUDE` | `INCLUDE` | Also native. MySQL has no covering clause and rewrites them as trailing keys |
| nullable UNIQUE | many NULL rows | **translated** to a filtered index | `UNIQUE` treats NULLs as EQUAL here and accepts exactly one. That changes ANSWERS, so `WHERE col IS NOT NULL` is added — which indexes exactly the rows PostgreSQL's index constrained. `NULLS NOT DISTINCT` is therefore the plain form here, and the one MySQL refuses |
| `GROUPING SETS` / `CUBE` / `GROUPING()` | native | **native** | All three, in the function spelling. MySQL has only `WITH ROLLUP` and refuses the rest |
| `LATERAL` | `CROSS JOIN LATERAL` | `CROSS APPLY` | Same construct, different keyword. MariaDB has neither |
| bound key list | one array parameter, unnested | one JSON document, `OPENJSON … WITH` | No array parameter. Unlike MySQL there is no hex round trip: a uuid is `UNIQUEIDENTIFIER`, so it travels as its own text and returns as itself, leaving no expression wrapped around the indexed column |
| recursive cycle guard | an array of visited keys | an `NVARCHAR(MAX)` path with `CHARINDEX` | No array type — but no declared width either, so unlike MySQL there is no depth at which the guard silently stops guarding. The server's own 100-level ceiling is lifted with `OPTION (MAXRECURSION 0)`, because the real bound is the caller's depth parameter |
| arc exactly-one CHECK | `(…)::int + (…)::int = 1` | `CASE WHEN … THEN 1 ELSE 0 END + … = 1` | A predicate is not a value here, so there is no arithmetic coercion to lean on |
| `FILTER (WHERE …)` | native | **refused** | No such clause. The rewrite changes what the aggregate counts rather than how it is spelled |
| JSON containment | `@>`, `<@` | **refused** | Through the 2019 level the JSON support is `JSON_VALUE`, `JSON_QUERY`, `ISJSON` and `OPENJSON` — there is no containment predicate at all. `HasAnyKey` IS expressible, through `OPENJSON` over both sides |
| numeric `RANGE` frame | `RANGE BETWEEN 3 PRECEDING` | **refused** | `RANGE` takes only `UNBOUNDED` and `CURRENT ROW`. `ROWS` accepts the offset, but the two differ over ties |
| upsert | `ON CONFLICT <target>` | `MERGE` | A different STATEMENT, not a clause, so the whole shape comes from the back end. `WITH (HOLDLOCK)` is not optional: without it two concurrent merges of one key both insert and one fails, which works in every test and breaks under load. The UNTARGETED `DoNothing()` is refused by name — a bare `DO NOTHING` fires on any unique index, and a match condition names columns |
| `ON DELETE RESTRICT` | `RESTRICT` | `NO ACTION` | No such keyword; `NO ACTION` is what it means, and the difference PostgreSQL draws is not observable through a constraint storm generates, which is never `DEFERRABLE` |

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
| Bulk load | ✓ `COPY` | `~` emulated | `~` emulated | ✓ **bcp**, implemented | ✓ array DML | ✓ `bulkWrite` |
| Non-equi join | ✓ | ✓ | ✓ | ✓ | ✓ | ✗ |
| Foreign keys | ✓ | ✓ | ✓ | ✓ | ✓ | ✗ |
| Multi-stmt tx | ✓ | ✓ | ✓ | ✓ | ✓ | `~` replica set |
| Index methods | btree, hash, gin, gist, spgist, brin | btree, hash, fulltext | btree, hash, fulltext | clustered/nonclustered, columnstore | btree, bitmap | single, compound, text |
| Partial index | ✓ `WHERE` | ✗ | ✗ | ✓ filtered | ✗ (function-based) | ✓ `partialFilterExpression` |
| Covering index | ✓ `INCLUDE` | `~` trailing keys | `~` trailing keys | ✓ `INCLUDE` | `~` trailing keys | ✗ |
| Expression index | ✓ | ✓ 8.0.13+ | ✓ | ✓ computed column | ✓ | ✗ |
| Operator class / prefix | ✓ opclass, collation | ✓ prefix length | ✓ prefix length | ✗ | ✗ | ✗ |
| Unique NULLs | distinct, or `NULLS NOT DISTINCT` (15+) | distinct | distinct | one NULL | distinct | `sparse` |
| Concurrent build | ✓ `CONCURRENTLY` | ✓ online DDL | ✓ | ✓ `ONLINE` | ✓ `ONLINE` | ✓ background |

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
