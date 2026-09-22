# M11: the three questions, asked before M11 starts

`docs/PLAN.md` set the rule at M9 and it has applied at every target since: the
driver question is "a fork that exposes the result rows, or an implementation of
the protocol subset storm needs — that is the estimate to make before starting,
not after."

M11 has a **third** question, and it is the one that matters most. PLAN.md makes
the capability model's ability to carry Oracle the **kill criterion for M12**:
"capability model cannot carry Oracle → Mongo is cancelled." So it is asked
here, before anything is built on the assumption that it can.

A SEPARATE module, so storm's own `go.mod` gains no Oracle dependency for a
measurement.

## Running it

Oracle Free is **multi-arch from 23.5**, so unlike SQL Server there is nothing
to caveat: the engine in CI and the engine on an Apple silicon laptop are the
same one. (M10 had to run Azure SQL Edge locally, a 2019-level subset, and
reserve the authoritative answer for CI.)

    container run -d --name storm-oracle -p 1521:1521 \
      -e ORACLE_PASSWORD=Storm1Passw0rd gvenzl/oracle-free:slim

    STORM_ORACLE_DSN='oracle://system:Storm1Passw0rd@localhost:1521/FREEPDB1' \
      go test -v ./...                                      # results 1 and 3
    STORM_ORACLE_DSN=... go test -run XXX -bench . -benchtime 30x   # result 2

Or push a change under `internal/oraclespike/`, which is what
`.github/workflows/oracle-spike.yml` triggers on. It is a workflow of its own
rather than a job in `ci.yml`, and it runs on the spike rather than on every
push, because this is a MEASUREMENT and not a gate — there is no Oracle back end
to protect yet.

**The tests report; they do not refuse.** A construct that turns out to be
missing is this file's output. Three facts DO fail the build, and they are the
three the kill criterion rests on — see result 3.

All numbers below: Oracle Database Free 23.x, `gvenzl/oracle-free:slim`,
ubuntu-latest, 2026-09-22.

## Result 1: the engine has everything M11 needs but one

| M11 needs | Have | Notes |
|---|---|---|
| `OFFSET … FETCH` | yes | the paging gate; 12c and later |
| `MERGE` | yes | the upsert, a STATEMENT as on SQL Server — no `ON CONFLICT` |
| recursive CTEs | yes | |
| window functions | yes | |
| `CROSS APPLY` | yes | this is LATERAL, same keyword as SQL Server |
| `JSON_TABLE` | yes | one bound value for a whole list — the IN-list trick, as on MySQL |
| `GROUPING SETS`, `GROUPING()` | yes | so the three MySQL cannot take port here, as they did to SQL Server |
| `FOR UPDATE SKIP LOCKED` | yes | |
| **`FETCH FIRST` *with* `FOR UPDATE`** | **NO** | **ORA-02014.** See below |
| `ROWNUM` with `FOR UPDATE` | yes | the replacement |
| partial UNIQUE | yes | not a partial index — a function-based unique index on a `CASE`, because a NULL key is not indexed. Same guarantee |
| native `BOOLEAN` | yes | 23c. `NUMBER(1)` + `CHECK` is not needed on this engine |
| `IDENTITY` column | yes | 12c; no sequence object to manage |
| 128-character identifiers | yes | 12.2 and later. 30 would have been too short for `ck_<table>_<column>` |

Two of these move things that were written down as open questions.
`docs/DIALECTS.md` says "Oracle has no native `BOOLEAN` before 23c → `NUMBER(1)`
plus a `CHECK`"; on the engine storm would target, it does. And the partial
UNIQUE is the third target in a row where soft delete's live-scoped uniqueness
survives — MySQL is the odd one out, not PostgreSQL.

### The one that is missing is the work queue

```
SELECT a FROM t ORDER BY a FETCH FIRST 1 ROWS ONLY FOR UPDATE SKIP LOCKED
ORA-02014: cannot select FOR UPDATE from view with DISTINCT, GROUP BY, etc.
```

`LIMIT n … FOR UPDATE SKIP LOCKED` is one statement on PostgreSQL, MySQL and SQL
Server, and it is how every job table storm generates for is read. Oracle
implements `FETCH FIRST` as an inline view with a window function, and refuses
`FOR UPDATE` against one.

