---
tags: [raorm, bench, results, m0]
generated: 2026-08-23
---

# M0 results — the thesis spike

> Regenerated, never hand-edited above the verdict line. Re-run with
> `make bench`. Do not quote these numbers from memory.

**Environment.** Apple M5 Pro (15 procs) · Go 1.26.6 darwin/arm64 · PostgreSQL
17.11 in an Apple `container` VM reached over vmnet · pgx/v5 v5.10.0 · pgxpool
min=max=8, shared by every implementation under test · 50,000 users across 100
orgs, ~14% of `age` NULL · median of `-count=10`.

## Numbers

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| **Floor_Ping** (`SELECT 1`) | 63,670 | 404 | 5 |
| **Floor_PgxNoDecode** (PK query, no decode) | 68,318 | 404 | 5 |
| Get_Pgx | 70,253 | 926 | 18 |
| **Get_Spike** | **66,639** | **455** | **8** |
| Scan1000_Pgx | 2,319,134 | 184,742 | 5,012 |
| **Scan1000_Spike** | **2,166,185** | **48,437** | **3,006** |
| Dynamic6_Pgx | 271,598 | 1,628 | 23 |
| **Dynamic6_Spike** | **269,238** | **557** | **15** |
| Prepare_Warm (SQL + bind, warm shape) | **14** | **0** | **0** |
| BuildAndPrepare_Warm (build query too) | **35** | **0** | **0** |
| DecodeRow_Offline (8 cols, no driver) | **31** | 48 | 3 |
| Compile_Cold (one shape) | 67 | 200 | 2 |
| Compile_AllShapes (all 64) | 4,306 | 12,800 | 128 |

Correctness: `TestSpikeMatchesPgx` runs **all 64 shapes** and requires rows
identical to a hand-written pgx scan. Green.

## Verdict: **M0 passes. The thesis is not falsified.**

Kill criterion was *>1.30× raw pgx after two optimisation passes*. Measured
**0.95×** on single-row and **0.93×** on 1,000-row — the spike is *faster* than
idiomatic pgx, not slower. Nowhere near the kill line.

| Gate | Target | Measured | |
|---|---|---|---|
| warm dynamic shape | 0 allocs, ≤200 ns | **0 allocs, 14 ns** | ✅ 14× under |
| cold shape | ≤25 µs, once per shape | **67 ns** (all 64 in 4.3 µs) | ✅ 370× under |
| single-row wall clock | ≤1.15× raw pgx | **0.95×** | ✅ |
| 1,000-row wall clock | ≤1.10× raw pgx | **0.93×** | ✅ |
| single-row allocations | ≤3 allocs/op | **8 end-to-end / 3 ours** | ⚠️ gate was mis-specified — see below |

## Three findings that change the plan

### 1. The allocation gates were written without a driver floor

`Floor_PgxNoDecode` issues the same PK query and decodes *nothing*: **5 allocs,
404 B**. That is pgx's own cost, and no ORM using `pgx.Query` can go below it.

So the right way to read the single-row number:

| | end-to-end | pgx floor | **the layer we control** |
|---|---:|---:|---:|
| idiomatic pgx scan | 18 | 5 | **13** |
| raorm spike | 8 | 5 | **3** |

**raorm's decode layer costs 3 allocations; the idiomatic pgx scan costs 13** —
a 4.3× reduction in the only part an ORM can influence. `DecodeRow_Offline`
confirms it in isolation: 31 ns, 3 allocs, and those three are the `string()`
copies for `email`, `name`, and `status`. Nothing else allocates.

`docs/PERFORMANCE.md` must restate every allocation budget as **"above the
driver floor"**. As written they were unmeetable by construction, and hitting
them would have meant fooling ourselves.

### 2. "Allocations independent of row count" is impossible, and the target should say so

