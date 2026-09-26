# storm — engineering conventions & developer profile roster

An embeddable ORM library for Go. Imported, not deployed. Compiled, not
interpreted. Module `github.com/gsoultan/storm` · Go 1.27.

**Postgres first**, then MySQL/MariaDB, SQL Server, Oracle, MongoDB — the
dialect is a compile-time parameter, never a runtime branch ([[docs/DIALECTS]]).
Model-first: one Go schema, generated API, generated migrations storm never
applies.

Read [[docs/CONCEPT]] before changing anything. Read [[docs/COMPARISON]] before
proposing a feature — most feature ideas are already in the *rejected* list with
a reason attached.

## Architecture (mandatory)

Compiler shape: **front end → IR → back end**, plus a thin runtime.
`schema/` (the model, and the IR) · `compile/` · `codegen/` · `runtime/` ·
`migrate/` · `tool/` · `cmd/storm/`.

The tool has a source-analysis front end as well: `tool/discover` parses the
adopter's module to find their models and `tool/bootstrap` synthesizes the
`main` that registers them (ADR-0006). It is stdlib-only, answers exactly one
static question, and **nothing under `schema/`, `compile/`, `codegen/` or
`runtime/` may import it**.

Hard rules. Each one names what holds it, because this list once said "all
CI-enforced" while six of them were not, and the tree drifted from four.
`scripts/check/boundaries.sh` runs on every push, and `internal/archcheck` is
the part of it that reads declarations rather than text.

