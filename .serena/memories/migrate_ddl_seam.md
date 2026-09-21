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
conclusion `tool/mstool/raw.go` reached independently.

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

## What the first live run found

The gate paid for itself on its first CI run, which is the argument for
writing it before the code is believed rather than after.

**`EXEC()` will not take a function call.** Its argument may only be variables
and string literals concatenated, and `QUOTENAME` is a function — so the
dropped-default statement did not parse. The variable holds the WHOLE statement
now and the quoting happens in the `SELECT`. Four of nine alters had already
applied; the other five were the same statement re-emitted, because a failed
apply leaves the change outstanding.

**The gate reported five failures and not one word about why.** `grep` kept the
`--- FAIL` header and dropped the statement and the server's error printed under
it. It uses `-A` now. Same lesson as P7, one level down: a gate that cannot say
what broke costs a CI cycle every time it fires.

The live run also caught a **wording regression in PostgreSQL** — the
partitioning refusal lost its text when the seam refactor rebuilt `diffTable`.
An audit comparing every string literal before and after the rewrite found it
and confirmed nothing else had moved.

Second run: 9 of 9 alters applied, both round trips empty.

## Found while building it

**A new table's indexes were dropped on the floor** by any back end whose
`CreateTable` does not append them. `pgddl.CreateTable` does; `msddl.Create`
emits them in a second pass over the whole schema that a diff building one table
at a time never reaches. Now pinned for both dialects by one test. This is the
fourth defect in already-shipping code found by adding a dialect rather than by
testing the one it was in.

## The package split it forced

`tool/mstool` exists because of a coverage floor. `tool` was one package split
across two CI jobs — PostgreSQL and no SQL Server in one, SQL Server and no
PostgreSQL in the other — so no floor either job could measure meant anything,
and `scripts/check/coverage.sh` had nudged it down twice with a note saying to
MOVE THE CODE next time. This change drifted it again (64.4 against 65), so the
code moved: `storm import`, the `storm.SQL` validator and the dialer, floored in
`scripts/check/mssql.sh` where they run. `tool` went back to 70 and measures
77.7; `mstool` measures 71.6.

The general rule, worth reusing: when a floor keeps drifting, the question is
which half of the package the drifting code belongs to, not what the number
should be.

## Automigrate, and the last two commands

`AutoMSSQL` landed the same day. Same promises as `Auto`, three kept differently
— `sp_getapplock` not `pg_advisory_lock`, `SET LOCK_TIMEOUT` not `lock_timeout`,
and NOTHING where PostgreSQL points a `search_path` — and one kept more easily:
it is ONE transaction, always, because neither of PostgreSQL's two plan splits
exists here and SQL Server's DDL is transactional. `SET XACT_ABORT ON` is what
makes that true rather than nearly true.

**The namespace guard is the interesting one.** There is no `search_path`, so
unqualified DDL lands in the LOGIN's default schema and nothing in a session can
change that. `AutoMSSQL` and `ForMSSQL` therefore REFUSE when the schema they
were asked to migrate is not the one the DDL will land in. The alternative is a
plan that never empties.

**msdrv's Error() drops the server's message text on purpose** — a SQL Server
message embeds row DATA ("The duplicate key value is (alice@example.com)") — so
neither the lock nor the guard can smuggle a sentence through `RAISERROR`. Every
ad-hoc RAISERROR is error 50000. The guard reads `SELECT SCHEMA_NAME()` and
writes its own refusal; the lock raises a distinctive RAISERROR **state** and
asserts on `interface{ ServerState() uint8 }`, so `migrate` links no driver.
It does NOT read the procedure's return value: a batch shaped
`EXEC proc; SELECT @r` puts the proc's DONEPROC ahead of the SELECT's metadata
and msdrv stops a result set at one.

`storm verify -pending` and `-stale` work too. `-stale` needed only the gate
lifted; it was already dialect-generic. `-pending` replays into a scratch
DATABASE, one `Exec` per FILE — which is what a runner does and what a file
holding a `DECLARE` requires.

Two more found on the way: `NormalizeMSSQL` needed a process-wide mutex (the
per-pid scratch name is self-cleaning, which left goroutines in one process
sharing it), and the CI step for the `storm.SQL` validator had pointed at
`./tool/` since the package split — `go test -run` with no matches is a success,
so it had quietly stopped gating for two commits.

## Still open

Nothing on this thread. The next target is the Oracle spike (M11): measure
`go-ora`'s allocations, find an arm64 container, and test whether the capability
model carries empty-string-is-NULL, which is the documented kill criterion for
M12.