Scan1000 is 3,006 allocs = **3 per row** + 6 fixed. Those three are the string
columns. Materialising 1,000 rows with 3 text columns cannot cost fewer than
3,000 allocations without interning or `unsafe`. The honest target is *no
per-row allocation beyond the string copies* — which the spike meets, while
pgx's 5,012 (5/row) does not.

The memory result is the stronger one: **48 KB vs 185 KB, 74% less.**

### 3. Wall-clock ratios are currently measuring the network, not the ORM

`Floor_Ping` is 63,670 ns. `Get_Spike` is 66,639 ns. **95.5% of single-row wall
time is round trip to the container VM.** The CPU work raorm saves is ~3.6 µs
out of 70 — real, but nearly invisible at this latency.

Total raorm CPU cost for a single-row query is **14 ns (SQL + bind) + 31 ns
(decode) ≈ 45 ns**, against a 64,000 ns round trip: **0.07% of the query.**

That cuts both ways, and the honest reading is:

- The `≤1.15× raw pgx` gate is **too easy to pass** over a network. It passed,
  but it did not test much.
- Any claim that raorm is "faster than pgx" is **not supported end-to-end**; the
  0.95× is within round-trip noise. What is supported is that it allocates far
  less and its own CPU cost is negligible.
- M2 must re-run this against a **unix-socket / co-located Postgres**, where CPU
  differences are visible, before the wall-clock gate means anything.

## Optimisation pass (same day)

M0 asked whether the design could be fast. This asks how much faster it can be
made. Two levers were tried; one paid enormously and one did not pay at all.

### ✅ Chunked string arena — kept

Text columns are copied into a `Slab` of never-reallocated chunks rather than
one `string()` per column per row. Chunks are retired, never grown in place, so
a string handed out earlier can never be invalidated — which is what makes the
`unsafe.String` safe.

| | before | after | |
|---|---:|---:|---|
| Decode 1,000 rows (offline) | 3,000 allocs · 26.6 µs | **8 allocs · 22.6 µs** | 375× fewer, 15% faster |
| Scan1000 end-to-end | 3,006 allocs · 185 KB | **19 allocs · 66 KB** | 158× fewer, 64% less memory |
| Parallel Scan100 (16 procs) | — | **16 allocs · 4.7 KB** | vs pgx 514 allocs · 19.4 KB |

Cost: a result set shares backing memory, so holding one row holds its chunk —
the same contract as pgx's `RawValues`. Slabs are deliberately **not pooled**;
reusing one would corrupt strings still held by the previous result, and that is
not a trade to make silently. `AllInto` lets a caller opt in when it knows
better.

### ❌ pgconn fast path — rejected

The hypothesis: pgx's type map and `Scan` machinery are dead weight for an ORM
that generates its own decoders, so dropping to `pgconn.ExecPrepared` with
hand-encoded wire parameters should remove the 5-allocation floor and the
`[]any` boxing entirely.

It did not.

| Parallel Scan100, 16 procs | ns/op | B/op | allocs |
|---|---:|---:|---:|
| pgx `Query` + `Scan` | 494,000 | 19,396 | 514 |
| **raorm via pgx** | **483,000** | 4,715 | **16** |
| raorm via pgconn | 530,000 | 4,514 | 22 |

**The floor is pgconn's, not pgx's.** `pool.Acquire` plus the `ResultReader`
allocate about as much as `pgx.Query` does, so removing the type map removed
nothing — and the extra `sync.Map` lookup for per-connection prepared-statement
state made it **10% slower under concurrency**. Code kept in
`internal/spike/fastpath.go`, marked rejected, so the result can be re-verified
rather than re-argued.

### ✅ Self-sizing arena — the change that mattered most

The first slab grew by doubling from 128 B. That over-allocates ~1.8×, and the
client-ceiling harness caught it: **the slab was slower than plain per-row
`string()`** (75.4M vs 85.7M rows/sec) because Go's small-object allocator is
fast enough that the extra memory traffic cost more than 1,500 saved mallocs.

