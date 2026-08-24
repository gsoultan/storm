# raorm — production readiness (2026-08-24)

**Not ready to adopt.** Correctness is strong; coverage of the type system and
the absence of any adopter are what block it. Full checklist in `docs/PLAN.md`
§"Production readiness".

## The blocker nobody would guess — NOW CLOSED for numeric and jsonb
**`numeric` shipped 2026-08-24 as `raorm.Decimal`** (exact, two words, no
allocation, stdlib-only; 18 significant digits, with a GENERATION error past
that and a decode ERROR rather than a wrong number). **`jsonb` shipped** as
`runtime.JSON` — raw bytes the caller unmarshals, no value predicates.

**`text[]` and `uuid[]` shipped 2026-08-24** — nil vs `'{}'` kept distinct, a
NULL element is `ErrArrayNull` not `""`, no value predicates (equality on an
array is order-sensitive; `@>`/`&&` do not exist yet). Two bugs found before
commit: the scanner clobbered decode errors (later column overwrote the one
that mattered — scan now returns on FIRST failure), and the fuzzer found a
17-byte value whose count field made `make` allocate gigabytes — allocations
are now bounded by the input, never by a field inside it.

Still missing: `date`, `time`, `interval`,
`inet`, and remaining array element types (`int8[]`, `numeric[]`). The
generator binds bool, integer and float
widths, text/varchar, bytea, uuid, timestamptz, enums — and stops. The fixture
no longer has any column the generator omits.

No application handling money can model its own tables. This is the first thing
a real adopter hits, and it is invisible from the benchmarks because the bench
table has none of those types.

**The decision that was taken:** a raorm-defined fixed-point type, keeping the
stdlib-only rule. Pulling in a decimal package would put a third party's type on
every row of every financial table; a caller who wants shopspring/decimal
converts at the edge, which is one line and their choice.

**Do not offer float64 for numeric.** It cannot represent 0.10.

## What IS solid, and why to trust it
- **Injection is structural.** The property asserted is *the SQL is identical
  whatever the value*, not a payload blocklist. A rejected payload must fail as
  a DATA error (length, encoding) — a syntax error would mean it reached the
  statement.
- **Fuzzing earned its place immediately.** 20 seconds found a malformed token
  stream that silently dropped its `WHERE` clause — a filter failing **open**.
  Now fails closed via `Stmt.Err`, which every terminal returns. CI fuzzes on
  every PR, not nightly.
- Migrations converge (diff → apply → verify → diff empty, on a live database).
- Every round-trip claim is asserted with `CountingExecutor`.
- Coverage floors in CI (`scripts/check/coverage.sh`).

## The lesson from testing the CLI
It was at 0% and hid **three** bugs, two able to lose data:
1. Flags after a positional were silently ignored — `raorm diff init -schema
   mine` diffed `public` and proposed dropping its objects.
2. Introspection rendered expressions under the wrong `search_path`, so any
   enum outside `public` reported drift **forever**.
3. `raorm import` printed DDL instead of a model, so the adoption on-ramp
   produced something the adopter already had.

**That is the rate to expect from M6.** Untested surfaces hide data-loss bugs,
and the CLI is the surface every user touches first.

See [[core]], [[write_path]], [[plan_types]], [[seam_and_codegen]].
