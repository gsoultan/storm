---
tags: [storm, releases]
updated: 2026-08-26
---

# Changelog

Versions follow [semver](https://semver.org). Until v1.0 the *generated* API
may change with a minor bump; what is promised, and for how long, is
[docs/STABILITY.md](docs/STABILITY.md).

Every entry names what changed and — where it matters — what it cost, because
a release note that cannot be checked is marketing.

## v0.2.0 — 2026-08-27

### Renamed: raorm is now **storm**

The module path changed, which for Go is a breaking change even though not a
line of API moved:

    github.com/gsoultan/raorm  →  github.com/gsoultan/storm

For a consumer the migration is two mechanical steps and nothing else:

```console
go mod edit -droprequire github.com/gsoultan/raorm
go get github.com/gsoultan/storm@v0.2.0
# then: sed -i '' 's|gsoultan/raorm|gsoultan/storm|g' on your imports
```

Everything else is a straight substitution — the package qualifier
(`raorm.SQL[T]` → `storm.SQL[T]`), the error prefixes, and the DSN environment
variable **`RAORM_DSN` → `STORM_DSN`**. Generated code carries the tool name
in its header, so **regenerate after upgrading**; `verify -stale` will tell
you if you forget.

The old path is not withdrawn and cannot be: `github.com/gsoultan/raorm`
v0.1.0 and v0.1.1 stay resolvable through the module proxy forever. They
simply stop receiving changes.

**One trap worth knowing about.** GitHub redirects the old repository name to
the new one, so `go list -m -versions github.com/gsoultan/storm` reports
`v0.1.0 v0.1.1 v0.2.0` — but the first two **cannot be used under this path**.
Their `go.mod` declares the old module path, and Go refuses the mismatch:

```
module declares its path as: github.com/gsoultan/raorm
        but was required as: github.com/gsoultan/storm
```

That is correct behaviour and not fixable from here — a tag's go.mod is
immutable. **Under `storm`, v0.2.0 is the first usable version.** If you want
a v0.1.x, require it under the old path, where it works exactly as it always
did.

### Added

- **`numeric[]`** decodes to `[]storm.Decimal` and encodes back, which closes
  type coverage: every type the model DSL can declare now round-trips.
  Element decoding is fallible — a numeric past the 18 significant digits a
  Decimal holds is an error rather than a wrong number — so `runtime.ArrayErr`
  joins `Array` as the single implementation of the bounds arithmetic a fuzzer
  once found a hole in.
- **`storm.TimeOfDay`** — PostgreSQL `time` (without time zone), microseconds
  since midnight. Its own type rather than a `time.Time`, for the reason
  `Interval` is not a `Duration`: an instant is a point on a calendar in a
  zone, a SQL `time` is none of those, and decoding one into the other forces
  a date to be invented. It gets `Eq`/`Gt`/`Gte`/`Lt`/`Lte` and ordering,
  which an interval cannot have, because 09:00 really is before 17:00.
  24:00:00 is accepted, as PostgreSQL accepts it.

### Fixed

- Adding a type surfaced three latent codegen faults, each of which would
  have hit the next type added: an arena missing from the capacity map read
  as capacity zero and failed the FIRST predicate as "too complex" (now a
  generation error naming the arena); two cursor-name maps produced
  `b.tods[] = q.tods[]`, caught by the parser; and the column handle wrote
  its value to the shared `int64` field while the arena read a different one
  — which compiled, bound zero, and matched every row.

## v0.1.1 — 2026-08-26

The release that makes the tool usable by someone who is not its author.
v0.1.0's `generate` could not produce compiling code in any module but this
one, and the rest of the commands were unreachable, so **v0.1.0 should be
skipped**.

### Fixed — the first-run path, none of which worked outside this repository

Found by doing the thing no gate had done: taking a fresh module outside both
storm and its adopter, running `go get`, and following the README. Detail and
the lesson in [docs/PRODUCTION-READINESS.md](docs/PRODUCTION-READINESS.md) P4.

- **`generate` emitted storm's own module path into the caller's module**, so
  a user's generated context package imported
  `github.com/gsoultan/storm/internal/store/...` and could not compile. The
  import path now comes from the host module's `go.mod`. This is the flagship
  command, and it had never worked for anyone but this repository — where the
  wrong answer happens to be the right one.
- **The tool was unreachable.** `Models` lived in `package main`, which cannot
  be imported, and the "generated bootstrap" its documentation referred to did
  not exist. Adopters could hand-roll `codegen.Package` (anubis did) but had
  no way to get `verify -pending`, `lint` or `explain` at all. The commands are
  now the importable package **`storm/tool`**; your whole tool is:

  ```go
  func main() { tool.Main(model.All(), nil) }
  ```

  `cmd/storm` remains as a stub whose only job is to fail with that
  instruction.
- **An output directory reached through a symlink was refused** ("outside this
  module"). On macOS `/tmp` is a symlink, so this hit anyone using a temporary
  directory. The deepest existing ancestor is now resolved on both sides.

### Added

- **`-raw-schema live`** — validate `storm.SQL[T]` declarations against the
  connected database instead of a scratch apply of the model. Required by any
  adopter whose model is a *projection* of a schema owned by migrations, whose
  queries therefore call functions and read tables the model does not
  describe. The first adopter could not use the tool without it. The default
  is unchanged, and the cost is stated: the model no longer vouches for those
  statements, so point it at a database built from migrations.
- **`storm.SQLExec`** — the no-rows half of the escape hatch, for the `:exec`
  statements that are about a third of a real query file. Same placeholder
  precheck as `SQL[T]`, and generation *requires* a zero-column result, so an
  exec that silently returned rows is a build failure pointing at `SQL[T]`.
- **Fast `int8[]` and `text[]` parameter codecs**, joining `uuid[]`. Measured
  at 500 elements: `int8[]` 466 allocations → 1 (21× faster), `text[]` 503 → 1
  (8×). Schemas with bigserial keys bind `int8[]` on the same `= ANY($1)`
  relation-load path storm's uuid-keyed fixtures hid.
- **`runtime.ShapeCap`** (default 1024) bounds the compiled-statement cache,
  with `SetShapeCap(0)` to opt out and a generated `ShapeFlushes()` gauge per
  package. 100k distinct shapes retain 170 KB instead of 27.8 MB.
- **Wire-format guard** in `runtime/pgxdrv`: pools in
  `QueryExecModeSimpleProtocol`/`Exec` are refused at construction, and every
  result is checked once per statement. Under those modes `false` arrives as
  the byte `'f'` and decoded as **true** — a silent inversion. Costs 3.82 ns
  per statement, zero allocations. See [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)
  for the PgBouncer table.
- **Generated headers carry the storm version**, so "which storm wrote this?"
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
- `storm.SQL[T]`: any statement PostgreSQL can run, typed, with a generated
  scanner validated at build time.
- Tooling gates: `lint` (round-trip budgets), `explain` (plans every
  statement), coverage floors, fuzzing, and an injection property that is
  structural rather than a filter.
- **First adopter shipped**: anubis's authz context — 44 queries, sqlc
  removed, authorize p95 unchanged.
