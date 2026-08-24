---
tags: [raorm, plan, milestones]
updated: 2026-08-24
status: proposed
---

# PLAN — milestones, gates, kill criteria

> The plan is a program, not a list. Every milestone has an **exit gate** that
> is a command someone can run, and most have a **kill criterion** that ends the
> project rather than negotiating with it.

Sequenced so the **riskiest claim is tested first and cheapest**. If the thesis
is false you learn it in two weeks for the price of one spike, not in month five
for the price of an ORM.

## Four decisions this plan assumes

Marked `Proposed` in `docs/adr/`. Override any of them and the plan changes;
they are called out here so the override is deliberate.

1. **Model-first, with migration-mediated DDL.** The Go model is the source of
   truth; raorm emits reviewable migrations and **never applies one**.
   Introspection stays as the on-ramp (`raorm import`). *Requires rewording the
   `anubis` rule that SQL lives only in `db/queries/` and `migrations/` — the
   DDL still lands there, but generated rather than hand-typed.*
   → [[adr/0001-schema-source-of-truth]]
2. **Postgres first; other targets sequenced.** The dialect is a compile-time
   parameter, so multi-target costs the hot path nothing. No dialect conditional
   outside `compile/`. → [[adr/0002-postgres-only]], [[DIALECTS]]
3. **No lazy loading, ever.** Fetch plans live in the type system, and they are
   **named** (`plans.go`), not inferred from call sites.
   → [[adr/0003-no-lazy-loading]]
4. **MongoDB is a back end, not a dialect.** The IR is a logical plan; shape
   differences (embed vs join) are declared, never inferred.
   → [[adr/0004-mongodb-as-backend]]

## Milestone table

| # | Milestone | Wks | Exit gate | Kill criterion |
|---|---|---|---|---|
| M0 | Thesis spike | ✅ | **PASSED 2026-08-23** — 0.95× raw pgx, 0-alloc warm path. `bench/RESULTS.md` | *(cleared: kill line was 1.30×)* |
| M1 | Model + schema IR + migration diff | ✅ | **PASSED 2026-08-24** — round-trip is a fixpoint; migrations converge on a live DB | *(cleared)* |
| M2 | Query compiler + read codegen | ◐ | **read path shipped 2026-08-24** — end-to-end parity, 0 allocs, deterministic. Expressiveness (joins, CTEs, windows, ordering, pagination) outstanding → **P5** | *(cleared: codegen model is sound)* |
| M3 | Relations without N+1 (named plans) | 4 | 2 round trips; unloaded read is a compile error; polymorphic integrity generated | plan types unusable → runtime-checked plan B |
| M4 | Writes, unit of work, batching | ✅ | **PASSED 2026-08-24** — 1,000 inserts = 1 `COPY`; 1,000 mixed = 1 batch; FK order correct with constraints *not* deferred | *(cleared)* |
| M5 | Typed escape hatch | 2 | real analytical query, fully typed, no `any` | — |
| M6 | First adopter: `anubis/authz` | 3 | authorize p95 does not regress | > 3 wks or p95 regression → freeze features |
| M7 | Tooling gate + hardening | ✅ | **PASSED 2026-08-24** — explain/lint/verify(-stale,-pending) shipped and tested; fuzz corpus + injection suite in CI; coverage floors enforced | *(cleared)* |
| M8 | v1.0 release (Postgres) | 2 | docs, examples, stability policy | — |
| M9 | MySQL 8 + MariaDB | 4 | full suite green on both; seam has no leaks | seam leaked → fix `compile/` before any further target |
| M10 | SQL Server | 3 | `OUTPUT`, `MERGE`, TVP bulk, paging gate | — |
| M11 | Oracle | 4 | empty-string-is-NULL surfaced at declare time | capability model cannot carry Oracle → **Mongo is cancelled** |
| M12 | MongoDB | 6 | one model serves both stores, divergence build-checked | — |

**M0–M8 (v1.0, Postgres): ≈ 23 weeks solo (~5.5 months), ≈ 15 with two** — M1 and
the bench harness parallelise cleanly, M2–M5 mostly do not.
**M9–M12 (all targets): + ≈ 17 weeks**, and these *do* parallelise across people
once the seam is proven by M9. Nothing in M9+ starts before v1.0 ships.

---

## Next development sequence (adopted 2026-08-24)

The milestone table says *what* and *why*. This says **what happens next, in what
order**, and it deliberately does not run M3 → M4 → M5 in numeric order. Three
findings from reading the code as it actually stands drove the reordering:

1. **`codegen.File` generates one table per call.** There is no whole-package
   generation and no `raorm generate` subcommand. M3 and M4 both hard-require
   multi-table output — FK-ordered flush needs the whole graph, relations span
   tables. This is a prerequisite, not a milestone.
2. **The query-side dialect seam does not exist.** `SELECT` / `ORDER BY` /
   `LIMIT` / `$N` / quoting are hardcoded Postgres literals in `codegen/gen.go`;
   `compile/` holds only `pgddl`. **R9's mitigation — "CI-enforced from M2, not
   added at M9" — was never implemented.** There is nothing to enforce.
