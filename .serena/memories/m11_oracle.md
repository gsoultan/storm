# M11: the Oracle estimate, and the back end built on it

`internal/oraclespike`, a separate module. Three questions asked before M11
starts, per the rule PLAN.md set at M9. The third is M12's kill criterion.
See [[m10_sqlserver]] for the precedent and [[production_readiness]] for P6.7.

## The headline: M12 is NOT cancelled

PLAN.md said "capability model cannot carry Oracle → Mongo is cancelled."
Measured, it can. But **half the mechanism DIALECTS.md described is impossible**,
and the reason is worth keeping:

storm has TWO DSLs that carry a value.
- **Expression DSL** — `storm.Exprs{}.Eq(&m.Tag, "")` — takes `any` and folds a
  Go literal into a `schema.Literal` at BUILD time. Visible, refusable.
- **Query DSL** — the generated `func (h TextCol) Eq(v string) Pred` — takes a
  runtime string and binds it. The generator never sees the value.

So `Eq("")` in a query can never be a declare-time error. The schema a `Check`
receives has tables, columns, enums, functions, views and triggers, and no
queries at all.

**The half that survives is enough**, and the load-bearing fact is **ORA-01400**:
an empty string reaching a `NOT NULL` column is an ERROR. So the difference is
silent in exactly ONE place — the **nullable text column**, where `""` and NULL
become one stored value — and refusing that column makes everything else
consistent, including `Eq("")`, which then correctly matches nothing because
nothing can be `''`.

The IR's function registry (`schema/expr.go`) has only `lower`, `upper`, `abs`,
`coalesce`, `nullif`. No `SUBSTR`, so the expression leak (`SUBSTR('abc',1,0)` is
NULL here, `""` on PostgreSQL) is UNREACHABLE through the model DSL. Reachable
through `storm.SQL`, which is outside the capability model already.

Whether the rule REFUSES nullable text or allows it and makes the collapse
visible in the generated type is M11's first design decision, not the spike's.

## The driver: 26.3 allocations per row

Through `driver.Rows` directly (the most favourable measurement), 8 columns:

| | allocs/row | per column |
|---|---|---|
| go-ora v2.9.0 via `driver.Rows` | **26.3** | 3.3 |
| go-ora v2.9.0 via `database/sql` | 26.5 | 3.3 |
| go-mssqldb (M10) | 11.3 | 1.4 |
| `runtime/msdrv` | 0.09 | — |
| `runtime/mydrv` | 1.07 | — |

The 0.2 gap between the two go-ora rows says the cost is the DRIVER's, not
`database/sql`'s — so "use database/sql better" is not an available answer.
Same conclusion as M9 and M10 by a wider margin: **M11 is a lowering AND a
client.**

Two go-ora defects found while measuring, both on first-day statements:
`SELECT SUBSTR('abc',1,0) FROM dual` returns "sql: no rows in result set" (it
drops a row whose only column is NULL — adding a second non-NULL column fixes
it, which is how the two were told apart), and `SELECT LENGTH(''), 1 FROM dual`
fails with "TTC error: received code 3". A wrapper strategy would inherit both.

## Constructs: everything but the work queue

Present: `MERGE`, `OFFSET…FETCH`, recursive CTEs, window functions,
`CROSS APPLY`, `JSON_TABLE`, `GROUPING SETS`, `GROUPING()`, `FOR UPDATE SKIP
LOCKED`, native `BOOLEAN` (23c — so DIALECTS.md's `NUMBER(1)`+`CHECK` lowering
may never be needed), `IDENTITY`, 128-char identifiers, and a partial UNIQUE via
a **function-based index on a CASE** (a NULL key is not indexed — same guarantee
as a filtered index). That is the third target in a row where soft delete's
live-scoped uniqueness survives; MySQL is the odd one out, not PostgreSQL.

**Missing: `FETCH FIRST … FOR UPDATE SKIP LOCKED` → ORA-02014.** Oracle
implements `FETCH FIRST` as an inline view and refuses `FOR UPDATE` against one.
This is the work-queue shape — one statement on every other target.
`WHERE ROWNUM <= n … FOR UPDATE SKIP LOCKED` works, but **`ROWNUM` is applied
before `ORDER BY`**, so a bounded ORDERED locked fetch is a subquery. A real
lowering with a real behaviour difference.

**Unquoted identifiers fold UP**, where PostgreSQL folds down, and `fold_probe`
and `"fold_probe"` are two tables. Generation is unaffected (storm quotes
everything); `storm import` reads a catalogue that shouts, so `schema/oracle`
needs a folding rule `schema/pg` and `schema/mssql` did not.

## What running the back end added (2026-09-22, after the estimate)

`compile/oraddl` and `compile/oracle`'s core landed the same day, gated from
`internal/oraclespike`. Six findings the documentation did not give — and the
two that generalise are METHOD findings, not Oracle ones:

- **A gate that EXECUTES catches what a gate that renders cannot.** Two
  statements Oracle refuses outright PREPARED without error, because go-ora's
  `Prepare` never reaches the server. A PREPARE that does not round-trip is the
  same shape of defect as a gate that passes by skipping.
- **A probe that counts ROWS catches what one checking "does it parse" cannot.**
  The row-constructor probe passed on parse and would have passed on a
  constructor that silently compared only the leading column. Counting rows,
  with a tie on that column, is what made the answer trustworthy.

The Oracle findings:

| finding | consequence |
|---|---|
| unquoted identifiers may not START with `_` — ORA-00911 | every internal alias goes through `Ident`; no other target cares |
| a JSON path must be a LITERAL — ORA-00907 — and nothing enumerates keys | `HasAnyKey`/`HasAllKeys` both REFUSED. Reverses the first draft: SQL Server does one through OPENJSON, Oracle neither |
| the row constructor WORKS for inequality | `RowCmpExpand = false`. Closer to PostgreSQL than to SQL Server; removes a whole expansion |
| native JSON needs an ASSM tablespace — ORA-43853 in SYSTEM | the gate runs as an application user in USERS |
| a trailing `;` is ORA-00911 through the protocol | `oraddl.Statements` is the primitive, `Create` (the FILE) built from it |
| no shared ROW lock, and no capped+locked read | three of seven lock modes refused; the work-queue shape refused by name |

**`compile/oraddl`'s empty-string rule is the capability decision made real**: a
nullable text column is refused, and that single refusal is what makes `Eq("")`
— which can never be a declare-time error — correctly match nothing.

Two places Oracle is EASIER than SQL Server: a bare `numeric` ports (Oracle's
`NUMBER` keeps the fraction where an unspecified `DECIMAL` truncates it), and a
partial UNIQUE ports as a function-based index on a `CASE`.

Gate state: the DDL applies, 29 lowered statements execute, both refusals are
asserted against the server.

## M11 RUNS (2026-09-23)

`storm generate -dialect oracle` works, and the whole stack is gated: model →
generator → generated package → `runtime/sqldrv` → go-ora → Oracle. The
generated package inserts, selects, pages and enforces its soft-delete unique
against a real server.

### The architectural change: a second row shape

`runtime.Rows` gained `Values() []any` beside `RawValues() [][]byte`. A
generated package calls exactly ONE, chosen at GENERATE time by the dialect —
no run-time branch, the same rule every other dialect decision follows. An
adapter implements one and returns nil from the other; that is a CONTRACT
rather than a type, because the alternative is a second Executor interface and
every decorator written twice.

**The consequence is wider than Oracle: any `database/sql` driver satisfies the
port now.** That was not why it was added.

- `runtime/valdec` — the fourth decoder family, the first that reads no bytes.
- `runtime/sqldrv` — a generic `database/sql` adapter. Documents what it does
  not do: CopyFrom emulated one INSERT per row, Batch N round trips.

**The measurement that made it possible**: every Oracle NUMBER arrives as an
exact decimal STRING (2^53+1 and 34 significant digits both intact). A float64
would have rounded both, so valdec REFUSES a float for an exact numeric — the
precision is gone by then and the error is the only place to say so.

### What running it found, beyond the lowering

| finding | where |
|---|---|
| a `[16]byte` argument is a BULK-INSERT REQUEST to go-ora ("all parameters should be arrays") | every insert failed; `sqldrv.normalize` converts it, which database/sql required anyway |
| a fixed-text insert must NUMBER its own placeholders — `:` alone is ORA-01745 | same defect M10 hit with a bare `@`, and it reached a live server both times |
| `loweringFor` had no case for Oracle | shipped one commit emitting POSTGRES SQL with Oracle decoders, and the package compiled, used valdec and read Values — all three checks true |
| the root package file emitted a HAVING decoder call without importing the family | a generated **SQL Server** package with a HAVING count had been uncompilable too |
| go-ora reports a 23c native BOOLEAN as NUMBER and yields "1" | a bool-only decoder would read every true as false |
| storm.Decimal holds 18 significant digits, so numeric(19,4) is refused by codegen | a storm rule, not an Oracle one; oraddl took the same model happily |

The `loweringFor` one is the most instructive: it was found by a test written to
cover a COVERAGE DIP, which is the argument for covering new code rather than
lowering a floor to fit it. Three gates now stop it recurring —
`TestEveryDialectFillsTheSeam` walks the struct rather than naming fields,
`TestOracleGeneratedPackageCarriesOracleSQL` is the MySQL gate's namesake, and
the outsider gate checks the generated package binds with Oracle's placeholder.

### Still not built

`schema/oracle` introspection (needs the case-folding rule), migrate's Oracle
half, MERGE/upsert. `storm diff`, `verify`, `explain`, `import` and `watch`
refuse by name.

A NATIVE TTC client is still the open question for performance — 26.3
allocations per row against 0.09 — and the estimate correction stands: TDS and
MySQL's protocol have public specifications and TTC does not.

## Operational notes

- Oracle Free is **multi-arch from 23.5**: the engine in CI and on an Apple
  silicon laptop are the SAME one. No Azure SQL Edge caveat like M10's.
  `gvenzl/oracle-free:slim`, `ORACLE_PASSWORD`, service `FREEPDB1`.
- The workflow is `.github/workflows/oracle-spike.yml`, triggered on
  `internal/oraclespike/**` rather than on every push, because it is a
  MEASUREMENT and not a gate. When there is an Oracle back end it moves into
  `ci.yml` beside the sqlserver job.
- The spike's tests **report**; only three assertions fail the build, and they
  are the three the kill criterion rests on. A missing construct is output.
- Positions come from `tool/positions.go` (which has the AST), not from
  `storm.Build`. DIALECTS.md's "pointer to the model line" exists through
  `storm portable`, not through a library call. Same for `msddl.Check`.

Estimate raised **4 → 6 weeks** for the SQL, and the CLIENT is now known to be
the wrong shape of estimate entirely: TDS and MySQL's protocol have public
specifications and TTC does not. That is the single biggest correction this
milestone made to its own plan.
