# M6 — first adopter: anubis/authz, PASSED 2026-08-25

Whole context in one day, not a slice. Kill criterion (>3 weeks or authorize
p95 regression) does not fire. Full record in `docs/PLAN.md` §M6-status; the
adopter-side history is anubis branch `authz-raorm-m6` (slice c4aabb4,
completion 26f612c).

## Shape of the migration
- 44 sqlc queries → 34 `raorm.SQL[T]` + 10 `raorm.SQLExec` in ONE package
  (`internal/authz/adapter/postgres/rquery`, files mirror db/queries 1:1) +
  one builder query (`RoleByName`) over an `rmodel` projection of `roles`.
- sqlc fully removed from the context: db/queries/authz/, gen/, yaml section.
- `cmd/raormgen` (adopter-owned, ~107 lines) PREPAREs declarations against
  the LIVE dev DB — deviation from model-scratch doctrine because
  `authorize()` etc. live in migrations, which stay schema-of-record; the
  model is a projection. This is the pattern to expect from function-heavy
  adopters.
- Executor adapter is 74 lines: anubis's `database.Querier` type-switched to
  `pgx.Tx`/`*pgxpool.Pool` and wrapped in pgxdrv — ambient transactions work
  unchanged (proved by rolled-back-tx tests).
- Domain string-id contract preserved via `id::text` in SELECTs and one
  `parseUUID`/`uuidStr` crossing for the builder.

## p95 gate (same-run pairs, n=500, anubis integration suite)
Final state: pgx p95=214.875µs vs raorm repository p95=203.958µs. Parity or
better all day. Numbers live in anubis commit 26f612c and the two test Logf
lines (`TestAuthorizeLatencyBudget`, `TestRaormSlice_AuthorizeLatencyBudget`)
— never quote them from anywhere else.

## What adoption found in raorm (all fixed + committed same day)
1. c25cce3 — raw-scanner qualifier used the DIRECTORY name; anubis's package
   `authzrquery` in dir `rquery/` produced non-compiling output. Now uses
   reflect's declared name + aliases the import when it differs.
2. c5ce1ed — `Null[T]` fields never matched: reflect names generic
   instantiations `Null[string]`, the check compared `== "Null"`. Prefix
   match; fixture `internal/aliasrow` (package `aliasrowx`) covers both this
   and #1.
3. c5ce1ed — two queries sharing a row type emitted duplicate scanners
   (compile error). Deduped in `codegen.Package` AFTER verifying positional
   agreement — disagreement errors, because scanners decode by position.
4. a2467db — `SQLExec` added: real query files are ~1/3 `:exec`. Same
   placeholder precheck; generation REQUIRES zero result columns (a void
   function call is wrapped `(fn($1) IS NULL) AS done` as SQL[T] instead).

## Acceptance-test doctrine worth reusing
PREPARE proves SQL/row shape, NOT call-site argument order. The adopter suite
(`TestRaormFull_*`, 5 families) asserts VALUES against direct SQL and runs
every write inside a deliberately rolled-back ambient transaction. Any future
adopter guide should prescribe exactly this.

## Open follow-ups
- anubis ADR-0009 §5 written; its no-sql-in-go check now catches backtick
  SQL (line-start keywords) — raorm's docs/MIGRATING-FROM-SQLC.md could
  absorb both patterns.
- M8 tag + two-week soak was gated on M6: gate is now open.