`WHERE ROWNUM <= n … FOR UPDATE SKIP LOCKED` works, so the lowering is known
rather than merely absent — but it is **not the same statement**: `ROWNUM` is
applied before `ORDER BY`, so a bounded *ordered* locked fetch needs the order
in a subquery and the bound outside it. That is a real lowering with a real
behaviour difference, and it belongs in M11's estimate rather than in its third
week.

### And one thing no other target does

Unquoted identifiers fold **UP** here, where PostgreSQL folds them down.

```
declared `fold_probe`, catalogue says "FOLD_PROBE"
after declaring both `fold_probe` and "fold_probe": 2 table(s)
```

storm quotes every identifier it writes, so generation is unaffected. But
`storm import` reads a CATALOGUE, and what it finds there is `USERS`. A model
generated from that would declare Go fields from shouting names; a model written
by hand would diff against it forever. `schema/oracle` needs a folding rule that
`schema/pg` and `schema/mssql` did not.

## What running the back end added

M11 started after the three results below, and `compile/oraddl` and
`compile/oracle` are gated from this module. Six things came out of running
them that reading the documentation did not give:

| finding | consequence |
|---|---|
| **An unquoted identifier may not start with an underscore** — `_storm_k` is ORA-00911 | Every internal alias storm invents goes through `Ident`. No other target cares |
| **A JSON path must be a LITERAL** — `JSON_EXISTS(doc, '$.' || k)` is ORA-00907, and nothing enumerates a document's keys | `HasAnyKey` and `HasAllKeys` are both REFUSED. This reverses the first draft, which claimed Oracle was richer than SQL Server here: SQL Server can do one of the two through OPENJSON, Oracle neither |
| **The row constructor WORKS for inequality** | `RowCmpExpand` is false. The one place this back end is closer to PostgreSQL than to SQL Server, and it removes a whole expansion |
| **`go-ora`'s `Prepare` never reaches the server** | The gate EXECUTEs. Two statements Oracle refuses outright prepared without error — a PREPARE that does not round-trip proves nothing |
| **The native JSON type needs an ASSM tablespace** — ORA-43853 in `SYSTEM` | The gate runs as an application user in `USERS`, which is what an application does anyway |
| **A trailing semicolon is ORA-00911** through the protocol | `oraddl.Statements` is the primitive and `Create` (the migration FILE) is built from it, rather than a splitter stripping a terminator storm added |

The first two were caught because the gate EXECUTES rather than renders, which
is P6.7 again. The third was caught because the probe counted ROWS rather than
checking that a statement parsed — a row constructor that silently compared only
the leading column would have passed a weaker test.

## Result 2: the driver costs 26 allocations per row

```
BenchmarkDriverRows200x8-4        30    949379 ns/op   200.0 rows/op   271950 B/op   5259 allocs/op
BenchmarkDatabaseSQLRows200x8-4   30   1199658 ns/op   200.0 rows/op   282471 B/op   5299 allocs/op
```

Measured the most favourable way for the library — through `driver.Rows`
directly, bypassing `database/sql` — exactly as the MySQL and SQL Server spikes
measured theirs.

