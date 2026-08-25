---
tags: [raorm, stability, semver]
updated: 2026-08-25
status: adopted at v0.x — the commitments below bind from v1.0.0
---

# API stability and versioning

raorm is an *embedded* dependency: its API surface is not just the `raorm`
package but every line of code it generates into your tree. This policy covers
both, because a breaking change in generated output breaks you exactly as hard
as a renamed function.

## Semver, applied to a generator

From **v1.0.0**:

- **Patch** (`1.0.x`): bug fixes. Generated output may change byte-wise (a fix
  IS a changed emission) but not in API: everything that compiled keeps
  compiling with the same behavior, except behavior that was a bug.
- **Minor** (`1.x.0`): additive. New generated methods, new declaration
  options, new tooling flags. Regeneration required; nothing removed, nothing
  re-typed.
- **Major**: anything that makes existing model declarations, generated-code
  call sites, or emitted migrations invalid.

**Regenerate on every upgrade.** `raorm verify -stale` in CI makes forgetting
impossible; mixed-version generated trees are not a supported state.

## The stable surface

1. The **declaration API**: plain-struct models, `Schema`/`Plans`/
   `Projections` methods, field-pointer addressing, `raorm.Model`,
   `raorm.Decimal`, `raorm.Interval`, `raorm.OneOfN`, `raorm.SQL[T]`.
2. The **generated call surface**: `New`, `Where`/`Any`/`Not`, typed column
   handles, `Order`/`Limit`/`Offset`/`After`/`Unordered`, `All`/`AllInto`/
   `One`/`Count`/`Exists`, `Create`/`Mutate`/`Delete`, `InsertAll`, the
   `*Op` constructors, plan and projection types, `Having` composers.
3. The **Executor port** — four methods, budget five, changes are major.
4. **Emitted SQL semantics** (not bytes): statement *shapes* may improve in
   minors; what a query MEANS may not.
5. The **CLI verbs and their exit semantics**.

## Explicitly not covered

- The `codegen`, `compile/*` and `schema` packages: they are the compiler's
  internals, exported for the generated context. Pin raorm; do not build on
  them.
- Unexported anything, generated file layout, comment text, `internal/`.
- The composition seams (`FragOf`, `PredToks`, …): generated-context plumbing,
  documented as such at every declaration.

## Deprecation

Nothing is removed in minors. A deprecated surface keeps working for one full
minor cycle with a `Deprecated:` doc comment naming the replacement, then may
go in the next major.

## The measured claims

Performance numbers in the README are re-measured per release from
`bench/RESULTS.md` on stated hardware — never carried forward. A release that
regresses an allocation tripwire or a coverage floor does not ship.
