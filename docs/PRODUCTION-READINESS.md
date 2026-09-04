---
tags: [storm, production, gates]
updated: 2026-08-27
---

# The road to production-grade

**Scope.** What must be true before a team that is not the author runs storm
on Postgres in production. Not v1.0 the version number — v1.0 the *promise*.
`docs/PLAN.md` owns the milestone sequence (M0–M12) and the honest assessment
of what shipped; this file owns the gap between "M6 passed" and "someone else
can run it", written as gates with kill criteria, because "production ready"
is not one property and a checklist that cannot fail is marketing.

Every item below was verified in the tree on 2026-08-25, after M6. Items the
assessment in `PLAN.md` already closed are not repeated here; what is repeated
is only what a first adopter or a reading of the runtime turned up.

---

## P0 — Silent wrong answers (nothing ships past these)

### P0.1 storm silently requires the binary wire format — **CLOSED 2026-08-26**

**The defect, measured** (`runtime/pgxdrv/wireformat_test.go`, against a live
server on 2026-08-25 — not inferred from reading):

```
simple protocol: bool false arrives as "f" (66) → runtime.Bool = true
simple protocol: int8 42 arrives as "42" (2 bytes)
CONFIRMED: SELECT false decodes as true — silent inversion, no error
```

Generated scanners decode `rows.RawValues()` as Postgres *binary* format:
`runtime.Int8` is `binary.BigEndian.Uint64`, `runtime.UUID` copies 16 bytes,
`runtime.Bool` is `len(b) > 0 && b[0] != 0`. Under pgx's **simple protocol**
values arrive as *text*, and the two failure modes differ in the worst
possible way:

- **Booleans invert silently.** `'f'` is 0x66, which is not zero, so `false`
  reads as `true` — no error, no log line. In an authorization system that is
  a deny becoming an allow.
- **Fixed-width numbers fail loudly.** `42` arrives as the two bytes `"42"`
  and `binary.BigEndian.Uint64` panics on the short slice.

Nothing in `runtime/pgxdrv` pins the exec mode or inspects a field
descriptor's format code, so neither is caught before it reaches a caller.

**Why it will happen to someone.** `QueryExecModeSimpleProtocol` and
`QueryExecModeExec` are the documented pgx settings for running behind
**PgBouncer in transaction or statement pooling mode** — the most common
Postgres deployment topology at scale. An adopter changes one pool setting for
an unrelated operational reason and every boolean in the system inverts, with
no error, no log line, and no test failure.

**Fix.**
1. `pgxdrv.NewPool`/`NewPoolConfig` refuse a config whose
   `DefaultQueryExecMode` is `SimpleProtocol` or `Exec`, naming PgBouncer and
   the supported alternative. Free: config time, once.
2. A pool the adopter built themselves can still be wrapped in
   `pgxdrv.Pool{P: …}`, so also verify at the wire: on the first row of a
   statement, check the field descriptors' format codes are binary and fail
   with a *data* error naming the column. Cost is one loop per statement
   shape, not per row — measure it and put the number in `bench/RESULTS.md`
   before keeping it.
3. `docs/` gains a deployment page: PgBouncer session mode works; transaction
   mode works only with prepared-statement handling pgx already provides;
   simple protocol is refused, and why.

**Gate — met.** Both halves shipped. `NewPool`/`NewPoolConfig` refuse
`SimpleProtocol` and `Exec` at construction, and every result is checked once
per statement (`checkFormats`), so a hand-built pool wrapped in
`pgxdrv.Pool{P: …}` is covered too. `wireformat_test.go` now asserts the
refusal, the error naming the column, and — still — that the decoder hazard is
real, so the guard cannot be argued away later.

**Cost — inside the kill criterion by ~230×.** 3.82 ns for an eight-column
result, zero allocations, against a same-session `Get` of ~89.5 µs: **0.0043%**
where the criterion allowed 1% (`bench/RESULTS.md`, 2026-08-26). The
config-time refusal did not have to ship alone.