3. **M3 is not blocked on joins.** M2's `= ANY` work already proved the N+1
   guarantee — 50 parents + 25,000 children, exactly 2 round trips, asserted by
   `CountingExecutor`. The M2 table said otherwise; that claim is struck.

| Phase | Work | Milestone | Est. |
|---|---|---|---|
| **P0** | git baseline + this plan ✅ | — | hours |
| **P1a** | multi-table package generation, `raorm generate` ✅ | prereq | ~4 d |
| **P1b** | relocate query lowering into `compile/pgsql` + CI check ✅ | R9 | ~2 d |
| **P2** | plan-type ergonomics spike, hand-written ✅ | de-risks M3 | 3 d |
| **P3** | writes, unit of work, batching ✅ | **M4** | 2 wk |
| **P4** | relations, named plans, `= ANY` alloc fix ◐ | **M3** | 4 wk |
| **P5** | joins, ordering, pagination, projections, CTEs, windows | **M2** rest | 4 wk+ |

≈ 12 weeks to a write-capable, relation-capable Postgres ORM with the dialect
seam intact; P5 before the M6 adopter.

### P0 — git baseline (hours)

9,143 lines of green, benchmarked code sat in an untracked working tree with
**zero commits**. One `rm -rf` and M0, M1 and M2 are gone.

One honest baseline commit, tagged `m2-read-path`. **Not** three fabricated
milestone commits — reconstructing history that never happened is worse than
one commit that says so.

### P1a — multi-table package generation (~4 days)
*Driver: compiler · Challenger: dx*

`codegen.Package(*schema.Schema, …)` emitting N tables into one package, driven
by a real `raorm generate`. Today `cmd/genbench` names a single table by hand.

**Gate:** output byte-deterministic across 5 regenerations; the existing
`bench/genuser` output is unchanged (so the P1a diff cannot hide a perf change).

### P1b — relocate the query lowering (~2 days)
*Driver: arch · Challenger: perf*

Move the Postgres literals and placeholder style out of `codegen/gen.go` into
`compile/pgsql`, and add a `scripts/check/` rule that fails on SQL syntax
literals under `codegen/`.

**Explicitly not a `Dialect` interface.** You do not know the right seam until
the second implementation, and inventing one now is speculative generality. The
interface waits for M9, when there are two implementations to generalise from.
This phase is *relocation plus a check* — nothing more.

**Why now, not at M9.** ~450 lines of lowering to move today. Every feature in
P3–P5 multiplies it. This is the cheapest this fix will ever be.

**Gate:** a planted `SELECT` literal in `codegen/` fails the boundary check;
generated output byte-identical to P1a's; warm-path benchstat delta zero.

**Kill criterion.** More than one week, or *any* measurable ns/alloc cost on the
warm path → revert it and **rewrite ADR-0002 to say Postgres-only through
v1.0**. Declaring the limitation honestly beats shipping a seam that only looks
like one.

### P2 — plan-type ergonomics spike (3 days)
*Driver: dx · Challenger: compiler*

M3 is four weeks and its one surviving risk is *"are named plan types usable?"*.
Answer it the way M0 answered the thesis: **hand-write** what the generator
would emit for a single `Org → []User` plan, add a `testdata/compilefail` case,
then write real calling code against it.

Same split as the M0 spike, for the same reason: *"is the design usable?"* and
*"can a generator emit it?"* are two failures that are otherwise
indistinguishable.

**Gate:** the API is usable without a comment explaining it (the `dx` veto); the
compile error names the plan to add.

**Kill criterion.** Unusable → adopt plan B (runtime-checked plan) **on day 3**,
not in week four.

#### Result: PASSED (2026-08-24). Plan B is not needed.

`internal/planspike/` — two genuinely generated table packages (`org`, `user`)
with a hand-written plan layer above them. **50 parents + 25,000 children in
exactly 2 round trips**, asserted at parent counts 1, 7 and 50 so a loader that
batches per parent cannot pass. Empty parent set costs 1, not 2. Parent
predicates compose exactly as on a bare query.

Reading an unloaded relation does not compile, and the message is the one
[[API]] promised:

```
rows[0].Users undefined (type org.Row has no field or method Users)
```

`testdata/compilefail/` asserts it, each case carrying a `// want:` line, so a
refactor cannot quietly turn ADR-0003 back into a convention.

**Four findings, in descending order of how much they change M3.**

1. **`Load(plan)` as written in [[API]] §7 cannot be built.** Go methods may not
   have type parameters, so `q.Load(UserFeed)` has no way to vary its return
   type by plan — `.All()` after it could only return one type for every plan.
   The plan must be the **entry point** (`store.OrgsWithUsers().Where(…)`) or a
   generated method per plan. The doc needs correcting; the design survives.
2. **The plan layer cannot live in a table package.** A plan names two tables,
   and a table package importing a sibling reintroduces the import cycle that
   one-package-per-table exists to avoid — `Org` has `Users`, `User` has an
   `Org`, and no spelling of the fields makes that compile. Plans belong in the
   parent package, which imports every table package and is imported by none.
   [[API]] already put `plans.go` there; this is the structural reason it must be.