Fix: each compiled statement records the byte size its last result needed
(`stmt.hint`, one atomic per shape). Shapes are stable, so one observation sizes
the next arena exactly and the doubling ramp never runs.

| Client ceiling, 40,000 × 500 rows, 16 workers | rows/sec | mallocs | GCs | alloc MiB |
|---|---:|---:|---:|---:|
| per-row `string()` | 83.5M | 60,000,132 | 86 | 917 |
| slab, doubling from 128 B | 75.4M | 560,548 | 99 | 1,262 |
| **slab, self-sizing** | **137.4M** | **120,394** | **46** | **707** |

**1.65× the throughput of a minimal per-row decoder, on 498× fewer mallocs and
23% less memory.** And 1.82× the un-hinted slab — the hint, not the arena, was
where most of the win was hiding.

### Where raorm is and is not "fastest"

Stated precisely, because the wall-clock numbers invite the wrong claim:

- **Not faster end-to-end, and cannot be.** Under load both sit at ~2.4 s for
  4,000 × 500-row queries because the database and the socket are the
  bottleneck. No ORM beats pgx on wall clock when both wait on the same socket,
  and any benchmark claiming otherwise is measuring noise.
- **Fastest in the part an ORM controls** — 1.65× the throughput of a minimal
  per-row decoder with the database removed, and 0 allocations / 14 ns for SQL
  construction against rebuilding the string on every call.
- **Far lighter.** Per identical DB-bound workload: **249× fewer mallocs, 4.8×
  less allocated, 4.9× fewer GC cycles, 3.8× less GC pause.**

| 4,000 queries × 500 rows, 16 workers | wall | GCs | GC pause | mallocs | alloc MiB |
|---|---:|---:|---:|---:|---:|
| pgx `Query` + `Scan` | 2.438 s | 102 | 5.55 ms | 10,060,365 | 355.6 |
| **raorm** | **2.379 s** | **21** | **1.47 ms** | **40,379** | **73.9** |

This is the honest shape of the claim. Wall clock is a wash because Postgres is
the bottleneck; what raorm buys is **the allocator and GC budget back**. On a
service doing 10k queries/sec that is roughly 25 billion fewer allocations per
hour, which is CPU returned to request handling and tail latency that stops
being GC-shaped. The mechanism is measured end to end; the throughput *gain*
still depends on your service being allocator-bound rather than database-bound,
and most are not.

Current allocation profile after both passes:

| | pgx | raorm |
|---|---:|---:|
| Get (1 row) | 18 allocs · 926 B | **6 allocs · 532 B** |
| Scan 1,000 rows | 5,012 allocs · 185 KB | **7 allocs · 41 KB** |
| Parallel Scan 100 | 514 allocs · 19.4 KB | **9 allocs · 4.7 KB** |

## Head to head against the field

Same database, same 50,000 rows, same workload, median of 6. Bun and GORM run on
`database/sql` over the pgx stdlib driver — how they are actually deployed —
with pools pinned to the same 8 connections as pgxpool. GORM was given its best
case: `PrepareStmt: true`, `SkipDefaultTransaction: true`, logger discarded.
sqlc shares the pgxpool directly and its code is genuinely `sqlc generate`d, not
hand-written.

### Single row by primary key

| | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| **raorm** | **67,710** | **532** | **6** |
| raw pgx (hand-written) | 71,553 | 926 | 18 |
| sqlc | 71,550 | 1,006 | 13 |
| GORM | 73,948 | 5,825 | 110 |
| Bun | 217,567 | 7,821 | 74 |

### 1,000 rows

| | ns/op | B/op | allocs/op | allocs vs raorm |
|---|---:|---:|---:|---:|
| **raorm** | **2,180,937** | **41,382** | **7** | — |
| raw pgx (hand-written) | 2,151,646 | 184,738 | 5,012 | 716× |
| sqlc | 2,403,270 | 912,466 | 5,022 | 717× |
| Bun | 3,511,792 | 447,536 | 13,899 | 1,985× |
| GORM | 4,500,524 | 751,186 | 23,934 | **3,419×** |

