# M6 — first adopter: anubis/authz, PASSED 2026-08-25

Whole context in one day, not a slice. Kill criterion (>3 weeks or authorize
p95 regression) does not fire. Full record in `docs/PLAN.md` §M6-status; the
adopter-side history is anubis branch `authz-storm-m6` (slice c4aabb4,
completion 26f612c).

## Shape of the migration
- 44 sqlc queries → 34 `storm.SQL[T]` + 10 `storm.SQLExec` in ONE package
  (`internal/authz/adapter/postgres/rquery`, files mirror db/queries 1:1) +
  one builder query (`RoleByName`) over an `rmodel` projection of `roles`.
- sqlc fully removed from the context: db/queries/authz/, gen/, yaml section.
- `cmd/stormgen` (adopter-owned, ~107 lines) PREPAREs declarations against
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
Final state: pgx p95=214.875µs vs storm repository p95=203.958µs. Parity or
better all day. Numbers live in anubis commit 26f612c and the two test Logf
lines (`TestAuthorizeLatencyBudget`, `TestStormSlice_AuthorizeLatencyBudget`)
— never quote them from anywhere else.

## What adoption found in storm (all fixed + committed same day)
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
(`TestStormFull_*`, 5 families) asserts VALUES against direct SQL and runs
every write inside a deliberately rolled-back ambient transaction. Any future
adopter guide should prescribe exactly this.

## Follow-ups — closed same day (storm b050fe4, anubis 54d43e4)
- MIGRATING-FROM-SQLC.md now carries the real patterns (SQL-owns-semantics
  variant, live-DB PREPARE, acceptance doctrine, :exec/function-call rows);
  API.md §10 + README document SQLExec.
- anubis CI: rgen drift enforced — gen-drift.sh regenerates when
  ANUBIS_DB_URL is set (honest skip otherwise), and backend-suite.sh
  re-checks right after migrate on the scratch DB, which also proves every
  declaration PREPAREs against PURE migrations each run.
- M8 soak clock started 2026-08-25 → earliest v0.1.0 tag 2026-09-08
  (recorded in docs/PLAN.md §M8). The tag is the only M-work left; it waits
  on the calendar, not on code.

## v0.1.1: the adopter stopped being a special case (2026-08-26)

anubis's ~100-line hand-rolled `cmd/stormgen` is gone. It existed only because
v0.1.0's commands lived in `package main` and could not be imported, and the
cost was quiet but real: the context had codegen and NOTHING else — no
`verify -stale`, no `verify -pending`, no `lint`, no `explain`, the gates
storm is built around.

storm v0.1.1 makes the commands the importable package `storm/tool`, so
stormgen is now `tool.Main(rmodel.All(), rquery.Queries())`. The live-DB
PREPARE deviation recorded above is a supported flag now — **`-raw-schema
live`** — added for exactly this shape of adopter (model is a projection,
migrations are the truth). Generated output is byte-identical apart from the
version header, which is the proof the tool does what the hand-rolled
generator did.

Drift callers pass `-dsn "$ANUBIS_DB_URL"`: storm's tool reads `STORM_DSN` and
anubis does not use that name.

**The generalisable finding**: an adopter that has to re-implement your tool
is telling you the tool is unreachable, not that their case is exotic. It took
being a stranger (P4) to see it, because from inside the repo the hand-rolled
generator looked like a reasonable adopter choice.