3. **Every builder method must be redeclared on the plan type.** Go has no
   delegation, and embedding `org.Query` would make `Where()` return `org.Query`
   — dropping straight out of the plan. It is pure boilerplate, but a
   *generator* writes it, so it costs nobody anything. Not a reason to stop.
4. **The default `limit: 1000` silently truncates a relation load.** Sensible
   as a guard on a single-table read; on a child fetch it drops rows and every
   count computed from the result is wrong. The spike's own first run failed
   this way at 50×500. Settled: **reaching the child limit is an error, never a
   partial result.** Open for M3: what the limit should be — derived from the
   parent limit times a declared per-parent bound, or required from the plan.
   Any constant is arbitrary.

### P3 — writes = M4 ✅ PASSED (2026-08-24)

Ahead of relations, against the numeric order, because: it is independent of the
IR work, it carries no design risk, its exit gates are already written, and
**`anubis` cannot adopt a read-only ORM at M6.** It consumes P1a's multi-table
output for FK ordering.

**Shipped.** A masked insert builder (`Create()`), a dirty-mask update
(`Mutate(row)`), delete by primary key, and generated optimistic locking on the
version column. Absence is tracked by the mask, never inferred from a zero
value — the inference other ORMs make is why they cannot insert a `false`, a `0`
or an empty string into a column with a default.

Gates met: dirty set **0 allocs**; one compiled statement per distinct mask;
16 concurrent writers on one version → exactly 1 wins and 15 get
`ErrStaleWrite`; the SET list names only assigned columns, asserted on the SQL
text rather than behaviourally (a behavioural test passes even if every column
is rewritten to its existing value).

**Two bugs the tests found, both real.** `runtime.CountingExecutor` raced — it
is exported for use in *user* tests, and tests are where concurrency lives, so
a contention test wrapping it failed under `-race` reporting a race in the tool
instead of the bug being hunted. And a NOT NULL column with no default that the
generator cannot bind made every INSERT fail at runtime; it is a generation
error now, and it fired on the fixture immediately.

**M4 complete (2026-08-24).** `COPY`, batching, `raorm.Unit` and upserts all
landed on top of [[adr/0005-executor-port-width]], which grew the `Executor`
port to four methods and left one of the five-method budget unspent.

Gates met, all four:

- **1,000 inserts = one `COPY`** — exactly 1 round trip, re-read confirms 1,000
- **1,000 mixed statements = one batch** — exactly 1 round trip, 1,000 affected
- **a graph writes correctly with constraints NOT deferred** — the child is
  staged *first*, and the test asserts against `pg_constraint` that the foreign
  keys are not deferrable, so the database cannot forgive a mistake and let the
  gate pass without ordering anything
- **dirty set: 0 allocations**, and the bulk row source does not scale
  allocations with row count

**No deferred id handles, and none needed.** [[API]] §8 sketched them — insert a
parent, get a placeholder, use it as a child's FK before the parent exists.
`raorm.Model`'s id is a **client-generated UUID**, so the parent's key is known
*before* the insert. Handles are only unavoidable when the database assigns the
key, which is the sequence-id model raorm does not use.

**Foreign-key cycles are a generation error** naming the cycle. A mutual
reference has no write order that satisfies both, and the fix is a modelling
decision (make one side nullable, write it in two steps) rather than something
a generator should paper over by deferring a constraint. Self-references are
not cycles: they order *rows*, not tables, and treating them as cycles would
make every hierarchy unwritable.

### P4 — relations = M3 (4 weeks)

Gates unchanged — see M3 below. One addition:

**Fold in the `= ANY` parameter-side allocation fix.** Re-measured 2026-08-24
now that the plan layer makes it the common path — numbers in
`bench/RESULTS.md`, never from memory. It is ~2 allocations per bound id, on
pgx's *parameter* side, not our scan path.

**One hypothesis already killed:** `pgtype.FlatArray` — pgx's own array wrapper,
and the obvious first thing to reach for — is within noise of a plain slice.
Both pay the same per-element cost inside pgx's generic array codec, which boxes
every element into an `any` and builds a per-element encode plan. The fix is a
`pgtype.Codec` registered on the type map in `runtime/pgxdrv` (the only package
allowed to name a pgx type), encoding the array straight into the output buffer,
repeated per array element type a relation key can have.
`BenchmarkAnyParam` and `BenchmarkEncodeIDArray` are the regression guard.

### P4 status (2026-08-24)

**Shipped:** relation metadata in the IR, and **one generated plan type per
relation** — finite by construction (n relations, never 2ⁿ) and needing no
declaration mechanism, so it lands without the `plans.go` front end. All four
relation kinds in the fixture at **exactly 2 round trips**: has-many,
belongs-to (keys de-duplicated, so 1,000 users on 50 orgs fetch 50 orgs),
self-referential has-many, and self-referential to-one where every key is NULL
(1 round trip, not 2). Unloaded relations still do not compile; the P2
compile-fail suite now runs against generated code.

**Shipped since (2026-08-24):**

