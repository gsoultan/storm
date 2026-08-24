# raorm — production readiness (2026-08-24)

**Not ready to adopt.** Correctness is strong; coverage of the type system and
the absence of any adopter are what block it. Full checklist in `docs/PLAN.md`
§"Production readiness".

## The blocker nobody would guess
**`numeric` is unsupported.** So are `date`, `time`, `interval`, `jsonb`,
`inet`, and **arrays of anything**. The generator binds bool, integer and float
widths, text/varchar, bytea, uuid, timestamptz, enums — and stops. The fixture
itself has two columns (`prefs jsonb`, `scopes text[]`) the generator silently
omits.

No application handling money can model its own tables. This is the first thing
a real adopter hits, and it is invisible from the benchmarks because the bench
table has none of those types.

**`numeric` needs a DECISION before code:** Go has no stdlib decimal and
`runtime/` is stdlib-only, so the representation is a genuine choice — lossless
`string`, a raorm fixed-point type, or relaxing the stdlib rule for one type.
Do not pick silently.

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
