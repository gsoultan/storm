# Indexing — the whole grammar round-trips, and what that cost to learn

Read with [[production_readiness]] and `docs/REFERENCE.md` §2a. Built 2026-09-05.

## What exists
`schema.Index{Name, Columns, Unique, Method, Where, Include, NullsNotDistinct,
With []StorageParam, Invisible}`; `IndexColumn{Name, Expr, Desc, NullsLast,
NullsFirst, OpClass, Collate, Prefix}`. DSL: `t.Index(keys...).Using/Unique/
NullsNotDistinct/Include/With/Where/Invisible/Named`; key modifiers
`storm.Desc/NullsLast/NullsFirst/OpClass/Collate/Prefix/Lower/Upper/IndexExpr`,
all accepting a field OR a key so they compose. **FK columns are auto-indexed
by Build pass 5** (`indexedFirst`) — that predates this work.

## The three lessons

1. **PostgreSQL prints only the NON-DEFAULT NULL placement.** `x ASC NULLS
   LAST` and `x DESC NULLS FIRST` come back bare. A model that says them
   would be dropped and recreated on every diff — so `indexvalidate.go`
   REFUSES the default placement. Any new index fact must be checked the same
   way: emit → `pg_get_indexdef` → parse → equal, or refuse it.

2. **Extensions install into the FIRST schema of the search path.** In a
   scratch namespace (round-trip test, `verify -pending`, model normalisation)
   that is the scratch schema, and it vanishes with it; `IF NOT EXISTS` is
   then satisfied by the wrong copy and a named opclass "does not exist".
   Fix: `CREATE EXTENSION ... WITH SCHEMA public` and the emitter qualifies
   storm-managed classes as `public.gin_trgm_ops`; the parser strips ANY
   schema qualifier; the differ compares bare names. A user on Supabase
   writes `"extensions.gin_trgm_ops"` and storm's install is a no-op.

3. **A multi-statement simple-query string is one implicit transaction**, so
   `SET search_path TO x; CREATE INDEX CONCURRENTLY ...` fails with "cannot
   run inside a transaction block". Replay sets the path on the session, then
   Execs each file alone. `tool/db_test.go` proves both the refusal and the
   fix. `diff -concurrently` writes one file per concurrent statement under
   `-- storm:no-transaction`; only indexes on tables that exist in the LIVE
   schema are rewritten (`Plan.Concurrently(live)`).

## Ground truth used for the parser
Captured from PG17 (`/tmp/ixdef` probe, 2026-09-05):
`(lower(email) COLLATE "C" text_pattern_ops DESC NULLS LAST, n) INCLUDE (name, ts)
NULLS NOT DISTINCT WITH (fillfactor='70') WHERE (deleted_at IS NULL)`;
`WITH (fastupdate=off)` unquoted booleans; `(((n + 1)), upper(name))` —
arithmetic doubly wrapped, function calls bare; `(n NULLS FIRST, ts DESC)`.
The parser tests in `schema/pg/parse_test.go` hold these strings verbatim.

## Not built, deliberately
- `spatial` (no geometry column type to index), per-extension schema config
  (write the qualified class instead), invalid-index detection in introspection.