- **Plans page their parents.** `Order`, `Offset` and `After` were added to
  `Query` *after* the plan delegation list was written and were missing from
  every plan — a plan could filter but not page. The list is the bug surface;
  anything added to `Query` belongs on it too.
- **`ChildTop(n)` — greatest-n-per-group.** "Fifty tenants, each with its five
  newest people" in one query, still 2 round trips. Requires `ChildOrder`, and
  that ordering must be a strict total order: *"the first three by date"* with
  ties returns an arbitrary three and a different arbitrary three next call.
- **The default lowering was chosen by measurement, and the measurement
  reversed the choice.** I defaulted to `row_number()` reasoning that one index
  scan beats a per-parent nested loop. Wrong: the window form reads every child
  of every matched parent, so its cost tracks the *total child count* while
  `LATERAL`'s tracks the *rows returned* — 32.8× at a hundred parents. Both stay
  generated; a default chosen by measurement needs something measured against.
- **The `= ANY` parameter codec.** 1,003 allocations → **1** isolated; 1,021 →
  **11** end to end at 500 ids, and now **flat in id count**. Byte-identical to
  pgx's own output, asserted — a parameter encoder that is fast and subtly wrong
  corrupts a query's meaning rather than failing it.
- **Recursive traversal.** `Descend`/`Ascend` with a mandatory depth bound and a
  path-array cycle guard, proven against a real cycle built in a real database.

**Still outstanding for M3:** named multi-relation plans (needs the `plans.go`
front end), m2m with payload, and polymorphic associations.

### P5 — read expressiveness = rest of M2 (4 weeks+)

Joins, runtime `Order`, `OFFSET` and keyset pagination, projections into custom
row types, CTEs, windows, `FILTER`, `GROUPING SETS`, `UNION ALL`. The largest
single design item; it lands through P1b's seam.

**Ordering and keyset pagination are not in any milestone and should be.**
`ORDER BY` is currently a *generation-time constant* (`codegen.Options.OrderBy`)
and there is no `OFFSET` and no cursor — a hard blocker for any list endpoint
`anubis` has. ~3 days. If M6 timing tightens, they pull forward into P3.

### Debts carried from M0 (bench-harness work, parallelises with P1)

| Debt | Source | Status |
|---|---|---|
| **Ent benchmark** — needs its own codegen step | M0 scope, never done | open |
| **Unix-socket re-benchmark** | M0 finding #3 | **blocked on the machine, not the code** |

**Unix socket — why it is still open.** `make db` runs Postgres in an Apple
container reachable only over TCP, and there is no local Postgres on the dev
machine (`brew list | grep postgres` is empty). Measuring raorm's overhead
against a ~64 µs round trip is measuring the network, so the ≤ 1.15×
wall-clock gate passed *without discriminating* — and it will keep doing so
until this runs over a socket. Unblocking it is:

```console
brew install postgresql@17 && brew services start postgresql@17
RAORM_DSN='postgres:///raorm?host=/tmp' make bench
```

Do not close this by re-running the container benchmark and reporting a better
number. The measurement is the point, not the number.

**Ent.** Every other rival is already in `bench/` (sqlc, Bun, GORM against raw
pgx). Ent needs `entgo.io/ent` in `go.mod` plus a real `go generate` step, so it
is the only one that cannot be added by writing a `_test.go` file — which is why
it keeps being the one left out.

---

## M0 — Thesis spike ✅ PASSED (2026-08-23)

**Result: the thesis is not falsified.** 0.95× raw pgx single-row, 0.93× on
1,000 rows, **0 allocations and 14 ns** on the warm dynamic path, all 64 shapes
correct against hand-written pgx, green under `-race -shuffle=on` with 32
concurrent goroutines. Full numbers and caveats: `bench/RESULTS.md`.

Three findings that amend later milestones:

1. **Allocation budgets need a driver floor.** `pgx.Query` costs 5 allocs
   before an ORM acts. raorm adds **3** (the string copies); idiomatic pgx
   scanning adds **13**. `docs/PERFORMANCE.md` restated accordingly.
2. **"Allocations independent of row count" is impossible** — 3 text columns
   means 3 allocs/row. The real win is memory: 48 KB vs 185 KB per 1,000 rows.
3. **M2 must re-benchmark over a unix socket.** Against a container VM the
   round trip is 63.7 µs and raorm's whole CPU cost is ~45 ns, so the
   wall-clock gate passed without discriminating. **No wall-clock claim from
   M0 should be repeated as evidence.**

Rivals benchmarked (2026-08-24): **sqlc, Bun and GORM** are in `bench/`, with
sqlc genuinely generated rather than hand-written. Headline at 1,000 rows:
raorm **7 allocations** against sqlc 5,022, Bun 13,899, GORM 23,934 — and 22×
less memory than sqlc. **Ent is still missing** and needs its own codegen step.

### Original specification

The only milestone that matters until it passes.

**Build**, by hand, no generator: the runtime for *one* table, 8 columns, 6
optional filters. Shape mask, indexed shape cache, fragment splicing, pooled
buffers, a binary-protocol decoder, and the `Executor` port over pgx.

