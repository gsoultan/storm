# The DDL seam inside `migrate/`

Landed 2026-09-20, after v1.1.0. Before it, `migrate/` wrote every statement in
PostgreSQL at 39 call sites, so `storm diff` and `storm verify` refused three of
the four shipped dialects. See [[m10_sqlserver]] and [[automigrate]].

## The shape

`migrate/ddl.go` holds a `ddl` struct of function values, chosen once per
dialect by `ddlFor(dialect, enums)`. It is `codegen.lowering`'s shape for
`codegen.lowering`'s reason: the dialect is known before the first statement is
rendered, so it is decided ONCE, and a back end with no such statement says so
by leaving the field **nil** — something an interface cannot express without a
second method to ask first.

`migrate/ddl_mssql.go` is the SQL Server set. `postgresDDL()` is what the
package used to inline, moved and not rewritten.

Deliberately NOT behind the seam, each for a stated reason in the file:
comparison (`narrowing`, `canonical`, `sameKeys` — questions about the model,
not about SQL), routines (`routinediff.go`; no other target has a routine model
in `schema` yet, so they are ABSENT rather than nil), and introspection.

`Diff(from, to) Plan` is unchanged and still returns no error.
`DiffFor(from, to, dialect) (Plan, error)` is the general form.
`TestPostgresRenderersCannotFail` reflects over the struct and fails loudly when
a new renderer that CAN fail is added — the thing that would silently make
`Diff`'s dropped error a lie.

## Three cuts that are not obvious

**The enum step is a whole STEP, not three statements.** The back ends disagree
about what an enum IS. PostgreSQL has a TYPE created, extended and dropped
independently of its columns; SQL Server has none — the labels are a CHECK
constraint per column. `Enums` returns an ERROR on SQL Server if either schema
still declares one, because an un-normalised model would propose to retype every
enum column and drop every enum CHECK. Refusing beats emitting that.

**`SetNotNull` and friends take the whole column.** SQL Server's `ALTER COLUMN`
restates the type on every change, including one that is only about nullability.

**Setting a default is ONE change even where it is two statements.** SQL Server's
default is a named constraint and `ADD CONSTRAINT … DEFAULT` on a column that has
one is error 1781. `mssqlDDL.SetDefault` emits the guarded drop AND the add; the
shared loop stays as it was, so PostgreSQL gained no step.

## Normalisation needs a second connection

`NormalizeMSSQL` / `ForMSSQL` take a `MSSQLDialer`, not a connection: a SQL
Server session is bound to its database at LOGIN, so the scratch **database**
cannot be reached from the connection that found the target. A scratch database
rather than a scratch schema because there is no `search_path` — the same
conclusion `tool/rawmssql.go` reached independently.

## What the gate is

`scripts/check/mssql.sh` APPLIES a plan and demands the NEXT plan be empty, nine
alters deep, and COUNTS the subtests. An empty second plan is the only evidence
that normalisation and introspection agree about widths, parenthesised defaults
and the CHECK an enum became; a migration that reapplies itself forever is what
disagreement looks like in production. Four of the five differently-spelled
statements fail in a way no text assertion sees (`ADD COLUMN` parses as a column
named `COLUMN`; an `ALTER COLUMN` omitting `NULL`/`NOT NULL` takes
`ANSI_NULL_DFLT_ON`'s answer; a second `DEFAULT` constraint is 1781; `DROP INDEX`
without `ON` does not parse). P6.7 again — see [[production_readiness]].

## Found while building it

**A new table's indexes were dropped on the floor** by any back end whose
`CreateTable` does not append them. `pgddl.CreateTable` does; `msddl.Create`
emits them in a second pass over the whole schema that a diff building one table
at a time never reaches. Now pinned for both dialects by one test. This is the
fourth defect in already-shipping code found by adding a dialect rather than by
testing the one it was in.

## Still open

`migrate.Auto` is PostgreSQL-only. The plan engine speaks this catalogue; the
applier does not. It needs `sp_getapplock` rather than `pg_advisory_lock`, and
an answer for what `NoTransaction` means where there is no concurrent index
build — `WITH (ONLINE = ON)` is Enterprise-only, so `Plan.Concurrently` is the
identity on SQL Server and says so.

`verify -pending` and `-stale` replay migration files through a scratch
PostgreSQL schema and refuse other targets by name.
