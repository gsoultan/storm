---
tags: [raorm, migration, sqlc]
updated: 2026-08-25
---

# Migrating from sqlc

sqlc and raorm share a creed — SQL decided at build time, rows mapped without
reflection — so a migration is mostly *deleting* things: the queries file, the
per-query structs, and the hand-rolled dynamic-filter builders that grow beside
every sqlc project. This guide migrates one bounded context; do one, ship it,
repeat.

## What maps to what

| sqlc | raorm |
|---|---|
| `schema.sql` (hand-written) | the Go model — `raorm import` writes the first draft from your live schema |
| `queries.sql` static queries | generated typed queries: `user.New().Where(...)` |
| `queries.sql` `:one` by PK | `user.New().Where(user.ID.Eq(id)).One(ctx, ex)` |
| hand-rolled dynamic filters | the same builder — every combination compiles once, 0 allocs warm |
| N `LEFT JOIN` + regroup in Go | a named plan: `store.UserFeed()`, 1 + relations round trips, asserted |
| `EXISTS` subqueries | `user.HasPosts()` / `store.UserHavingPosts(q, ps...)` |
| analytical queries | keep the SQL: `raorm.SQL[T](...)` — PREPARE-validated, generated scanner |
| migration files (hand-written) | `raorm diff <name>` emits them; you still review and apply |
| `db.WithTx(tx)` | `pgxdrv.Tx{T: tx}` — same idea, four-method port |

## The steps

1. **Adopt the schema**: `raorm import -dsn $DEV > model/model.go`. The draft
   lists everything it could not express as a comment block — re-declare those
   in `Schema` methods before trusting a diff. Then prove the fixpoint:
   `raorm diff init` against a scratch database must produce your schema, and
   a second diff must be empty.
2. **Generate**: `raorm generate internal/store`, commit the output, add
   `raorm verify -stale` and `-pending` to CI next to your existing checks.
3. **Port reads**: static sqlc queries become builder calls; anything sqlc
   couldn't express dynamically was hand-rolled — that code deletes outright.
   Declare `Projections` for your narrow reads (sqlc made you write these as
   separate queries; here they share the builder).
4. **Port relation loads**: wherever you regrouped joined rows in Go, declare
   a plan. `raorm lint` then budgets every load pattern in review.
5. **Keep the SQL worth keeping**: your analytical queries move into
   `raorm.SQL[T]` declarations verbatim — they gain build-time validation and
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

- sqlc queries are per-column nullable-aware from the schema; raorm's model
  must declare nullability (`*T`) correctly — `raorm import` gets this right,
  hand-written models should double-check.
- `:many` queries with `LIMIT $n` map directly; `OFFSET` exists but the doc
  comment will try to talk you into keyset, and it is right.
- Version-column optimistic locking is generated only if the model declares
  `.Version()` — sqlc had no equivalent, consider adopting it.