**Benchmark** against raw pgx, sqlc, Bun, Ent, GORM — same schema, same pool
config, same workload, `-count=10`, `benchstat`.

**Exit gate**
- single-row select ≤ 1.15× raw pgx wall time **and** ≤ 3 allocs/op
- warm dynamic shape: **0 allocs** for SQL construction, ≤ 200 ns overhead
- results committed to `bench/RESULTS.md`

**Kill criterion.** If the hand-written path cannot get within **1.30×** of raw
pgx after two honest optimisation passes, the thesis is false. Write the
negative result, publish it, close the repo, keep using sqlc with a hand-rolled
filter builder. *This outcome is a success — it costs two weeks instead of five
months, and the write-up is worth more than most shipped libraries.*

Note the spike is hand-written on purpose. It separates *"is the runtime design
fast?"* from *"can a generator emit it?"* — two failures that would otherwise be
indistinguishable.

## M1 — Model, schema IR, migration diff ✅ PASSED (2026-08-24)

**Shipped:** `raorm` (declaration API + struct walker), `schema` (the IR,
stdlib-only), `compile/pgddl` (DDL emission), `schema/pg` (introspection),
`migrate` (diff + normalisation), `cmd/raorm` (ddl · diff · verify · import).

**Gates met.** The round-trip is a *fixpoint*: model → DDL → apply → introspect
→ emit → apply → introspect produces identical IR. Migrations **converge** —
a plan generated against a live database leaves nothing pending once applied,
proven against real Postgres including an evolution step that drops a table.
Destructive steps are detected and annotated. Build output is byte-deterministic
across 20 runs. Nine declaration-error paths are asserted on message content.

**Four findings that changed the design:**

1. **`Schema` must take a pointer receiver.** A value receiver copies the struct,
   so `&u.Email` points into the copy and the offset is garbage. Measured, not
   assumed. Detected at build time with a message naming the fix; the docs said
   value receiver and were wrong.
2. **Expression text cannot be diffed.** Postgres rewrites everything it stores
   (`BETWEEN` → two comparisons, `'pending'` → `'pending'::status`,
   `lower(email)` → `lower((email)::text)`). String canonicalisation cannot fix
   this in general, so `migrate.For` runs the model through a **scratch
   namespace** and diffs catalog form against catalog form. This is the dev
   database ADR-0001 already assumed.
3. **`UNIQUE` constraints cannot hold expressions.** `Unique(Lower(&u.Email))`
   silently emitted DDL that does not parse; it now becomes a UNIQUE INDEX.
4. **One-to-one needs an owner.** `User.Profile *Profile` plus `Profile.User User`
   emitted two foreign keys and a cycle. Rule adopted: **the required side owns
   the key**; two equally-optional sides is a build error.

### Original specification

`model` — the declaration DSL (`s.Text`, `s.Enum[T]`, `s.HasMany`, `s.Mixin`,
`s.On(target, …)`). `schema` (stdlib only) — the IR: tables, columns, types and
OIDs, nullability, defaults, PKs, FKs, unique constraints, indexes, enums,
composite/array types. Three front ends: **model** (the source of truth),
`schema/pg` (live `pg_catalog`, for `import` and `verify --drift`), and
`schema/sqlfile`. Plus `migrate` — model ↔ schema diff emitting reviewable SQL.

**Exit gate**
- round-trip on a 40-table fixture: model → DDL → introspect → IR → diff empty
- `raorm import` against `anubis`'s real schema reproduces a model that
  round-trips to the same DDL; every type mapped or listed as explicitly
  unsupported — silence is not allowed
- `raorm verify --pending` fails on a model change with no migration; passes
  after `raorm migrate diff`
- destructive steps require `--allow-destructive` and are annotated in the SQL
- diff output is byte-deterministic and golden-tested

**Kill criterion.** If the diff engine turns into a project of its own, delegate
DDL emission to **Atlas** and keep only the model → IR front end. Losing the
in-house differ costs little; losing three months to it costs the plan.

## M2 — Query IR, compiler, read codegen (3 weeks)

Relational-algebra IR (select, from, join, where, group, having, window, CTE,
order, limit). Typed predicates via the specification pattern. Shape
enumeration and fragment lowering in `compile/`. Deterministic Go emission in
`codegen/`. `raorm generate` producing one package per bounded context.

**Status (2026-08-24): the read path is shipped and the codegen model is
proven.** `runtime` (shape cache, slab, decoders, executor port, pgx adapter),
`codegen` (Row, typed predicates, shape packing, fragment table, scanner,
terminals), generated code compiling and running against real Postgres.

Generated matches the hand-written M0 spike end to end — 2.36 ms vs 2.43 ms on
1,000 rows, 6 allocations vs 7 — and is byte-deterministic across regenerations.
The builder micro-path is 22.4 ns against the spike's 13.9 ns, both zero-alloc;
**the ±5% gate was mis-specified** and is restated below. Full detail and the
three wrong hypotheses in `bench/RESULTS.md`.