**What writing it taught.** The first rule was "every column must be binary",
which failed four fixture tests on a Postgres **enum**: pgx sends an enum's
label as text, and that label *is* the value. The rule is now a closed
deny-list of the types storm decodes from a fixed binary layout, so
user-defined types a schema adds later need no permission — while domains,
which PostgreSQL reports under their base type's OID, are still checked
properly. `docs/DEPLOYMENT.md` states the whole thing for adopters, with the
PgBouncer pooling-mode table that is the actual reason anyone lands here.

**Driver: dba · Challenger: perf.**

---

## P1 — Unbounded growth and provenance

### P1.1 The shape cache has no bound and no eviction — **CLOSED 2026-08-26**

**The defect.** `runtime.TreeCache.entries` is a `map[uint64][]*treeEntry`
that only ever grows: `Put` appends, nothing evicts, and each entry retains a
compiled `*Stmt` (its SQL string) **for the life of the process** — which the
`SpliceTree` doc comment states as the design, because shapes are assumed to
come from code structure.

**Why the assumption breaks.** A search screen with *n* optional filters can
mint 2ⁿ shapes; user-chosen sort columns and directions multiply it again. The
structure is then derived from request data, and the standing rule applies:
*a map keyed by attacker-supplied input needs a bound and an eviction.*
Twenty optional filters is a megabyte-scale cache that never shrinks, and the
process looks like a slow leak with no leak.

**Fix — shipped: cap and bulk drop.** Past `runtime.ShapeCap` (default 1024
per cache) the map is dropped whole and refills from the shapes still in use.

Neither candidate in the original plan won. **Cap + refuse** is not
self-healing: 100k junk shapes at startup would fill the cache and every
legitimate shape afterwards would compile forever. **Cap + evict** needs
per-entry usage tracking, which is a *write on the read path* — a shared
cache line dirtied by every hit on every core, which is the exact cost the
warm path exists to avoid. A bulk drop is bounded, self-healing, keeps the
read path read-only, and costs one allocation on the cold path. Statements
handed out before a drop stay valid: callers hold the pointer, not the map.

`SetShapeCap(0)` restores the old unbounded behaviour for a program that can
prove its shapes come from code. Generated packages expose `ShapeFlushes()`,
so "a filter screen turned into 2ⁿ statements" is a gauge rather than a
mystery.

**Gate — met.** `TestShapeCap_BoundsAShapeExplosion` mints 100k shapes:
capped holds 575 and retains 170 KB, unbounded holds 100,000 and retains
27,833 KB — **164×**, and the test fails if the unbounded arm does *not*
explode, so it trips both ways. Warm path within half a nanosecond with zero
allocations (`benchstat` in `bench/RESULTS.md`), and `Get` is byte-identical
— the check is in `Put`.

**Driver: perf · Challenger: sec.**

### P1.2 Nobody can depend on storm — **CLOSED 2026-08-26**

**The defect.** The module is unpublished: `git ls-remote` on
`github.com/gsoultan/storm` returns no branches. anubis therefore depends on
it by **relative path** (`replace … => ../storm` plus `use ../storm` in a
committed `go.work`), which resolves only from `~/projects/anubis`. Proven
today: a second worktree of the same repo cannot build, and anubis's GitHub CI
— which checks out one repository — cannot either.

**What actually happened.** The owner published storm on 2026-08-26 —
`github.com/gsoultan/storm` is public with `main` at 67c7b71 — and solved
consumption from the anubis side rather than by pinning a module version:
`scripts/ci/fetch-workspace-modules.sh` reads the sibling entries out of
`go.work` and clones each from GitHub at `ANUBIS_STORM_REF` (default `main`),
with a FATAL path that says, correctly, that an unpushed sibling makes the
repository unbuildable by anyone but its author. The Dockerfile does the same.

That is a legitimate alternative to a pinned version: it works with an
unreleased module and keeps the workspace layout developers already have. The
cost is that anubis's build now tracks a moving `main` rather than a
checksum-verified version, which is fine for a pre-v0.1 dependency and is the
thing `v0.1.0` will retire.

**How it closed, the same day.** `main` was pushed, **tagged `v0.1.0`**, and
anubis moved off the sibling entirely: the `replace` is gone from `go.mod`,
`../storm` is gone from `go.work`, and the requirement is
`github.com/gsoultan/storm v0.1.0` — a checksum-verified module from the
proxy. The clone-the-sibling scaffolding that made CI work in the meantime is
no longer load-bearing.