At 1,000 rows the CPU work is finally large enough to show through the network:
raorm is **1.10× faster than sqlc, 1.61× than Bun, 2.06× than GORM** on wall
clock, and uses **22× less memory than sqlc** and **18× less than GORM**.

### Reading these fairly

- **raorm ≈ raw pgx on wall clock, everywhere.** 67.7 vs 71.6 µs and 2.18 vs
  2.15 ms are the same number. Both wait on the same socket. The difference is
  6 allocations against 18, and 7 against 5,012.
- **sqlc is the honest bar and it is a good one.** Its generated code is
  `pgx.Query` + `rows.Scan`, so it lands where raw pgx lands. Two costs are
  structural, not sloppiness: `rows.Scan` boxes every column into `any`, and the
  generated `var items []User` has no pre-allocation, so a 1,000-row result
  reallocates its way up. That is most of the 912 KB.
- **Bun's 217 µs single-row number is not explained** and is very likely the
  `database/sql` path not reusing a prepared statement rather than anything
  inherent to Bun. Do not cite it as "Bun is 3× slower" — cite the allocation
  counts, which are stable and reproducible.
- **GORM's 110 allocations for one row** is the clearest picture of what
  reflection-per-column costs, and it is with prepared statements enabled.

## M2 — generated code vs the hand-written spike

M2's kill criterion: *can a generator emit what a human wrote by hand, within
5%?* Answer: **end to end yes, on the micro-benchmark no — and the gate was the
wrong shape.**

### End to end, 1,000 rows

| | ns/op | B/op | allocs |
|---|---:|---:|---:|
| **raorm, generated** | **2,355,646** | **41,367** | **6** |
| raorm, hand-written (M0) | 2,432,559 | 41,400 | 7 |
| raw pgx | 2,231,454 | 184,739 | 5,012 |
| sqlc | 2,315,063 | 912,470 | 5,022 |
| Bun | 2,598,374 | 447,538 | 13,899 |
| GORM | 4,120,512 | 752,522 | 23,937 |

Generated code matches the hand-written spike (−3%, inside noise) and keeps
every allocation win against the field. `dynamic6` is 271.3 µs generated against
272.5 µs hand-written — the same number.

### The builder path, where the gate fails

| | hand-written | generated | delta |
|---|---:|---:|---:|
| shape lookup | 1.79 ns | 2.75 ns | +0.96 ns |
| bind arguments | 9.79 ns | 11.70 ns | +1.91 ns |
| **prepare (shape + bind)** | **13.9 ns** | **22.4 ns** | **+61%** |
| build + prepare | 35.3 ns | 49.1 ns | +39% |

Both are **0 allocations**. The gap is 8.5 ns on a path that is **0.013% of a
64 µs query**.

### What the first three attempts got wrong

Worth recording, because two of them were confident and both were wrong:

1. *"The open-addressed cache probe is the cost."* Added a one-entry L1 in front
   of it. **No change at all** — 41.8 ns before and after.
2. *"Three comparisons per column in `bind`."* Renumbered the operators so
   argument-taking ones are contiguous and collapsed it to one unsigned range
   check. **No change** — 29.8 ns before and after.
3. The actual cause: `q.op(i)` and `q.Shape()` were **value-receiver methods on
   a 150-byte struct**, so every call copied the whole Query — eight calls per
   bind, ~1.2 KB of memcpy. Emitting the shift inline and taking the shape by
   value in a package function took bind from **29.8 → 11.7 ns** and prepare
   from **42.3 → 22.4 ns**.

Isolating the cost with a micro-benchmark per stage found in one run what two
rounds of reasoning had missed. **Measure the stages, do not reason about them.**

