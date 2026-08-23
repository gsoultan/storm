# raorm — named plan types: the P2 spike result (2026-08-24)

**PASSED. Plan B (runtime-checked plans) is not needed.** M3's only surviving
risk was whether named plan types are usable at all; three days answered it
instead of four weeks. `internal/planspike/` — two genuinely *generated* table
packages with a **hand-written** plan layer, the same split as the M0 spike.

## Proven
- 50 parents + 25,000 children in **exactly 2 round trips**, asserted at parent
  counts **1, 7 and 50** — a loader that batches per parent passes at n=1, so
  one count proves nothing.
- Empty parent set costs **1** round trip, not 2: `= ANY('{}')` is a
  guaranteed-empty query and not issuing it is the difference between "2 when
  there is work" and "2 always".
- Reading an unloaded relation **does not compile**:
  `rows[0].Users undefined (type org.Row has no field or method Users)`
  `testdata/compilefail/` asserts it, each case carrying a `// want:` line.

## Four findings
1. **`q.Load(plan)` as [[docs/API]] §7 wrote it CANNOT BE BUILT.** Go methods may
   not have type parameters, so a method taking a plan *value* cannot vary its
   return type by plan. The plan must be the **entry point**
   (`store.OrgsWithUsers().Where(…)`) or a generated method per plan. Doc
   corrected; the design survives.
2. **The plan layer cannot live in a table package.** A plan names two tables,
   and a table package importing a sibling reintroduces the cycle
   one-package-per-table exists to avoid (`Org` has `Users`, `User` has an
   `Org`). Plans go in the **parent** package. This is the structural reason
   `plans.go` lives where the docs already put it.
3. **Every builder method must be redeclared on the plan type.** Go has no
   delegation, and embedding `org.Query` makes `Where()` return `org.Query`,
   dropping out of the plan. Boilerplate — but a generator writes it.
4. **The generated default `limit: 1000` silently truncates a relation load.**
   Sensible on a single-table read; on a child fetch it drops rows and every
   count computed from the result is wrong. The spike's own first run failed
   this way at 50×500 — evidence the number cannot be picked by feel.
   **Settled: reaching the child limit is an ERROR, never a partial result.**
   **Open for M3:** what the limit should be — derived from the parent limit
   times a declared per-parent bound, or required from the plan.

## Also settled
**M3 is not blocked on joins.** `docs/PLAN.md` said it was; the `= ANY` batch
already delivers the N+1 guarantee without a single join. That claim is struck.

See [[core]], [[seam_and_codegen]].

## Generated, not hand-written (2026-08-24)
The spike is deleted; its tests now run against **generated** code and still
pass, compile-fail cases included. That is what writing it by hand first bought.

**One plan type per RELATION, not per combination.** Finite by construction — n
relations give n plan types, never 2ⁿ (R3's explosion) — and it needs no
declaration mechanism, so it landed without the `plans.go` front end. Named
multi-relation and nested plans still need that; this is the tier below.

Relation metadata now reaches the IR (`schema.Table.Relations`). It used to be
discovered during the model walk and discarded: a foreign key says
`users.org_id → orgs.id`, but not that the field is `Org`, that `Org` has a
`Users` slice pointing back, or which side owns the key.

**The distinction that cost a compile round:** `Relation.Nullable` describes the
*owning side's Go field*; a loader needs whether the *foreign-key column* is
nullable. A self-referencing hierarchy is exactly where they disagree —
`Org.Children` is a plain slice, but `orgs.parent_id` must be nullable or the
root has nowhere to point. `relPlan.KeyNullable` is the column's.

All four kinds at exactly 2 round trips: has-many; belongs-to (keys
de-duplicated, so 1,000 users on 50 orgs fetch 50 orgs); self-referential
has-many; self-referential to-one with every key NULL — **1** round trip, not 2.

**`= ANY` parameter cost, re-measured:** ~2 allocations per bound id, on pgx's
parameter side. **`pgtype.FlatArray` does not help** — within noise of a plain
slice, because both pay per-element cost inside pgx's generic array codec. The
fix is a `pgtype.Codec` in `runtime/pgxdrv`. Numbers in `bench/RESULTS.md`.

See [[write_path]], [[seam_and_codegen]].

## Relation loading, second pass (2026-08-24)

**The delegation list is the bug surface.** Go has no delegation, so a plan
redeclares every builder method of the parent `Query`. `Order`, `Offset` and
`After` were added to `Query` later and were missing from every plan — a plan
could filter its parents but not page them. **Anything added to `Query` has to
be added there too.**

**`ChildLimit` is a global guard, not a page size.** Fifty parents with
`ChildLimit(100)` is an error, not a hundred each. Pinned by a test so nobody
"fixes" it into a silent per-parent truncation.

**`ChildTop(n)` is the per-parent limit** — greatest-n-per-group, still 2 round
trips. Requires an ordering, and it must be a strict total order.

**The lowering default was reversed by measurement.** I chose `row_number()` by
argument: one index scan feeding a window beats a per-parent nested loop. Wrong.
The window form reads **every child of every matched parent** and discards past
n, so its cost tracks the *total child count*; `LATERAL`'s tracks the *rows
returned*. 3.6× at one parent, 13× at ten, **32.8× at a hundred**. Both stay
generated and exported — the gap narrows as n grows, and a default chosen by
measurement needs something to have been measured against.

**`= ANY` is fixed** — `pgxdrv.RegisterFastArrays`, 1,003 allocations → 1
isolated, 1,021 → 11 end to end at 500 ids, **flat in id count**. Only `uuid[]`;
`text[]` and `int8[]` still need it. `FlatArray` was the wrong fix and the
negative result is why this was worth building.

**Recursive traversal ships** with a mandatory depth bound and a path-array
cycle guard. The guard **excludes** the repeating row rather than emitting it
once — so a cycle puts no duplicate in the result set, which matters for a
caller building a map by id.

See [[write_path]], [[seam_and_codegen]].