- **`schema/`, `compile/`, `codegen/`, `runtime/` import stdlib only.**
  Every driver dependency lives in exactly one adapter package (`pgx/v5` in
  `runtime/pgxdrv`, which `schema/pg`'s introspection also reads through) and
  nowhere else. No driver type crosses that boundary. *boundaries.sh; the
  package list is derived from the tree, so a new back end cannot be left off
  it — which is how `compile/oracle` was, while it was a list.*
- **Nothing under those four imports `tool/` or `cmd/`.** *boundaries.sh.*
- **No `reflect` under `runtime/`.** No exceptions, no fallback path.
  *boundaries.sh.*
- **No dialect branch at run time.** The dialect is chosen once, where a build
  starts — codegen's `decodersFor` and `loweringFor`, migrate's `ddlFor`, the
  CLI's flag — and everything downstream is handed the result. The SQL a
  generated package runs is written under `compile/`, never in `codegen/`.
  *boundaries.sh: `runtime/` imports nothing of storm's outside itself, so it
  cannot reach the code that chooses; and `TestNoSQLTextInCodegen`.*
- **≤ 10 Go files per folder** — outgrowing it means a missing concept.
  *archcheck, as a ratchet: eight folders were over when it was written, at the
  counts recorded there, and none may grow. Splitting `runtime/` to get under
  ten would move exported types, which is a breaking change of its own.*
- **≤ 15 methods per interface**; the `Executor` port has four and a budget of
  five. *archcheck.*
- **One interface per file, one struct per file.** *archcheck, as a ratchet on
  the total excess: new code holds to it, and the old excess may only shrink.*
- **Package clauses are unique**, prefixed with their layer where they would
  collide (`schema/pg` → `package schemapg`), and **storm's own packages are
  imported without aliases**. *archcheck.*
- **Generated output is byte-deterministic** across runs and machines.
  *boundaries.sh regenerates the committed packages and compares them.*
- **The public API stays compatible within v1**, for the packages
  [[docs/STABILITY]] names. *`scripts/check/apicompat.sh`, against the last v1
  tag.*
- **storm emits migrations; only `migrate.Auto` applies them** (ADR-0001, as
  amended 2026-09-07). No *command* applies DDL and nothing applies it
  implicitly — automigrate is a function an adopter calls on purpose, from a
  package they import on purpose, and it refuses any plan that can lose data
  unless explicitly allowed. *That Auto refuses, serialises and applies all or
  nothing is pinned by `migrate/auto_test.go`; that no command applies DDL is
  held in review.*
- **The IR is a logical plan, not a SQL AST** (ADR-0004) — it is what keeps the
  SQL back ends from ossifying around Postgres. *Held in review.*

## Developer profile roster

Adopt the **Driver** profile that owns the code you touch, then re-read your own
diff as the **Challenger** whose budget it most likely breaks. Name both in the
task summary (`Driver: compiler · Challenger: perf`).

| Profile | Owns | Vetoes | Proof |
| :--- | :--- | :--- | :--- |
| **compiler** | `query/`, `compile/`, `codegen/`, shape enumeration, fragment lowering | an identifier reaching SQL text from a runtime value; a builder node that allocates per call; non-deterministic generated output; a lowering rule without a golden test | golden-file suite; `storm verify` clean; fuzz corpus green |
| **perf** | `runtime/`, scanners, shape cache, pooling | `reflect` in any runtime path; `any` boxing per column; a map lookup per query on the warm path; a benchmark whose capacity differs between sides; quoting a number not re-measured this run | `benchstat` delta vs `bench/RESULTS.md`; `testing.AllocsPerRun` assertions; targets in [[docs/PERFORMANCE]] |
| **dba** | `model/`, `schema/`, `migrate/`, introspection, EXPLAIN gates, `bench/` fixtures | storm applying DDL *implicitly*, *unserialised*, or *in part* (`migrate.Auto` is the sanctioned exception and holds all three); a destructive migration step without an explicit opt-in; a query added to a hot path without `EXPLAIN (ANALYZE, BUFFERS)`; an N+1 shipped; a relation load with unbounded round trips | `storm explain` in CI; round-trip counting decorator; model → DDL → introspect round-trip diff empty; `verify --pending` green |
| **dx** | public API, generated-code readability, CLI, errors | an error that does not name the query and the shape mask; an API needing a comment to be understood; generated code a human cannot review; a breaking API change without a version | migration guide runs clean; example suite compiles; adopter feedback from M6 |
| **sec** | injection surface, identifier handling, arg binding | any identifier interpolated from a runtime value; a placeholder count not statically known; bound args logged above debug level; a raw fragment that skips build-time validation | injection corpus green; placeholder arity proven at generation time; fuzz over identifiers |
| **arch** | package boundaries, `Executor` port, scope line | a core package importing non-stdlib; pgx leaking out of `runtime/pgxdrv`; a feature outside the scope line in [[docs/CONCEPT]]; **any dialect branch on the hot path**; a capability sniffed at runtime instead of negotiated at build time | `scripts/check/boundaries.sh` and `internal/archcheck`; `scripts/check/apicompat.sh`; scope line re-read in review |
| **test** | suites, fixtures, corpora, CI gates | a bug fix without a failing-first test; a relation without a round-trip-count assertion; a skipped suite reported as done; an allocation target without an `AllocsPerRun` assertion | `go test -race -shuffle=on` green; the floors in `scripts/check/coverage.sh` — `runtime/` 95%, each `compile/` back end 80–90% |

## Standing truths (all profiles)

A fast path that skips a check is a vulnerability. A check that allocates per
request is a regression. A cache that ignores who asked is a data leak. Any map
keyed by attacker-supplied input needs a bound and an eviction. Every bug fix
ships a test that fails before and passes after, with the root cause named in
one sentence. **Never quote a performance number from memory — re-run the
bench.**

## Working in this repository

Several sessions work on storm at once, and that is how main went red for six
commits in September 2026: one session's `git add -A` in a SHARED checkout
swept another session's unfinished work into five commits it did not write.
So:

- **One worktree per session, on its own branch** (`git worktree add`). A
  checkout another session is using is not yours to stage from, stash in, or
  pull into.
- **Stage by explicit path.** Never `git add -A` or `git add .`.
- **Nothing reaches main except through a pull request whose checks are green.**
  Branch protection on `main` requires `gates`, `test`, `postgres-next`,
  `sqlserver` and `spike`, and every CI step reports even when an earlier one
  failed, so one red step can no longer hide a broken test behind it.
