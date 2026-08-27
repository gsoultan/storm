# storm — engineering conventions & developer profile roster

An embeddable ORM library for Go. Imported, not deployed. Compiled, not
interpreted. Module `github.com/gsoultan/storm` · Go 1.26.

**Postgres first**, then MySQL/MariaDB, SQL Server, Oracle, MongoDB — the
dialect is a compile-time parameter, never a runtime branch ([[docs/DIALECTS]]).
Model-first: one Go schema, generated API, generated migrations storm never
applies.

Read [[docs/CONCEPT]] before changing anything. Read [[docs/COMPARISON]] before
proposing a feature — most feature ideas are already in the *rejected* list with
a reason attached.

## Architecture (mandatory)

Compiler shape: **front end → IR → back end**, plus a thin runtime.
`schema/` · `query/` · `compile/` · `codegen/` · `runtime/` · `cmd/storm/`.

Hard rules, all CI-enforced (`scripts/check/*.sh`):

- **`schema/`, `query/`, `compile/`, `codegen/`, `runtime/` import stdlib only.**
  Every driver dependency lives in exactly one adapter package (`pgx/v5` in
  `runtime/pgxdrv`) and nowhere else. No driver type crosses that boundary.
- **No `reflect` under `runtime/`.** No exceptions, no fallback path.
- **No dialect conditional outside `compile/`.**
- **≤ 10 Go files per folder** — outgrowing it means a missing concept.
- **≤ 15 methods per interface**; the `Executor` port is five and stays five.
- **One interface per file, one struct per file.**
- **Folders name the layer; package clauses are prefixed and unique**
  (`schema/pg` → `package schemapg`). No import aliases at call sites.
- **Generated output is byte-deterministic** across runs and machines.
- **storm emits migrations but never applies DDL** (ADR-0001). No runtime code
  path may alter a schema.
- **The IR is a logical plan, not a SQL AST** (ADR-0004) — it is what keeps the
  SQL back ends from ossifying around Postgres.

## Developer profile roster

Adopt the **Driver** profile that owns the code you touch, then re-read your own
diff as the **Challenger** whose budget it most likely breaks. Name both in the
task summary (`Driver: compiler · Challenger: perf`).

| Profile | Owns | Vetoes | Proof |
| :--- | :--- | :--- | :--- |
| **compiler** | `query/`, `compile/`, `codegen/`, shape enumeration, fragment lowering | an identifier reaching SQL text from a runtime value; a builder node that allocates per call; non-deterministic generated output; a lowering rule without a golden test | golden-file suite; `storm verify` clean; fuzz corpus green |
| **perf** | `runtime/`, scanners, shape cache, pooling | `reflect` in any runtime path; `any` boxing per column; a map lookup per query on the warm path; a benchmark whose capacity differs between sides; quoting a number not re-measured this run | `benchstat` delta vs `bench/RESULTS.md`; `testing.AllocsPerRun` assertions; targets in [[docs/PERFORMANCE]] |
| **dba** | `model/`, `schema/`, `migrate/`, introspection, EXPLAIN gates, `bench/` fixtures | storm *applying* DDL; a destructive migration step without `--allow-destructive`; a query added to a hot path without `EXPLAIN (ANALYZE, BUFFERS)`; an N+1 shipped; a relation load with unbounded round trips | `storm explain` in CI; round-trip counting decorator; model → DDL → introspect round-trip diff empty; `verify --pending` green |
| **dx** | public API, generated-code readability, CLI, errors | an error that does not name the query and the shape mask; an API needing a comment to be understood; generated code a human cannot review; a breaking API change without a version | migration guide runs clean; example suite compiles; adopter feedback from M6 |
| **sec** | injection surface, identifier handling, arg binding | any identifier interpolated from a runtime value; a placeholder count not statically known; bound args logged above debug level; a raw fragment that skips build-time validation | injection corpus green; placeholder arity proven at generation time; fuzz over identifiers |
| **arch** | package boundaries, `Executor` port, scope line | a core package importing non-stdlib; pgx leaking out of `runtime/pgxdrv`; a feature outside the scope line in [[docs/CONCEPT]]; **any dialect branch on the hot path**; a capability sniffed at runtime instead of negotiated at build time | `scripts/check/import-boundary.sh`; scope line re-read in review |
| **test** | suites, fixtures, corpora, CI gates | a bug fix without a failing-first test; a relation without a round-trip-count assertion; a skipped suite reported as done; an allocation target without an `AllocsPerRun` assertion | `go test -race -shuffle=on` green; `testdata/compilefail` suite; coverage: `compile/` 100%, `runtime/` ≥ 95% |

## Standing truths (all profiles)

A fast path that skips a check is a vulnerability. A check that allocates per
request is a regression. A cache that ignores who asked is a data leak. Any map
keyed by attacker-supplied input needs a bound and an eviction. Every bug fix
ships a test that fails before and passes after, with the root cause named in
one sentence. **Never quote a performance number from memory — re-run the
bench.**