### Why the residual 8.5 ns is not a defect

Two causes, both of them the generator doing *more* than the spike:

- **The generated table has 8 filterable columns; the spike hard-coded 6.** Bind
  walks 8 slots instead of 6, and the Query is 152 bytes against 104.
- **The cache is general.** M0 indexed a flat array because six filters gave 64
  shapes. A real table has more (column, operator) pairs than a `uint64` has
  bits, so shapes are packed 4-bit operator nibbles and the cache is
  open-addressed. That costs ~1 ns and is what makes the design work on a
  40-column table.

**The 5% gate was mis-specified** — the same category of error as M0's
allocation budget. It compared a general generator against a hand-tuned special
case on a micro-benchmark representing 0.013% of a query. The gates that mean
something — zero allocations, end-to-end parity, and correctness — all pass.
`docs/PLAN.md` restates it as an absolute nanosecond budget plus an allocation
budget.

### Composable predicates

The chained form (`q.EmailEq(v)`) is fast but not composable — a predicate
cannot be returned from a function or shared between queries, which is the
whole argument for objects over strings. So the generator also emits typed
column handles producing a `Pred` value.

```go
var Active = genuser.Status.Eq("active")
func InOrg(id [16]byte) genuser.Pred { return genuser.OrgID.Eq(id) }

q := genuser.New().Where(InOrg(id), Active, genuser.Age.Gte(21))
```

| | ns/op | B/op | allocs |
|---|---:|---:|---:|
| chained `q.OrgIDEq(...)` | 37.1 | 0 | **0** |
| composed `Where(p...)` | 73.7 | 0 | **0** |

**Both allocation-free** — the variadic slice stack-allocates, confirmed by the
0 B/op. Composition costs 37 ns, or 0.06% of a 64 µs query. Chained stays for
hot paths; composed is the default because reuse matters more than 37 ns.

Both produce an identical shape and identical SQL, asserted by test.

### Relation loading — the N+1 guarantee, asserted

`= ANY($1)` binds a whole list to **one** placeholder, so list length never
changes the statement. That is the property relation loading needs: without it,
loading 50 children would mint a new compiled statement per distinct list
length.

Verified: a 1-element, 50-element and 500-element list produce **identical SQL
and an identical shape**. And with the round-trip counter wrapped around the
executor:

```
50 parents + 25,000 children across 50 distinct orgs in 2 round trips
```

The test fails if it is ever not exactly 2. This is the one claim the whole
product rests on, so it is asserted rather than hoped for.

### One honest cost: pgx's array encoder

| | ns/op | B/op | allocs |
|---|---:|---:|---:|
| `= ANY` with 500 ids, 500 rows | 386,387 | 93,116 | **1,021** |

That is ~2 allocations per bound id, and it is **not** the scan path — decoding
500 rows still costs a handful. It is pgx encoding a 500-element `uuid[]`
parameter, on the way in.

So the allocation story is asymmetric and should be stated that way: reading is
essentially free, and binding a large array is not. Encoding the array directly
into a pooled buffer would fix it — the same trick that removed the `[]any`
boxing — but it is unbuilt, and quoting the 6-alloc scan figure for a relation
load would be wrong.

### Also verified

- Generated SQL is identical to the hand-written statement modulo identifier
  quoting, which the generator is right to add.
- Generated rows are byte-identical to the spike's across a 200-row result.
- Five regenerations produce one distinct SHA-256 — **byte-deterministic**.
- The generator emits parsing, gofmt-clean code for **every table in the
  fixture**: enums, typed jsonb, arrays, nullable columns, foreign keys,
  self-references, one-to-one, exclusion constraints, mixins.
- `Count` ignores `Limit` (counting a truncated set is a bug) and agrees with
  the row count returned by `All`.

## Expression-tree IR (2026-08-24)