**Restated gate** (the ±5% micro-benchmark comparison is withdrawn):
- generated code allocates no more than the hand-written spike ✅ (6 vs 7)
- end-to-end within 5% of the spike ✅ (−3%)
- builder overhead under 100 ns absolute ✅ (22.4 ns)
- output byte-deterministic ✅ (5 regenerations, one hash)

**Added since:** composable typed predicates (`user.Email.Eq(v)` producing a
`Pred`, `Where(...)`, `WhereIf(...)`) — **zero allocations**, identical shape and
SQL to the chained form. Plus `Count` (which ignores `Limit`, because counting a
truncated set is a bug) and `Exists`. The generator is exercised against every
table in the fixture.

**Still outstanding for M2 — this is the larger half:**

| | why it matters |
|---|---|
| **joins** | needed for relational *expressiveness* — **not** for M3. Struck 2026-08-24: the `= ANY` batch already delivers the N+1 guarantee without a single join |
| `Or` / `Not` | needs an expression tree; one operator nibble per column cannot represent disjunction |
| CTEs as values, recursive CTEs | [[COMPLEX-QUERIES]] §2, §6 |
| window functions, `FILTER`, `GROUPING SETS` | §1, §5 |
| `UNION ALL` with a checked projection, lateral | §7, §8 |
| projections into a custom row type | every analytical query |

The read path proves the **compilation model** — that a generator can emit
allocation-free code matching a hand-written runtime. Expressiveness is a
separate and larger body of work, and the per-column nibble shape will have to
become a real expression tree to carry it.

**Original exit gate**
- the query IR covers what [[COMPLEX-QUERIES]] uses: CTEs as values, recursive
  CTEs with a mandatory depth bound and cycle guard, window functions over
  aggregates, `FILTER`, `GROUPING SETS`, correlated `EXISTS`/`NOT EXISTS`,
  `UNION ALL` with a checked common projection, `generate_series`, lateral joins
- **the M0 benchmark, reproduced by generated code, within 5% of the spike**
- generated output byte-identical across runs and machines; `gofmt` clean
- generated code for `anubis/authz`'s existing queries compiles and returns
  identical rows to the current sqlc output on a fixture database

**Kill criterion.** If generated code cannot match the spike within 5%, the
codegen model is wrong. Stop and fix it — do not add features on top of a
generator that emits slow code, because every later milestone inherits it.

## M3 — Relations without N+1 (3 weeks)

The differentiator, and the hardest milestone.

Fetch plans in the type system: `With()` returns a distinct generated projection
type. Loading strategies behind `internal/cost` — two-query `= ANY($1)` as the
default, `LATERAL` and `jsonb_agg` as measured alternatives.

**Exit gate**
- 1 parent + 50 children = **exactly 2 round trips**, asserted by the counting
  decorator over `Executor`, for every relation kind (has-one, has-many,
  belongs-to, m2m plain and with payload, self-referential) and every nesting
  depth in the fixture
- **polymorphic associations** ([[EXAMPLE]] §1.8): `ExclusiveArc` emits the
  exactly-one `CHECK` and per-variant partial indexes; `Discriminator` warns
  about absent referential integrity and requires `AcknowledgeNoFK`; all
  variants load in **one batched round trip**; `MatchSubject` breaks at compile
  time when a variant is added
- recursive traversal of a self-reference (`Descend`/`Ascend`) lowers to a
  single `WITH RECURSIVE` with a depth bound, proven not to hang on a cycle
- **greatest-n-per-group** ([[REFERENCE]] §4.9): `First`/`Last`/`Top(n)` are
  per-parent and still one round trip; `.Limit()` inside a relation is a
  generation error; an ordering that is not a strict total order is a
  generation error; all three lowerings (`DISTINCT ON`, `LATERAL`,
  `row_number()`) benchmarked and the default chosen by measurement
- reading an unloaded relation is a **compile error**, proven by a
  `testdata/compilefail` suite that asserts the build fails with the expected
  message
- strategy comparison published in `bench/RESULTS.md` — the default is whichever
  measured fastest, not whichever seemed obvious

**R3 is largely retired by named plans.** The earlier design generated a type
per `With(...)` combination — 2ⁿ per entity — and leaned on AST scanning of call
sites to keep it finite. Designing the API ([[API]] §7) produced a better answer:
**you name the plan**, in a `plans.go` you own, and the generator emits exactly
those types. No combinatorial explosion, no AST scanning, no chicken-and-egg
between generation and type-checking. Plans also become the one reviewable file
listing every load pattern in the system, and `raorm lint --plans` can cost each
one in round trips.

Ad-hoc inline `.With(...)` remains possible later via AST scanning, as a
convenience tier. It is explicitly **not** on the v1 path.

**Kill criterion / plan B.** If named plan types prove unusable in practice —
generics over "loaded or not" too painful, plan files too churny — fall back to
one loaded type per entity with a runtime-checked plan: reading an unloaded
relation panics in dev, returns a typed error in prod. Weaker than a compile
error, still better than Ent's silent empty slice.

## M4 — Writes, unit of work, batching (2 weeks)

