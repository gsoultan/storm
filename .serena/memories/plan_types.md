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