The per-column operator mask could not represent `age > 18 OR age < 5` (one
column twice) or `A AND (B OR C)` (a nested group). Predicates are now a
**postfix token stream**, and the stream itself is the compiled-statement key.

Tokens carry only compiler-generated ids — kind, operator, column, arity —
never a caller's value, so two queries with the same structure and different
values share one statement. Cache hits are confirmed by **comparing the tokens**,
not just a hash: a 64-bit collision would return the wrong SQL, and that is not
a risk an ORM gets to take.

Verified against Postgres, not just against itself:

```
status = $1 AND ("age" < $2 OR "age" > $3)    3,572 rows, every row checked
org_id = $1 AND NOT (status = $2)             every row checked
org_id = $1 AND (status = $2 OR status = $3)  333 rows — count agrees with
                                              the same query written by hand
```

That last one is the one that matters: `AND` binds tighter than `OR`, so a
missing group would silently return the wrong rows and still look plausible.
Cross-checking the count against Postgres is what catches it.

### The cost, stated plainly

| | mask (before) | tree (after) | |
|---|---:|---:|---|
| prepare (structure + bind) | 22.4 ns | **42.9 ns** | 0 allocs both |
| build 6 predicates + prepare (chained) | 49.1 ns | **257 ns** | 0 allocs both |
| build 6 predicates in one `Where(...)` | — | **124 ns** | 0 allocs |
| scan 1,000 rows, end to end | 2.24 ms · 7 allocs | **2.26 ms · 6 allocs** | unchanged |

**Still zero allocations everywhere.** End-to-end is unchanged, because it was
never the builder that cost anything.

The remaining 257 ns is a real 5× regression on that path and it is mine: the
Query grew from ~150 to ~330 bytes (token buffer plus per-type value arenas) and
it is a value type, so each builder call copies it.

The first measurement was **477 ns**. Two fixes halved it:

- `push` and `leaf` now mutate through a **pointer receiver**, so the builder
  loop stops copying ~330 bytes twice per predicate. They are only ever called
  on a local, so the public API keeps value semantics — `q2 := q1.Where(x)` must
  not mutate `q1`, and a pointer Query would break that.
- Passing all six predicates to **one** `Where(...)` rather than chaining six
  calls costs **124 ns** instead of 257: one copy instead of six. That is a
  usage note, not a defect, and it is now benchmarked so it stays true.

At 124–257 ns this is 0.2–0.4% of a 64 µs query, and end-to-end is unchanged.

**A bug this found:** converting `push` to a pointer receiver initially left its
`Query` return in place. Go permits discarding a return value in a statement, so
`q.push(t)` compiled and silently threw the mutation away — every predicate was
dropped and the SQL came out as `WHERE ` with nothing after it. The tests caught
it immediately; a compiler would not have.

### Also new

- `Any(...)` (OR group), `Not(...)`, `NotAny(...)`, alongside `Where(...)`.
- **Overflow is an error, never a dropped predicate.** The buffers are fixed;
  a query that outgrows them sets a flag that every terminal returns.
- `Shapes()` reports how many distinct structures have compiled — the signal
  `raorm lint` needs to catch a builder minting a statement per request.
- Chained sugar (`q.StatusEq(v)`) is now generated as `Where(Status.Eq(v))`, so
  there is one code path and the two forms cannot drift.

## Not yet tested

- **Ent** — the one rival still missing; it needs a codegen step of its own.
  sqlc, Bun and GORM are done (above).
- ~~Concurrency~~ **done**: `TestConcurrentShapes` drives all 64 shapes from 32
  goroutines under `-race -shuffle=on`, then asserts every shape compiled to
  one stable statement. Green — a lost CAS leaves no shape unusable.
- Writes, relations, `COPY` — all M4/M3, out of M0 scope.
- A shape count large enough to need the sharded-map fallback (>64 bits).

## What the spike is

