# Stability gates — what holds the v1 promise, and the traps in them

Added 2026-09-25. Two things prompted it: `apidiff` v1.1.0→main found
`runtime.Rows.Values: added` sitting on main unnoticed (bac90a1), and AGENTS.md
turned out to call six unenforced rules "CI-enforced".

## The gates

- **`scripts/check/apicompat.sh`** — a pinned apidiff of the packages
  docs/STABILITY.md covers (`storm`, `runtime`, `runtime/{pgxdrv,mydrv,msdrv}`,
  `migrate`) against the latest `v1.*` tag. Fails on any incompatible change.
  Exceptions go in an inline array in the script: empty by default, and each
  one gets defended in review. In `apidiff -m` output a root-package line has
  NO path (`- SQLExec: removed`); a subpackage line reads `- ./runtime.X`.
- **`internal/archcheck`**, run by boundaries.sh, holds the declaration rules:
  - one struct and one interface per file
  - ≤ 10 files per folder, ≤ 15 methods per interface, Executor ≤ 5
  - unique package clauses, no aliases of storm's own packages

  Where the tree had drifted, the rule is a RATCHET: the `maxFiles` map
  (8 folders), `maxExtraStructs` 134, `maxExtraInterfaces` 7. It fails on
  growth AND on shrink, and a shrink means lowering the number. Out of scope:
  bench/, examples/, internal/, testdata and nested modules.
- **boundaries.sh**:
  - The stdlib-only list is DERIVED from
    `go list ./schema/... ./compile/... ./codegen/... ./runtime/...`, exempting
    only runtime/pgxdrv and schema/pg. The hand-kept list had left off
    compile/oracle and compile/oraddl.
  - runtime/ reaches no storm package outside runtime/, which is what makes
    "no dialect branch at run time" structural.
  - The core reaches nothing in tool/ or cmd/.
- **oracle-spike** runs on every push to main and every PR, with its image
  pinned by digest (23.26.3, 26ai Free). It used to be path-filtered, and
  codegen/ was not on the list.
- **CI shape, from 2026-09-26.** vet, boundaries and apicompat run in their own
  `gates` job. Every named step in ci and oracle-spike runs
  `if: ${{ !cancelled() }}`, so one red step no longer hides the others; a red
  boundaries step once did, and main ran no live test for six commits. Branch
  protection on main requires gates, test, postgres-next, sqlserver and spike
  once the user applies `/tmp/stormapi/protection.json`. The classifier treats
  applying it as a permission grant, so it is the user's to do.
- **Release smoke test.** After tagging, generate from the PUBLISHED module, in
  a fresh module with no replace, for all five dialects, then build and vet.
  v1.2.0's run found storm prescribing `go get …/storm/tool` for a missing
  go-ora driver; `missingToolDep` now names the package go reports.

## Traps found building them

- **`git fetch --depth=1` in a FULL clone** writes `.git/shallow` and grafts
  history off at the fetched tags: `git log main` stopped at v1.1.0. The repair
  works when every parent is present: delete `.git/shallow`, then run
  `git fsck --connectivity-only`. The gate now fetches only when no v1 tag
  exists, and passes --depth only to a clone that is already shallow.
- **Ratchet baselines must be MEASURED.** Guessed 101 and 8; the real numbers
  were 134 and 7. Same lesson as the coverage floors.
- **A red early step skips everything after it.** When boundaries fails in
  ci.yml, every later step of the `test` job is skipped: live tests, MySQL and
  MariaDB, coverage, fuzz, govulncheck. So a red main is not "one failure"; it
  is no CI at all past that step.
- **A hung test may be a spin, not a deadlock.** In this sandbox a nil-interface
  method call in a test can SPIN instead of panicking, as pgxdrv's zero
  `rows{}.Close()` did. A test that hangs until the 10m timeout may be exactly
  that.

## The value shape

`runtime.ValueRows` = Rows + Values. Generated value-family reads wrap Query in
`runtime.AsValueRows` (`decoders.require`, `rowsFrom`, `batchRows`). Byte
adapters must NOT implement Values. See [[m11_oracle]].
