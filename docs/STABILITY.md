---
tags: [storm, stability, semver]
updated: 2026-09-04
status: adopted at v0.x — the commitments below bind from v1.0.0
---

# API stability and versioning

storm is an *embedded* dependency: its API surface is not just the `storm`
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

**Regenerate on every upgrade.** `storm verify -stale` in CI makes forgetting
impossible; mixed-version generated trees are not a supported state.

## The stable surface

1. The **declaration API**: plain-struct models and field-pointer addressing;
   the `Schema`, `Plans`, `Projections`, `Aggregates` and `Joins` methods;
   the package-level `storm.Union` declaration; `storm.Model`, `storm.Decimal`,
   `storm.Interval`, `storm.TstzRange`, `storm.TSVector`, `storm.TimeOfDay`,
   `storm.UUID`, `storm.OneOfN`, `storm.AnyRef`, `storm.SQL[T]` and
   `storm.SQLExec`.

   Within `Aggregates`: `By`/`ByExpr`, `Count`/`CountOf`/`CountDistinct`,
   `Sum`/`Avg`/`Min`/`Max`, `SumOver`/`AvgOver`/`MinOver`/`MaxOver`, `Compute`,
   `Filter`, `Having`, `GroupingOf`, `Rollup`/`Cube`/`Sets`, the window
   functions (`RowNumber`, `Rank`, `DenseRank`, `PercentRank`, `CumeDist`,
   `Lag`, `Lead`, `FirstValue`, `LastValue`), `Over` with `PartitionBy`,
   `OrderByAsc`/`OrderByDesc` and the `Rows`/`Range` frames, and the expression
   vocabulary on `Exprs` (`DateTrunc`, `Coalesce`, `NullIf`, `Abs`, `Lower`,
   `Upper`, `Add`/`Sub`/`Mul`/`Div`/`DivScale`, the comparisons and
   `And`/`Or`/`Not`).

   Within `Joins`: `Inner`/`Left`, `With`/`InnerWith`/`LeftWith`, `Take`,
   `TakeFrom`, `Where`, `OrderAsc`/`OrderDesc`, `OnCols`.

   Within `Union`: `From`, `Take`, `Const`, `Where`, `Param`, `OrderAsc`/
   `OrderDesc`, `Distinct`.
2. The **generated call surface**: `New`, `Where`/`WhereIf`/`Any`/`Not`/
   `NotAny`, typed column handles and their per-column shorthands,
   `Order`/`Limit`/`Offset`/`After`/`Unordered`, `All`/`AllInto`/`One`/
   `Count`/`Exists`, `Create`/`Mutate`/`Delete`, `InsertAll`, the `*Op`
   constructors, plan types with `ChildLimit`/`ChildTop`/`ChildOrder`,
   projection readers (`All<Name>`, `One<Name>`), aggregation readers
   (`All<Name>`, `All<Name>Into`), join readers (`All<Name>`), the semi- and
   anti-join composers (`<Parent>Having<Rel>`, `<Parent>NotHaving<Rel>`,
   `AndHaving`, `AndNotHaving`), the union readers (`<Name>`, `<Name>Into`),
   and the self-reference traversals `Descend`/`Ascend`.
3. The **Executor port** — four methods, budget five, changes are major.
4. **Emitted SQL semantics** (not bytes): statement *shapes* may improve in
   minors; what a query MEANS may not.
5. The **CLI verbs and their exit semantics**.

## Explicitly not covered

- The `codegen`, `compile/*` and `schema` packages: they are the compiler's
  internals, exported for the generated context. Pin storm; do not build on
  them.
- Unexported anything, generated file layout, comment text, `internal/`.
- The composition seams (`FragOf`, `PredToks`, …): generated-context plumbing,
  documented as such at every declaration.

## Which server versions

A version storm has never run against is a version storm does not support, so
this list is what CI actually starts:

| | Tested | Why these |
|---|---|---|
| PostgreSQL | **16 and 18** | 16 is the floor `storm explain` needs — it reads the planner's JSON — and 18 is what adopters run. Both ends, because a catalog change between majors is invisible until introspection meets it |
| MySQL | **8.4** | The LTS, and the version every measurement in these docs was taken against. Pinned rather than `mysql:8`, which floats and would quietly change what the gate means |
| MariaDB | **11.4** | The LTS |
| pgx | the version in `go.mod` | Minimal version selection resolves an adopter's build to the HIGHEST requirement in the graph, so a storm that requires an older pgx is a storm whose driver adapter runs a pgx its own suite never exercised. That already happened once: storm said 5.10 while its adopter built 5.11 |

Between the two PostgreSQL versions, the second one is not a formality.
Introspection reads `pg_catalog` directly, and the catalog is the part of
PostgreSQL that changes between majors.

Individual features need more than the floor and say so where they are
declared: `NullsNotDistinct` is 15+, `storm.UUIDv7()` is 18+.

## Deprecation

Nothing is removed in minors. A deprecated surface keeps working for one full
minor cycle with a `Deprecated:` doc comment naming the replacement, then may
go in the next major.

## The measured claims

Performance numbers in the README are re-measured per release from
`bench/RESULTS.md` on stated hardware — never carried forward. A release that
regresses an allocation tripwire or a coverage floor does not ship.