The header-only drift this predicted did happen, exactly as described: the
committed generated files said `(devel)` and the pinned build emits
`v0.1.0`, so `rgen/` needed regenerating. The drift gate in
`scripts/ci/backend-suite.sh` caught it rather than letting it reach CI.

**Gate — met.** anubis builds and its whole test suite passes with **no
sibling checkout anywhere on the machine's path**: the dependency now comes
from the module proxy. That is the property the gate asked for.

**Driver: arch · Challenger: dx.**

---

## P2 — Operability: what happens at 3am — **CLOSED 2026-08-26**

### P2.1 Generated code carries no version — done

Headers now read `Code generated by storm v0.1.0. DO NOT EDIT.`, sourced from
the build info of the storm module the generator was built against —
`(devel)` from storm's own tree, `(replaced)` for a filesystem replace, since
a replaced module has no version worth pretending about. Skew was already
*detectable* by running `verify -stale` with the new binary; this is the
forensic half, readable without running anything. It costs an upgrade its
regeneration, which is the bargain sqlc and protoc-gen-go make too.

### P2.2 The tracing story is undocumented, not missing — done, and the proof corrected the story

Writing the test found the thing the doc would have got wrong: pgx splits
tracing across interfaces, and **`QueryTracer` does not see batched
statements** — which is where a named plan's relation loads travel. An
adopter wiring only `QueryTracer` would watch the one query they wrote, never
see the four the plan issued, and conclude storm hides work. The recipe in
`docs/DEPLOYMENT.md` is therefore one type implementing `QueryTracer` +
`BatchTracer` + `CopyFromTracer`, with a table mapping each storm call to the
interface that sees it, and `runtime/pgxdrv/tracing_test.go` asserts a query,
an exec and both statements inside a batch are observed. Nothing new was
built, as planned.

### P2.3 Errors must never carry values — done

`bench/errhygiene_test.go` binds a sentinel through thirteen query shapes —
every value-taking operator across text, numeric, uuid and time columns, plus
composition, disjunction, negation and a keyset cursor — and asserts it
appears in neither the compiled SQL nor any error from `Err`, `All`, `One`,
`Count` or `Exists` against a refusing database. It also asserts the sentinel
IS among the bound args, so the test cannot pass by binding nothing.