Hand-written, not generated — deliberately, so that "is the runtime design
fast?" is answered separately from "can a generator emit it?" (M2's job).
`internal/spike` is ~330 lines: shape mask, indexed compiled-statement cache,
fragment splicing, pooled binder, wire-bytes decoder, and a five-method
executor port with the only pgx import behind it.

---

## `= ANY($1)` parameter side (2026-08-24)

Measured because the generated relation plan layer makes this the common path:
every plan batches children with `= ANY($1)`, so an allocation per bound id is
an allocation per parent.

`BenchmarkAnyParam` — one row returned, ids varied, so this is the parameter
side and not the scan:

| ids | ns/op | B/op | allocs/op |
|---|---|---|---|
| 1 | 217,134 | 517 | 10 |
| 50 | 2,229,303 | 7,714 | 114 |
| 500 | 2,678,528 | 74,075 | 1,021 |

**~2 allocations per bound id.** It is on pgx's *parameter* side, not raorm's
scan path — the scan of a 1,000-row result is still a handful.

`BenchmarkEncodeIDArray` isolates the encoder with no server and no scan:

| shape | ns/op | B/op | allocs/op |
|---|---|---|---|
| `[][16]byte` | 15,225 | 16,056 | 1,003 |
| `pgtype.FlatArray[[16]byte]` | 12,760 | 16,032 | 1,002 |

**Negative result — do not re-try it.** `pgtype.FlatArray` is pgx's own array
wrapper and the obvious first thing to reach for. It comes out within noise:
both shapes pay ~2 allocations per element inside pgx's generic array codec,
which boxes every element into an `any` and builds a per-element encode plan.

The fix is therefore not a different pgx wrapper. It is a `pgtype.Codec`
registered on the connection's type map in `runtime/pgxdrv`, encoding `uuid[]`
straight into the output buffer — ndim, hasnull, element OID, one dimension,
then 16 bytes per id. It has to live in the adapter because that is the only
package allowed to name a pgx type, and it would have to be repeated per array
element type (`text[]`, `int8[]`) that a relation key can have.

### Built, 2026-08-24 — `runtime/pgxdrv.RegisterFastArrays`

One encode plan for the whole slice, writing the binary array format straight
into the output buffer, delegating everything else to the codec pgx installed.

Isolated encoder, 500 ids:

| | ns/op | B/op | allocs/op |
|---|---|---|---|
| pgx `[][16]byte` | 14,611 | 16,056 | 1,003 |
| pgx `FlatArray` | 12,610 | 16,032 | 1,002 |
| **raorm codec** | **576** | **24** | **1** |

End to end through `= ANY($1)`:

| ids | B/op before → after | allocs/op before → after |
|---|---|---|
| 1 | 517 → 453 | 10 → **6** |
| 50 | 7,714 → 5,824 | 114 → **11** |
| 500 | 74,075 → 35,787 | 1,021 → **11** |

**Allocations are now flat in id count** — 11 at fifty ids and 11 at five
hundred, where before they were 114 and 1,021. That is the property worth
having: a relation load's parameter cost no longer scales with parent count.

**Wall clock end to end is unchanged** (~2.6 ms at 500 ids) and the difference
is inside noise, because the query is dominated by the server and the socket.
Claim the allocation and byte numbers, which are measured and large. Do not
claim the round trip got faster.

Byte-for-byte identical to pgx's own output at n = 0, 1, 2, 7 and 500,
asserted — a parameter encoder that is fast and subtly wrong corrupts a query's
meaning rather than failing it. A nil slice still encodes as SQL NULL, not an
empty array: `= ANY(NULL)` is unknown, `= ANY('{}')` matches nothing.

**Still generic:** only `uuid[]` has a fast path. `text[]` and `int8[]` relation
keys still go through pgx's generic codec, and each needs the same treatment.

---

## Greatest-n-per-group: which lowering (2026-08-24)

