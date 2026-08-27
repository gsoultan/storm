---
tags: [storm, migration, sqlc]
updated: 2026-08-25
---

# Migrating from sqlc

sqlc and storm share a creed — SQL decided at build time, rows mapped without
reflection — so a migration is mostly *deleting* things: the queries file, the
per-query structs, and the hand-rolled dynamic-filter builders that grow beside
every sqlc project. This guide migrates one bounded context; do one, ship it,
repeat.

## What maps to what

| sqlc | storm |
|---|---|
| `schema.sql` (hand-written) | the Go model — `storm import` writes the first draft from your live schema |
| `queries.sql` static queries | generated typed queries: `user.New().Where(...)` |
| `queries.sql` `:one` by PK | `user.New().Where(user.ID.Eq(id)).One(ctx, ex)` |
| hand-rolled dynamic filters | the same builder — every combination compiles once, 0 allocs warm |
| N `LEFT JOIN` + regroup in Go | a named plan: `store.UserFeed()`, 1 + relations round trips, asserted |
| `EXISTS` subqueries | `user.HasPosts()` / `store.UserHavingPosts(q, ps...)` |
| analytical queries | keep the SQL: `storm.SQL[T](...)` — PREPARE-validated, generated scanner |
| `:exec` statements | `storm.SQLExec(...)` — same PREPARE check, must return zero columns |
| `SELECT fn(...)` calls | `storm.SQL[T]` on the function's row; wrap void as `(fn($1) IS NULL) AS done` |
| migration files (hand-written) | `storm diff <name>` emits them; you still review and apply |
| `db.WithTx(tx)` | `pgxdrv.Tx{T: tx}` — same idea, four-method port |

## The steps

1. **Adopt the schema**: `storm import -dsn $DEV > model/model.go`. The draft
   lists everything it could not express as a comment block — re-declare those
   in `Schema` methods before trusting a diff. Then prove the fixpoint:
   `storm diff init` against a scratch database must produce your schema, and
   a second diff must be empty.
2. **Generate**: `storm generate internal/store`, commit the output, add
   `storm verify -stale` and `-pending` to CI next to your existing checks.
3. **Port reads**: static sqlc queries become builder calls; anything sqlc
   couldn't express dynamically was hand-rolled — that code deletes outright.
   Declare `Projections` for your narrow reads (sqlc made you write these as
   separate queries; here they share the builder).
4. **Port relation loads**: wherever you regrouped joined rows in Go, declare
   a plan. `storm lint` then budgets every load pattern in review.
5. **Keep the SQL worth keeping**: your analytical queries move into
   `storm.SQL[T]` declarations verbatim — they gain build-time validation and
   a generated scanner and lose nothing.
6. **Port writes last**: sqlc's `INSERT ... RETURNING` becomes `Create()`;
   note the semantic upgrade — unset columns take DATABASE defaults, because
   absence is a mask, not a zero value. Bulk paths get `InsertAll` (COPY) and
   the `Unit` (FK-ordered, atomic).

## What you gain that sqlc cannot express

Compile-time-safe dynamic queries; relations with asserted round-trip counts;
unloaded relations that do not compile; keyset pagination as a first-class
cursor; `verify -pending` ("changed the model, forgot the migration" fails
CI); `explain` and `lint` as gates. Measured side by side in
`bench/RESULTS.md`: same wall clock, **6 vs 5,022 allocations** and 22× less
memory per thousand-row scan.

## What to watch

- sqlc queries are per-column nullable-aware from the schema; storm's model
  must declare nullability (`*T`) correctly — `storm import` gets this right,
  hand-written models should double-check.
- `:many` queries with `LIMIT $n` map directly; `OFFSET` exists but the doc
  comment will try to talk you into keyset, and it is right.
- Version-column optimistic locking is generated only if the model declares
  `.Version()` — sqlc had no equivalent, consider adopting it.
- `sqlc.arg(name)` becomes positional `$n`. You choose the numbering, and a
  repeated `sqlc.arg` becomes a repeated `$n` — but nothing checks that your
  call sites pass arguments in the order you chose. Test it (below).
- Two queries may share a row type only if their SELECT lists agree column
  for column — scanners decode by position, and generation enforces the
  agreement rather than transposing silently.
- In raw rows, nullable columns are `runtime.Null[T]`, not `*T`.

## What the first real migration taught (anubis/authz, 2026-08-25)

One day, 44 queries, sqlc deleted from the context, authorize p95 unchanged.
Three patterns from it are worth prescribing:

**The SQL-owns-semantics variant.** anubis's queries call database functions
(`authorize()`, `membership_assign()`) and carry guarded UPDATEs by design —
ADR'd, deliberate, not builder material. That migration is mostly steps 5–6:
`storm.SQL[T]`/`SQLExec` declarations in **one designated package per
context** (files mirroring the old `queries/` layout so diffs read side by
side), plus builder queries where the shape is a plain table read. The
builder count being small is fine; the property that matters — every
statement schema-checked at build time — covers both forms equally.

**PREPARE against the live dev database when migrations are the schema of
record.** storm's own `generate` PREPAREs against the *model* in a scratch
schema, so the model vouches for every query. If your functions and views
live only in migrations, the model cannot vouch for calls into them — write
a small adopter-owned generate command (anubis's is ~100 lines: connect,
PREPARE each declaration, `codegen.ResolveRawScanner`, `codegen.Package`)
and point it at the dev database. Say in its doc comment that the model is a
projection and migrations remain the truth.

**PREPARE proves shape, not argument order.** The one thing generate-time
checking cannot see is whether call sites pass `$1, $2` in the order you
numbered them. The anubis acceptance suite is the template: for each query
family, assert VALUES against a direct-SQL twin, assert the not-found
mapping, and run every write inside a deliberately rolled-back transaction —
which also proves your executor adapter keeps ambient transactions ambient.

Domain layers that speak string ids (sqlc's uuid→string override) keep that
contract by selecting uuid columns `::text` and binding strings — pgx's uuid
codec accepts them for parameters — with one `[16]byte`↔string crossing next
to the executor adapter for builder queries.
