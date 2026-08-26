---
tags: [raorm, releases]
updated: 2026-08-26
---

# Changelog

Versions follow [semver](https://semver.org). Until v1.0 the *generated* API
may change with a minor bump; what is promised, and for how long, is
[docs/STABILITY.md](docs/STABILITY.md).

Every entry names what changed and — where it matters — what it cost, because
a release note that cannot be checked is marketing.

## Unreleased

### Fixed — the first-run path, none of which worked outside this repository

Found by doing the thing no gate had done: taking a fresh module outside both
raorm and its adopter, running `go get`, and following the README. Detail and
the lesson in [docs/PRODUCTION-READINESS.md](docs/PRODUCTION-READINESS.md) P4.

- **`generate` emitted raorm's own module path into the caller's module**, so
  a user's generated context package imported
  `github.com/gsoultan/raorm/internal/store/...` and could not compile. The
  import path now comes from the host module's `go.mod`. This is the flagship
  command, and it had never worked for anyone but this repository — where the
  wrong answer happens to be the right one.
- **The tool was unreachable.** `Models` lived in `package main`, which cannot
  be imported, and the "generated bootstrap" its documentation referred to did
  not exist. Adopters could hand-roll `codegen.Package` (anubis did) but had
  no way to get `verify -pending`, `lint` or `explain` at all. The commands are
  now the importable package **`raorm/tool`**; your whole tool is:

  ```go
  func main() { tool.Main(model.All(), nil) }
  ```

  `cmd/raorm` remains as a stub whose only job is to fail with that
  instruction.
- **An output directory reached through a symlink was refused** ("outside this
  module"). On macOS `/tmp` is a symlink, so this hit anyone using a temporary
  directory. The deepest existing ancestor is now resolved on both sides.

### Added

- **`raorm.SQLExec`** — the no-rows half of the escape hatch, for the `:exec`
  statements that are about a third of a real query file. Same placeholder
  precheck as `SQL[T]`, and generation *requires* a zero-column result, so an
  exec that silently returned rows is a build failure pointing at `SQL[T]`.
- **Fast `int8[]` and `text[]` parameter codecs**, joining `uuid[]`. Measured
  at 500 elements: `int8[]` 466 allocations → 1 (21× faster), `text[]` 503 → 1
  (8×). Schemas with bigserial keys bind `int8[]` on the same `= ANY($1)`
  relation-load path raorm's uuid-keyed fixtures hid.
- **`runtime.ShapeCap`** (default 1024) bounds the compiled-statement cache,
  with `SetShapeCap(0)` to opt out and a generated `ShapeFlushes()` gauge per
  package. 100k distinct shapes retain 170 KB instead of 27.8 MB.
- **Wire-format guard** in `runtime/pgxdrv`: pools in
  `QueryExecModeSimpleProtocol`/`Exec` are refused at construction, and every
  result is checked once per statement. Under those modes `false` arrives as
  the byte `'f'` and decoded as **true** — a silent inversion. Costs 3.82 ns
  per statement, zero allocations. See [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)
  for the PgBouncer table.
- **Generated headers carry the raorm version**, so "which raorm wrote this?"
  is answerable from the file.
- [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) and
  [docs/PRODUCTION-READINESS.md](docs/PRODUCTION-READINESS.md).

### Changed

- `runtime.TextArray` and friends name a text-format array
  (`ErrArrayTextFormat`) instead of misreporting it as multi-dimensional. An
  `enum[]` arrives as text because pgx has no binary codec for one; the error
  now says to declare the column `text[]` or cast it.

## v0.1.0 — 2026-08-26

First tagged release. Postgres only; the dialect seam exists and is
CI-enforced but has one implementation, which makes it a hypothesis rather
than a fact (see [docs/DIALECTS.md](docs/DIALECTS.md)).

- Model-first schema with generated, never-applied migrations; `diff` /
  `verify` / `verify -stale` / `verify -pending`.
- Compile-time query building: a dynamic query's shapes each compile once and
  a warm call allocates nothing to build its SQL.
- Relations as **named plans** with asserted round-trip counts; reading an
  unloaded relation does not compile.
- Writes: masked inserts and updates, optimistic locking, `COPY` bulk load,
  FK-ordered atomic unit-of-work.
- `raorm.SQL[T]`: any statement PostgreSQL can run, typed, with a generated
  scanner validated at build time.
- Tooling gates: `lint` (round-trip budgets), `explain` (plans every
  statement), coverage floors, fuzzing, and an injection property that is
  structural rather than a filter.
- **First adopter shipped**: anubis's authz context — 44 queries, sqlc
  removed, authorize p95 unchanged.