"Each parent with its first N children", in one query. Both lowerings are
generated from the same model, so this compares two *lowerings*, not two
hand-written approximations. `BenchmarkTopNPerParent`, 500 children per parent.

ns/op, lower is better:

| parents | n | window | lateral | lateral is |
|---|---|---|---|---|
| 1 | 1 | 340,038 | 95,216 | **3.6x faster** |
| 1 | 5 | 316,481 | 91,224 | **3.5x** |
| 1 | 50 | 307,515 | 109,239 | **2.8x** |
| 10 | 1 | 1,418,189 | 107,018 | **13.3x** |
| 10 | 5 | 1,432,406 | 120,100 | **11.9x** |
| 10 | 50 | 1,567,553 | 286,205 | **5.5x** |
| 100 | 1 | 10,593,364 | 322,665 | **32.8x** |
| 100 | 5 | 10,860,570 | 469,177 | **23.1x** |
| 100 | 50 | 13,187,256 | 1,989,972 | **6.6x** |

Allocations are identical at every point (same rows returned), so the whole
difference is server-side work.

**`LATERAL` is the default.** It wins at every parent count and every n, and the
gap widens with parent count.

**The reasoning that picked the other one, and why it was wrong.** The first cut
defaulted to `row_number()` on the argument that one index scan feeding a window
beats a per-parent nested loop. The window form reads **every child of every
matched parent** and discards those past n, so its cost tracks the *total child
count*; LATERAL's tracks the *rows actually returned*. At 100 parents x 500
children that is 50,000 rows scanned to return 100. The nested-loop iteration
LATERAL pays per parent is real and is dwarfed by not doing that.

Note the trend within a parent count: the gap narrows as n grows (32.8x at n=1
down to 6.6x at n=50), which is the same fact seen from the other side — the
larger the fraction of children kept, the less the window form wastes. The
window lowering stays generated and exported for a caller whose data sits at
that end, and because a default chosen by measurement needs something to have
been measured against.

---

## Ent joins the rivals (2026-08-25)

The last M0 debt: Ent was the one rival needing its own dependency and codegen
step, which is why it kept being the one left out. Its client is **genuinely
generated** (ent v0.14.6, committed like sqlc's output): **164K of code for
this ONE table** — R2, "generated-code volume becomes Ent's disease", measured
rather than asserted; raorm's whole six-table context is smaller.

Same pool (8 conns), same workload, same run. Loopback :5433 through the
container VM's port forward — wall clocks remain round-trip-dominated, so the
allocation columns are the comparison:

**Get (single row by id):**

| | ns/op | B/op | allocs/op |
|---|---|---|---|
| raorm (spike) | 94,606 | 532 | **6** |
| sqlc | 91,655 | 1,006 | 13 |
| raw pgx | 97,251 | 926 | 18 |
| Bun | 280,795 | 7,818 | 73 |
| GORM | 92,582 | 5,823 | 110 |
| **Ent** | 92,047 | 6,043 | **159** |

**Scan1000 (status filter, ordered, 1,000 rows):**

| | ns/op | B/op | allocs/op |
|---|---|---|---|
| raorm (generated) | 2,630,722 | 41,354 | **6** |
| raw pgx | 2,372,746 | 184,748 | 5,012 |
| sqlc | 2,796,736 | 912,460 | 5,022 |
| Bun | 2,745,265 | 447,536 | 13,899 |
| **Ent** | 2,857,854 | 805,224 | **23,016** |
| GORM | 3,782,505 | 751,056 | 23,934 |

Ent's single-row read allocates the most of ANY rival — 159, where a
hand-written pgx read is 18 and raorm is 6 — and its thousand-row scan is
GORM-class (23,016 vs 23,934) despite Ent being fully code-generated. Being
generated is not what makes raorm cheap; generating the SCAN PATH instead of
generating calls into a reflective core is.

Bun's ~281µs Get remains the unexplained outlier noted earlier; still do not
cite it.