The property held already (storm's runtime errors are static sentinels); this
makes it fail the day someone adds `fmt.Errorf("... %v", val)` to be helpful.
What it deliberately does not claim: PostgreSQL puts offending values in its
own diagnostics (`Key (email)=(…) already exists`), that message belongs to
the server, and storm must not rewrite it.

**Driver: sec · Challenger: dx.**

### P2.4 Constraint violations were unclassified — **CLOSED 2026-08-27**

Every violation — duplicate key, missing parent, failed CHECK, exclusion
overlap, serialization failure — arrived as an opaque driver error. Telling a
409 from a 500 meant type-asserting `*pgconn.PgError` and comparing SQLSTATE
strings in a handler, which is the driver knowledge the `Executor` port exists
to contain.

`runtime` now defines the sentinels and `ConstraintError`; `runtime/pgxdrv`
maps SQLSTATE at the boundary, the only place one is read. Every exit path is
covered including `rows.Err()`, because pgx defers a statement's error to the
drain and that is where a `RETURNING` insert's violation surfaces.

`ConstraintError` carries schema metadata and no bound value, keeping P2.3's
property: the server's own diagnostic does carry values, belongs to the server,
and stays reachable through `Unwrap` rather than being folded into storm's text.

**Gate — met.** Each of the five violation kinds is asserted against a live
server, an unrelated error is asserted to pass through unchanged, and the
example maps them to 409 with a `retryable` flag.

---

## P4 — The first-run path was broken, and only an outsider could see it
**Found and fixed 2026-08-26, after v0.1.0**

Everything above was verified from inside this repository or from anubis, a
module written by storm's author with storm's assumptions baked in. So the
last check was to be a stranger: a fresh module outside both trees, `go get
github.com/gsoultan/storm@v0.1.0`, and follow the README.

Three defects in the first five minutes, all invisible from inside:

1. **`generate` emitted storm's own module path into somebody else's
   module.** `PackageImport` was built from the constant
   `github.com/gsoultan/storm`, so a user's generated context package
   imported `github.com/gsoultan/storm/internal/store/user` — code that
   cannot compile, from the tool's flagship command. It went unnoticed
   because every generation that had ever run was inside this repository,
   where the wrong answer is the right one. The path now comes from the
   host module's `go.mod`, and the regression test generates into a module
   named `example.com/someoneelse`, which is the only arrangement that can
   tell the two apart.

2. **The tool was unreachable.** `Models` was documented as "set by the
   generated bootstrap in the user's module", but nothing generated a
   bootstrap and `Models` lived in `package main`, which cannot be imported.
   An installed `storm` binary could only print "no models registered", and
   the doc it pointed at described a `go tool storm generate` flow that
   could not work. So an adopter got `generate` by hand-rolling a `main`
   around `codegen.Package` — which is what anubis did — and simply never
   got `verify -pending`, `lint` or `explain` at all. The commands are now
   the importable package `storm/tool`, and a user's whole tool is
   `func main() { tool.Main(model.All(), nil) }`.

   **Closed at the cause in v0.3.0.** Five lines is still five lines the
   adopter has to know to write, and one they only write if they read the
   right section first — so the tool stayed reachable-in-principle. It now
   discovers the models by parsing and writes that bootstrap itself
   ([ADR-0006](adr/0006-discovery-replaces-the-bootstrap.md)), which means
   `verify`, `lint` and `explain` are reachable by default rather than by
   diligence. `scripts/check/outsider.sh` grew a second stranger who writes
   no bootstrap at all, and asserts the two paths generate the same bytes.

3. **An output path through a symlink was refused.** On macOS `/tmp` is a
   symlink, so an absolute output directory under it compared as "outside
   this module". Now the deepest existing ancestor is resolved on both
   sides.

**Gate — met.** A module outside both trees now goes model → five-line main →
`ddl` → `generate` → compiling generated code → insert and query against
PostgreSQL, with `Shapes() == 1` after the query. That is the whole adopter
path, walked by something that shares no assumptions with this repository.

Since v0.3.0 the same gate is walked a second time by a module with **no
bootstrap and no registry** — only a model package — because everything the
first stranger exercises would keep passing if discovery were broken: the tool
it uses is one the test itself wrote.

**The lesson, which is the reusable part:** every check that runs inside your
own repository shares your repository's assumptions. The bug here was not
subtle — it was the main command, completely broken for everyone else — and
no test, gate, coverage floor or adopter caught it, because the adopter was
also us. Ship a stranger's smoke test, or ship the bug.

So the stranger is now a gate: `scripts/check/outsider.sh` builds a module
with a different path outside the tree and asserts the whole path a new
adopter walks —

    model → five-line main → ddl → generate → compile → diff → apply → verify
          → verify -pending → lint → explain → import

— every command the tool has. The migration half is the riskiest thing an ORM
does; `import` is the on-ramp anyone with an existing database walks, and its
draft is asserted to be Go that parses, because a draft nobody can compile is
not an on-ramp. The database-backed half skips without a DSN, so the check
still runs where there is no server.

Two properties make it a tripwire rather than a passing test. It was verified
to fail with the import-path fix reverted, where it names the defect and
prints the offending imports. And it asserts `verify` reports drift on the
empty namespace BEFORE the migration is applied — without that, "clean
afterwards" would pass equally well if `verify` were broken and always said
yes.

**Driver: dx · Challenger: arch.**

---

## P3 — The soak, and what it is actually measuring

The two-week anubis soak (2026-08-25 → 2026-09-08; it was written to gate
`v0.1.0` and has outlived that — the tag it now bears on is the next release
after it closes)
is worth nothing if the only thing observed is "no crash". Record, weekly:

| Signal | Where from | What it would prove |
|---|---|---|
| `authorize` p50/p95/p99 through the repository | the budget test already in anubis | no drift under real traffic, not a synthetic loop |
| shape count per generated package | `Shapes()` | P1.1 in the wild: does a real screen mint shapes per request? |
| RSS after N days | the running process | the shape cache and binder pools are ceilings, not ramps |
| `storm verify -stale` in CI | already gated | generated code never silently diverged |

**Status 2026-08-27: the soak now measures a live process, and its first real
run is clean for the request path.** Until `anubis/scripts/soak-load.sh`
existed, every reading recorded "not running" — the recorder measured a test
loop that exits, so two of the four signals had no source and the soak was
really proving that the test suite passes. Under ~12,200 authorize decisions
per second, live heap stayed flat (271–353 MB across four rounds) while RSS
tripled, shapes stayed at 1 and flushes at 0. Nothing accumulates per request;
the RSS climb is pages Go has not returned. Absolute memory numbers need a
longer, quieter run before they mean much — the comparison is the signal.

**The tag went out first (2026-08-26), so the remedy changed.** `v0.1.0` was
cut on the day the four P0/P1/P2 gates closed rather than at the end of the
soak window. That is the owner's call and it is defensible — the gates that
would have blocked it are closed — but it means the soak is now running
against a **released** version, and its kill criterion can no longer move a
tag that exists.

So the criterion is restated rather than quietly dropped. Any of: authorize
p95 drift beyond the 2 ms budget, a shape count that grows monotonically with
traffic rather than plateauing, or RSS without a plateau — each now opens a
**v0.1.1**, and the finding goes in this file first. The signals are
unchanged; only the remedy is.

## P5 — The only gate on emitted SQL did not look at the new features — **CLOSED 2026-09-02**

`storm explain` is the one check that asserts every statement storm emits is
one PostgreSQL accepts: it sends each through `EXPLAIN (GENERIC_PLAN)`, which
plans a parameterised statement without inventing bind values. It is worth
exactly what it enumerates, and it enumerated base reads and named-plan
relation loads — the part that could hardly be wrong.

Everything v0.3.0 added was invisible to it. Declared aggregations carry GROUP
BY, HAVING, FILTER, grouping sets and window frames; declared joins carry CTEs
and an ON clause across tables. That SQL is fixed at generate time, so it is
either valid or it is a runtime error the first time a caller asks for it — and
"no test calls it" is the normal state of a feature declared last week. On the
orders model the enumeration went from 6 statements to 14.

**And it was not running in CI at all**, which is v1 criterion 5 ("fails CI on a
query that regressed its plan").

**Gate.** `scripts/check/explain.sh` applies the orders model's DDL to a scratch
namespace and plans every statement, as a CI step. Proven to have teeth rather
than assumed: changing the HAVING lowering to emit ` HAVING WHERE ` — one
keyword, accepted by every unit test, executed by no test — is caught with
`syntax error at or near "WHERE"` and fails the build.

`storm lint` is deliberately NOT in that script. The orders model declares no
named plans on purpose (every relation already gets one), so lint there would
report success without checking anything. The round-trip budget is gated by
`TestPlanCosts`, which asserts the exact count for each of the fixture's three
plans and fails through `go test ./...`.

**Not covered, and worth saying:** the seq-scan half of explain needs statistics
to mean anything. Against an empty CI database the planner's row estimates
cannot reach the threshold, so this gate asserts validity, not performance. A
stats-bearing replica is where the performance signal lives.

---

## What is already load-bearing (do not re-litigate)

Injection is structural and fuzzed (~80M executions, one real fail-open found
and fixed). The structural argument now covers the escape hatch too: `storm.SQL`
statements are pinned to the text `storm generate` PREPAREd, so a statement
assembled at run time is refused before the executor is reached, and a
declaration that is not a package-level var fails the build. Round-trip counts are asserted per shape. Migrations converge
end-to-end. `-race -shuffle=on` is green with coverage floors. govulncheck is
gated with zero reachable findings. The write path's atomicity is tested, not
argued. The first adopter migrated a whole bounded context in a day, and the
four defects that found are fixed with regression tests
(see `.serena/memories/m6_first_adopter.md`).

Numbers live in `bench/RESULTS.md` and in the adopter's own test output.
Neither is ever quoted from memory.