Generated setters flipping a fixed-width dirty mask. `raorm.Unit` with
FK-ordered flush through `pgx.Batch`. `COPY` for bulk. `RETURNING`. Optimistic
locking on a version column. Upserts with `ON CONFLICT`.

**Exit gate**
- 1,000 inserts = one `COPY`; 1,000 mixed statements = one `pgx.Batch`
- a graph write orders correctly with deferred constraints **off** — proving
  ordering, not relying on Postgres to forgive it
- dirty-set computation: 0 allocs, asserted by `testing.AllocsPerRun`
- a concurrent-update test proves the version column rejects the stale writer

## M5 — Typed escape hatch (2 weeks)

`raorm.SQL[T]` raw queries validated at build time by `PREPARE` against a dev
database, with generated scanners. Raw fragments usable as join sources inside
typed queries.

**Exit gate**
- a real `anubis` analytical query — window function over a CTE with a lateral
  join — fully typed, zero `any`, scanned by generated code
- a query whose columns do not match `T` **fails generation**, with the
  Postgres error and the source location quoted
- offline mode works: a checked-in schema snapshot substitutes for the dev DB,
  so CI does not require a live Postgres

## M6 — First adopter: `anubis/authz` (3 weeks)

Same pattern as gwaf → gateon. Migrate exactly one bounded context, and pick the
hot one: `authz` carries the `authorize p95 < 2 ms` budget, so it will find
anything the benchmarks missed.

**Exit gate**
- authorize p95 **does not regress**; benchstat delta published
- `anubis` CI still green: no SQL strings in Go, import boundaries intact,
  domain still stdlib-only, ≤ 10 files per folder
- the migration diff is smaller than the sqlc code it replaced, or the DX claim
  is not real

**Kill criterion.** If migrating one context takes more than three weeks, or
regresses p95, raorm is not ready for the other seven. Freeze all feature work
and fix the adoption path — a library that is hard to adopt has no users, and
its own author is the first one to notice.

## M7 — Tooling gate and hardening (2 weeks)

`raorm explain` (EXPLAIN ANALYZE per named query, in CI), `raorm lint`
(seq-scan-over-threshold, shape-count explosion, unbounded relation load),
`raorm verify` (generated code matches schema — fails CI on stale output).

**Exit gate**
- the compiler survives a fuzz corpus over identifiers, types, and predicate
  trees
- **injection suite: no identifier reaches SQL text from a runtime value.** This
  must be a structural property — the placeholder count for every shape is known
  at build time, so a violation is a generation error, not a runtime check
- `go test -race -shuffle=on` green
- `raorm lint` fails a deliberately bad query in a fixture repo

## M8 — v0.1 (2 weeks)

Docs, runnable examples, API stability policy, semver commitments, and a
`MIGRATING-FROM-SQLC.md`. Ship only after M6 has run in `anubis` for two weeks.

---

## Production readiness — an honest assessment (2026-08-24)

What is genuinely solid, and what would stop a team adopting this tomorrow.
Written as a checklist because "production ready" is not one property.

### Solid

- **Injection is structural, not a filter.** The SQL is identical whatever the
  value, asserted directly — stronger than any payload corpus because it holds
  for payloads nobody thought of. Payloads round-trip as data; a rejection must
  be a *data* error (length, encoding), never a syntax error.
- **Fuzzed.** Three fuzzers, ~80M executions. One found a real defect in 20
  seconds: a malformed token stream silently dropped its `WHERE` clause — a
  filter failing *open*. Fixed to fail closed and loud, and CI fuzzes every PR.
- **Migrations converge**, asserted end to end against a live database:
  diff → apply → verify clean → diff again empty.
- **Round-trip counts are asserted**, not assumed: 2 for a relation, 1 per
  relation in a named plan, 1 for a bulk `COPY`, 1 for a batch, 1 for a
  recursive traversal, 1 batched for every arc variant.
- **`go test -race -shuffle=on` green**, with coverage floors enforced in CI.

### Blocking, in the order it would bite

1. **Type coverage.** ~~`numeric` and `jsonb` are unsupported~~ — **both
   shipped 2026-08-24.**

   `numeric` is `raorm.Decimal`: an exact fixed-point value, two machine words,
   no allocation, stdlib-only. float64 is deliberately not offered — it cannot
   represent 0.10, and an accounting system that rounds is a defect rather than
   a tolerance. The limit is stated rather than hidden: an int64 unscaled value
   carries 18 significant digits, a column declared past that is a **generation
   error**, and a value past it from the database is an **error rather than a
   wrong number**.

   `jsonb` decodes to `runtime.JSON` — raw bytes the caller unmarshals into a
   type it declared, because the generator cannot know a jsonb column's shape
   and decoding into `map[string]any` would allocate a map per row for callers
   who wanted a struct. It offers no value predicates: whole-document equality
   is almost never what a caller means, and content filtering needs `->>` and
   `@>`, which the operator set does not have.

   **`text[]` and `uuid[]` shipped 2026-08-24.** nil is SQL NULL and an empty
   non-nil slice is `'{}'` — different facts, kept distinct. A NULL *element*
   is `ErrArrayNull`, not `""`. The fuzzer found the decoder could be made to
   allocate gigabytes by a corrupt 17-byte count field before the fix; the
   allocation is now bounded by the input, never by a field inside it. The
   fixture no longer has any column the generator omits.

   **`date`, `interval`, `inet`/`cidr` and `int8[]` shipped 2026-08-24**, each
   with its honesty rule asserted: dates decode as midnight UTC (a stated
   convention); `raorm.Interval` keeps months/days/micros apart because a month
   has no length; inet and cidr are one `netip.Prefix` because the *database*
   polices the host-bits distinction; interval offers no equality because
   `'24:00' = '1 day'` surprises in both directions.

   **Still missing:** time-of-day (needs a pgx binding decision) and the
   remaining array element types (`numeric[]`). Neither blocks a typical
   schema.
