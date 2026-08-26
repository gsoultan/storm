---
tags: [raorm, production, gates]
updated: 2026-08-25
---

# The road to production-grade

**Scope.** What must be true before a team that is not the author runs raorm
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

### P0.1 raorm silently requires the binary wire format

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

**Gate.** `wireformat_test.go` already holds the evidence and becomes the
guard's test: after the fix it must assert the *refusal* (and the error naming
the column) instead of the inversion. **Kill criterion:** if the per-statement
wire check cannot be made to cost less than 1% of a warm `Get`, the
config-time refusal ships alone and the residual risk — a hand-built pool in
simple protocol — is stated in `STABILITY.md` rather than hidden.

**Driver: dba · Challenger: perf.**

---

## P1 — Unbounded growth and provenance

### P1.1 The shape cache has no bound and no eviction

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

**Fix.** A per-cache cap with a documented policy, defaulting high enough that
a well-behaved program never reaches it. Two candidates, and the choice must
be measured, not argued:
- **Cap + refuse:** past the cap, compile without caching (correct, slower,
  bounded) and expose the event so it can be alerted on.
- **Cap + evict:** 2-random or CLOCK eviction; keeps the hot set, costs a
  counter per entry on the warm path — which is exactly what the warm path
  exists to avoid.

The current `Shapes()` accessor and `raorm lint`'s shape-count budget stay as
the *design-time* gate; this is the *runtime* backstop for when a call site
outgrew its review.

**Gate.** A test that mints 100k distinct shapes and asserts bounded retention
(the retention tripwire in `bench/RESULTS.md` is the template — it must trip
both ways). Warm-path benchmark unchanged within noise, published.

**Driver: perf · Challenger: sec.**

### P1.2 Nobody can depend on raorm

**The defect.** The module is unpublished: `git ls-remote` on
`github.com/gsoultan/raorm` returns no branches. anubis therefore depends on
it by **relative path** (`replace … => ../raorm` plus `use ../raorm` in a
committed `go.work`), which resolves only from `~/projects/anubis`. Proven
today: a second worktree of the same repo cannot build, and anubis's GitHub CI
— which checks out one repository — cannot either.

**Fix, in order.**
1. Push raorm; tag `v0.1.0` when the soak closes (see P3).
2. anubis pins a real version, deletes the `replace`, and drops `../raorm`
   from `go.work`. CI then builds from a checksum-verified module.
3. Until then, anubis's `dev` **must not be pushed** — the branch builds
   locally and nowhere else. This is a decision for the repo owner, not a
   task: publishing is irreversible in the way that matters (the code is
   public, and the module proxy caches it).

**Gate.** anubis CI green on a runner that has never seen `~/projects/raorm`.

**Driver: arch · Challenger: dx.**

---

## P2 — Operability: what happens at 3am

### P2.1 Generated code carries no version

Headers say `Code generated by raorm. DO NOT EDIT.` and the source table, but
not the raorm version. Skew between committed generated code and a bumped
runtime is detectable only by *running* `raorm verify -stale` with the new
binary — which CI does, but a support conversation cannot. Stamp the version;
it is one line, and it makes "which raorm wrote this?" answerable from the
file. (The layout risk is already absent: generated code builds tokens through
`runtime.MakeOrder`, never as raw literals.)

### P2.2 The tracing story is undocumented, not missing

raorm delegates every round trip to the `Executor`, and pgx already has
`QueryTracer`, so per-query spans and slow-query logging are wireable today —
but nothing says so, and an adopter who cannot see their queries assumes they
cannot. Write the recipe (tracer on the pool, span per statement, the shape id
as an attribute) and prove it in `examples/`. Build nothing new until the
recipe is shown to be insufficient.

### P2.3 Errors must never carry values

The error format prints the shape and column names (`shape 0x2c
[tenant_id=, age>=]`) and deliberately not the bound values — the right
design, since these strings land in logs that are less protected than the
database. It is currently a convention. Make it a **test**: bind a sentinel
value into every operator kind, force the failure, and assert the sentinel
appears in no error string.

**Driver: sec · Challenger: dx.**

---

## P3 — The soak, and what it is actually measuring

The two-week anubis soak (2026-08-25 → 2026-09-08, gating the `v0.1.0` tag)
is worth nothing if the only thing observed is "no crash". Record, weekly:

| Signal | Where from | What it would prove |
|---|---|---|
| `authorize` p50/p95/p99 through the repository | the budget test already in anubis | no drift under real traffic, not a synthetic loop |
| shape count per generated package | `Shapes()` | P1.1 in the wild: does a real screen mint shapes per request? |
| RSS after N days | the running process | the shape cache and binder pools are ceilings, not ramps |
| `raorm verify -stale` in CI | already gated | generated code never silently diverged |

**Kill criterion for the tag.** Any of: p95 drift beyond the 2 ms budget, a
shape count that grows monotonically with traffic rather than plateauing, or
RSS without a plateau. Any one of them moves the tag and opens a P0/P1 item —
the date is not the gate, the signals are.

---

## What is already load-bearing (do not re-litigate)

Injection is structural and fuzzed (~80M executions, one real fail-open found
and fixed). Round-trip counts are asserted per shape. Migrations converge
end-to-end. `-race -shuffle=on` is green with coverage floors. govulncheck is
gated with zero reachable findings. The write path's atomicity is tested, not
argued. The first adopter migrated a whole bounded context in a day, and the
four defects that found are fixed with regression tests
(see `.serena/memories/m6_first_adopter.md`).

Numbers live in `bench/RESULTS.md` and in the adopter's own test output.
Neither is ever quoted from memory.
