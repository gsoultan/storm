# storm — M9 (MySQL): the estimate, made 2026-09-10

**M9 is NOT "a driver project and only that."** `docs/PLAN.md` said that, and it
was wrong. Corrected in place; this is the reasoning.

## What was actually missing

The dialect seam has two implementations of the DECODE side (`runtime/mydec`,
ADR-0007) and two of the DDL side (`compile/myddl`). It has **one** of the QUERY
side: `compile/pgsql` serves both dialects. `codegen.Dialect` selects only the
decoder family and which column types are supported — nothing about SQL text.

So a MySQL-dialect package came out carrying PostgreSQL SQL. Measured against
**MySQL 8.4.11** in the `storm-my` container:

- `SELECT "id" … FROM "my_users"` → **Error 1064**. Default `sql_mode` has no
  `ANSI_QUOTES`, so a double-quoted name is a string LITERAL, not an identifier.
  `myddl.Ident` uses backticks and is correct — but it is DDL-only.
- the insert's output clause → **Error 1064**. MySQL 8 has no such clause.
- `$1` placeholders. MySQL wants `?`. `runtime.Lowering.Placeholder` is
  documented as DECIDED (ADR-0010) and was never built.

`myddl` applies cleanly and always did; `scripts/check/mysql.sh` proves it and
is honestly scoped — it says "does the MySQL **DDL** run" and never claimed more.

## The lesson, which is R9's own one level up

`codegen.TestMySQLGeneratedPackageCompiles` passed throughout. **Compiling and
executing are different claims.** R9's note says a seam whose second
implementation does not build is a bad hypothesis; one that BUILDS while
emitting the other dialect's SQL is worse, because the gate reads as though it
works. Whenever a seam's second implementation is added, the gate has to
exercise the property the seam is FOR, not the nearest cheap proxy.

## What was done about it

`codegen` now REFUSES `DialectMySQL` (`ErrMySQLQueryLoweringMissing`) rather
than emit a package no server accepts — storm's rule is that a construct the
target cannot express is a generation error, and silence is not an option.
storm's own seam tests opt out via `codegen.AllowUnexecutableMySQLForTest()`,
unexported-in-spirit, because the decode property they assert is real.

## `compile/mysql` — DONE 2026-09-10, proven not asserted

Backtick identifiers; the bare `?` via `runtime.Placeholder` (zero value is
PostgreSQL, so pg output is byte-identical); typed-`JSON_TABLE` list lowering;
no output clause on insert. Every form PREPAREd and EXECUTEd against MySQL
8.4.11 by `scripts/check/mysql.sh`, verified to fail with Error 1064 when the
quoting regresses.

**The `JSON_TABLE` COLUMNS declaration must be TYPED to the column it matches.**
Declared `JSON` it compares a JSON scalar against a native value — wrong AND
unindexable. That is why `InFrag` takes a colType and is not in the operator
table. Also: the index-lookup assertion needs ENOUGH ROWS (the check seeds 500);
with three rows MySQL correctly drives from the table and the plan proves
nothing.

## Real scope, remaining

1. ~~`compile/mysql`~~ — done.
1b. **Wire codegen to it.** codegen calls `compile/pgsql` for every statement
   whichever dialect was asked for: 68 functions across 13 files, most for
   constructs `compile/mysql` does not lower yet (joins, aggregates, unions,
   top-N, recursion, upsert, locking). This is why `DialectMySQL` is still
   refused.
2. **The driver.** Unchanged and still real: `go-sql-driver/mysql` decodes into
   `driver.Value` before storm sees it — one boxing allocation per column per
   row, which ADR-0007 exists to refuse. Needs a fork exposing binary result
   rows, or the protocol subset.

The 4-week estimate in PLAN.md counted (2) only. **Re-estimate before starting.**

Reachable MySQL for this work: `root:storm@tcp(192.168.64.3:3306)/`, container
`storm-my`, MySQL 8.4.11.

Related: [[decisions]] (ADR-0007, ADR-0010), [[seam_and_codegen]], [[core]].