2. **No adopter has run it.** M6 exists because the first adopter finds what
   benchmarks miss — and testing the CLI in one sitting found three defects,
   two able to lose data. That is the rate to expect, and it does not fall
   until someone runs it against a real workload.
3. **No release, no API stability policy** (M8). Nothing is tagged and nothing
   is promised.
4. ~~No `raorm lint`, no `raorm explain`.~~ **Both shipped 2026-08-24**, plus
   `verify -pending` (ADR-0001's third mode: "changed the model, no migration"
   as a CI failure that prints its own fix). lint budgets every named plan's
   round trips from the IR alone; explain plans every statement raorm will
   issue via GENERIC_PLAN — a validity gate on any database, a seq-scan gate
   only where statistics exist, and the doc says which is which. M7's tooling
   gate is closed; the fuzz corpus and injection suite closed earlier.
5. **Postgres only, and the dialect seam is unproven.** It exists and is
   CI-enforced, but a seam with one implementation is a hypothesis. M9 tests it.
6. ~~No transaction helper.~~ **`pgxdrv.Tx` shipped 2026-08-24.** The doctrine
   is now a capability: `Tx{T: tx}` runs every generated surface — queries,
   plans, COPY, a Unit flush — inside one pgx transaction unchanged, proven by
   a compose test that rolls all of it back. And the write path's key
   integrity property got its test rather than its argument: **a failed Unit
   flush is atomic** — a pgx batch runs between one Sync, PostgreSQL treats it
   as an implicit transaction, and the test plants a failing second statement
   and asserts the first left nothing behind.
7. **The wall-clock story is still unmeasured** over a unix socket — see the
   debts table above.

### Not blocking, worth knowing

- `runtime.SpliceTree` assumes a `$N` placeholder (documented at `takesArg`).
- Only `uuid[]` has a fast array encoder; `text[]` and `int8[]` keys still go
  through pgx's generic codec.
- Nesting a plan through a to-one relation is a generation error by choice.
- The discriminator form of polymorphism (`AnyRef`) is unbuilt; exclusive arcs
  are.

## Risk register

| # | Risk | Severity | Mitigation | Owner |
|---|---|---|---|---|
| R1 | **Thesis wrong** — compilation does not buy enough | fatal | M0 kill criterion answers it in 2 weeks for 2 weeks' cost | perf |
| R2 | Generated-code volume becomes Ent's disease | high | call-site-driven generation; one package per context; determinism; `raorm verify` | dx |
| R3 | Projection type explosion (M3) | ~~high~~ **low** | retired by **named plans** — you get only the plans you declare; no AST scanning needed | compiler |
| R4 | Build-time DB dependency for `PREPARE` | medium | checked-in schema snapshot; DB optional in CI | dba |
| R5 | Postgres-only until v1.1 costs early users | medium | accepted for v1; M9–M12 sequenced; capability model checks portability at build time | arch |
| R9 | **Dialect seam rots** while only one back end exercises it | high | M9 gate: no dialect conditional outside `compile/`, CI-enforced from M2 — not added at M9 | arch |
| R10 | Migration diff engine becomes its own project | medium | M1 kill criterion: delegate to Atlas | dba |
| R11 | Mongo's shape mismatch leaks into the SQL back ends | high | IR is a *logical plan* from M2, not a SQL AST; `OnDocument` is required, never inferred; M11 gates M12 | arch |
| R6 | pgx API churn | low | five-method `Executor` port; pgx confined to `runtime/pgxdrv` | arch |
| R7 | ORM maintenance burden is historically enormous | high | scope discipline: no *applied* DDL, no lazy loading, no runtime dialect branch, no UI. Every "no" is a year saved | arch |
| R8 | Author is the only user | medium | M6 first-adopter gate; publish the negative results too | dx |

## What "done" means for v1

Six sentences, all falsifiable:

1. A dynamic query with warm shape allocates nothing to build its SQL.
2. Reading an unloaded relation does not compile.
3. Any relation load is a bounded, asserted number of round trips.
4. Any SQL Postgres can run is expressible, typed, with a generated scanner.
5. The ORM fails CI on a query that regressed its plan.
6. raorm emits migrations and never applies one.
7. An unsupported construct on any configured target fails **generation**,
   naming the target and the source line.