| | allocations per row | per column (8 columns) |
|---|---|---|
| go-ora v2.9.0, via `driver.Rows` | **26.3** | 3.3 |
| go-ora v2.9.0, via `database/sql` | 26.5 | 3.3 |
| go-mssqldb (M10's measurement) | 11.3 | 1.4 |
| storm's `runtime/msdrv` | 0.09 | — |
| storm's `runtime/mydrv` | 1.07 | — |

**The answer is the same as M9's and M10's, and by a wider margin.** The gap
between the two rows above is 0.2 allocations per row, which says the cost is
the driver's and not the standard library's: `database/sql` adds almost nothing
here, so a "just use `database/sql` better" answer does not exist. And 3.3 per
column is not the one boxing allocation ADR-0007 predicted — go-ora allocates
about three times that before storm could see a byte.

So M11 is a lowering **and** a client, as M9 and M10 were. That is the estimate.

### Two go-ora defects found while measuring

Neither is about Oracle, and both reproduce on a statement a first-day user
would write.

```
SELECT SUBSTR('abc', 1, 0) FROM dual   →  sql: no rows in result set
SELECT LENGTH(''), 1 FROM dual         →  TTC error: received code 3 during response reading
```

The first drops a row whose only column is NULL — from `dual`, which cannot
return none. Adding a second, non-NULL column makes it work, which is how the
two were told apart. The second fails in the protocol layer.

This is worth recording for its own sake: the driver storm was evaluating
mis-handles NULL text scalars, and a wrapper strategy would have inherited both.

## Result 3: the capability model CAN carry Oracle

### The claim as written is half impossible

`docs/DIALECTS.md` says:

> Declaring `oracle` in `portability.assert` makes any `Eq("")` or non-null-
> constrained text column a **declare-time error** with a pointer to the model
> line.

storm has **two** DSLs that carry a value, and they differ:

- The **expression** DSL — `storm.Exprs{}.Eq(&m.Tag, "")`, used in checks,
  generated columns and index predicates — takes `any` and folds a Go literal
  into a `schema.Literal` at BUILD time. `""` there is visible.
- The **query** DSL — the generated `func (h TextCol) Eq(v string) Pred` — takes
  a runtime string and binds it as a parameter. The generator never sees the
  value.

So `Eq("")` **in a query cannot be a declare-time error**, and no capability
model can make it one. The schema a `Check` receives has tables, columns, enums,
functions, views and triggers in it, and no queries at all.

### But the half that survives is enough

Everything measured against the server:

| probe | result |
|---|---|
| `INSERT ''` into a nullable `VARCHAR2` | reads back as NULL |
| the same through a **bound parameter** | reads back as NULL — the driver does not normalise it away |
| **`INSERT ''` into a `NOT NULL` column** | **ORA-01400** |
| `WHERE req = ''` | 0 rows |
| `""` and NULL, as stored values | **1 distinct value** |
| `SUBSTR('abc',1,0)` | NULL (PostgreSQL: `""`) |
| `'' \|\| 'x'` | `'x'` — concatenation does not propagate the NULL |

The load-bearing one is **ORA-01400**. It means the difference is only ever
*silent* in one place: a **nullable text column**, which is the one place `""`
and NULL become the same stored value. Everywhere else it is an error, and an
error is something an application can see.

So the rule is a column rule, and `capability_test.go` is the working prototype:

```
ora_orgs.note: a NULLABLE text column cannot round-trip on Oracle — an empty
string is stored as NULL, so "" and nil are one value. Make it NOT NULL, or
move it to a table whose absence means absent
```

Refusing that column **makes the rest consistent**:

- nothing can BE `""`, so `Eq("")` matching nothing is the RIGHT answer rather
  than a wrong one — which is why the impossible half of the claim does not need
  to be implementable;
- writing `""` to a required column is ORA-01400 rather than a silent NULL;
- the IR can emit only `lower`, `upper`, `abs`, `coalesce` and `nullif` as
  functions, and none of the text ones can turn a non-empty string into an empty
  one. `SUBSTR` is not in `schema/expr.go`'s registry, so the leak the table
  above records is **not reachable** through the model DSL. It is reachable
  through `storm.SQL`, which is the escape hatch and is outside the capability
  model by the same rule that already puts it there.

**Therefore: the capability model carries Oracle, and M12 is not cancelled.**

### Two notes for whoever writes `compile/oraddl`

**The position comes from the CLI, not from `storm.Build`.** DIALECTS.md's
"pointer to the model line" is attached by `tool/positions.go`, which has the
AST. A model built by a library call has no positions, so the prototype reads
`c.Pos` when it is there and says the table and column when it is not — exactly
as `msddl.Check` does.

**A declared CHECK is the model's own SQL** and every back end passes it through
unchanged, so storm cannot respell a `''` inside one. The prototype warns rather
than refuses, and names why. That is the same category as `storm.SQL`.

### The shape of the rule is a choice M11 makes, not this spike

Refusing every nullable text column is safe and costs a very common modelling
shape. The alternative is to ALLOW it and make the collapse visible in the
generated accessor's type, so `""` and `nil` are one value the caller is told
about rather than one they discover. Both are build-time honest; only the first
is lossless. The evidence above supports either, and picking one is M11's first
design decision rather than this spike's.
