# raorm — M0 spike result (2026-08-23)

**M0 PASSED. The thesis is not falsified.** Numbers live in `bench/RESULTS.md` —
re-run `make bench`, never quote from memory.

Headline: **0.95×** raw pgx single-row, **0.93×** on 1,000 rows, **0 allocations
and 14 ns** on the warm dynamic path (target was 0 allocs / ≤200 ns), all 64
shapes byte-identical to hand-written pgx, green under `-race -shuffle=on` with
32 concurrent goroutines. Kill line was 1.30×; never approached.

## The three findings that matter more than the headline

1. **Allocation budgets were written without a driver floor.** `pgx.Query`
   costs **5 allocs / 404 B** before an ORM does anything (`Floor_PgxNoDecode`).
   So end-to-end: raorm 8, idiomatic pgx 18 → **raorm's own layer costs 3, pgx's
   scan costs 13**. The three are the unavoidable `string()` copies.
   `docs/PERFORMANCE.md` now states every allocation budget *above the floor*.
2. **"Allocations independent of row count" is impossible.** 3 text columns = 3
   allocs/row, always. The real win is memory: **48 KB vs 185 KB** per 1,000
   rows (74% less).
3. **Wall-clock ratios measured the network, not the ORM.** Round trip to the
   Apple-container Postgres is 63.7 µs; raorm's entire CPU cost is ~45 ns
   (14 ns SQL+bind, 31 ns decode) = **0.07% of the query**. The ≤1.15× gate
   passed without discriminating. **Do not repeat the 0.95× as evidence
   raorm is faster than pgx** — it is inside round-trip noise. M2 must
   re-benchmark over a unix socket.

## Still owed from M0 scope
sqlc / Bun / GORM / Ent comparisons — only raw pgx exists. **sqlc is the one that
matters**, since it is what `anubis` uses today.

## Where the code is
`internal/spike/` (~350 lines, hand-written *not* generated, so "is the runtime
design fast?" is answered separately from "can a generator emit it?" — M2's job):
`shape.go` mask + indexed `atomic.Pointer` cache + fragment splicing,
`query.go` value-type builder + pooled `Binder`, `row.go` wire-bytes decoder,
`pgxdrv.go` the only pgx import.
`bench/` floor / spike / pgx / correctness / concurrency + `RESULTS.md`.
`make db` starts Postgres in an Apple container and derives the DSN.

## Design detail worth keeping
The pooled `Binder` holds the values **and** the `[]any`, with each interface
entry pointing at the Binder's own field. Boxing a pointer does not allocate, so
argument binding is free. An earlier version returned a `release func()` closure
— that closure was the *only* allocation on the warm path (16 B). Removing it
took warm Prepare from 1 alloc / 24 ns to **0 allocs / 14 ns**.

See [[core]], [[decisions]].

## Optimisation pass + rivals (2026-08-24)

**Two levers tried. One paid enormously, one was rejected.**

- ✅ **Chunked string arena (`Slab`) with a per-shape size hint.** Text columns
  copy into never-reallocated chunks; `stmt.hint` (one atomic per shape) records
  the bytes the last result needed, so the next arena is sized exactly and the
  doubling ramp never runs. **The hint was where most of the win was**: without
  it the slab was *slower* than plain per-row `string()` (75.4M vs 85.7M
  rows/sec) because doubling over-allocates ~1.8x and Go's small-object
  allocator is fast. With it: **137.4M rows/sec, 1.65x a minimal per-row
  decoder, 498x fewer mallocs.**
- ❌ **pgconn fast path — rejected.** Hypothesis was that pgx's type map is dead
  weight for a codegen ORM. Wrong: **the 5-alloc floor is pgconn's, not pgx's.**
  Bypassing bought ~4% fewer bytes, MORE allocations, and was 10% slower under
  concurrency. Code kept in `internal/spike/fastpath.go` marked rejected.

**GC pressure, 4,000 queries x 500 rows, 16 workers:** pgx 102 GCs / 5.55 ms
pause / 10,060,365 mallocs / 355.6 MiB → raorm **21 GCs / 1.47 ms / 40,379 /
73.9 MiB**. Wall clock identical (2.44 s vs 2.38 s) — Postgres is the
bottleneck.

**Head to head at 1,000 rows (allocs):** raorm **7** · raw pgx 5,012 · sqlc
5,022 · Bun 13,899 · GORM 23,934. Memory: raorm 41 KB vs sqlc 912 KB.
Wall clock: raorm 1.10x faster than sqlc, 1.61x Bun, 2.06x GORM.

## The claim to make, and the one not to
**Do not claim raorm is faster than pgx end-to-end.** It is not and cannot be —
both wait on the same socket, and every single-row wall-clock difference is
inside noise. Claim the **allocation and GC numbers**, which are measured and
large. The throughput consequence follows mechanically but only materialises if
a service is allocator-bound rather than database-bound, and most are not.

Bun's 217 us single-row number is **unexplained** (probably `database/sql` not
reusing a prepared statement). Do not cite it. Ent is still un-benchmarked.

## M1 shipped (2026-08-24)

`raorm` (declaration API + struct walker) · `schema` (IR, stdlib-only) ·
`compile/pgddl` · `schema/pg` (introspection) · `migrate` (diff + normalise) ·
`cmd/raorm`. Gates: round-trip is a **fixpoint**, migrations **converge** on a
live database, destructive steps flagged, build byte-deterministic, 9 error
paths asserted on message content.

**Four hard-won facts — do not re-derive:**
1. **`Schema(t *raorm.Table)` needs a POINTER receiver.** A value receiver copies
   the struct; `&u.Email` then points into the copy and the offset is garbage
   (verified empirically). Detected at build time.
2. **Never diff expression text.** Postgres rewrites everything it stores
   (BETWEEN → two comparisons, `'x'` → `'x'::enum`, `lower(email)` →
   `lower((email)::text)`). `migrate.For` applies the model to a scratch
   namespace and diffs catalog-form vs catalog-form. `stripCasts` in canonical()
   is only a cheap second line of defence for offline snapshot diffs.
3. **UNIQUE constraints cannot hold expressions** — `Unique(Lower(&u.Email))`
   must become a UNIQUE INDEX or the DDL does not parse.
4. **One-to-one needs an owner: the required side owns the key.** `User.Profile
   *Profile` + `Profile.User User` otherwise emits two FKs and a cycle. Two
   equally-optional sides is a build error.

Also: exclusion constraints need `btree_gist` when mixing `=` on a scalar with
`&&` on a range; enum names must not be pluralised (they are types, not tables);
FK auto-indexes must be added *after* user declarations so a leading user index
suppresses the redundant one.

## M2 read path shipped (2026-08-24)

`runtime` (shape cache + slab + decoders + executor port + pgxdrv) and `codegen`
(Row, typed predicates, shape packing, fragment table, scanner, terminals).
Generated code runs against real Postgres and is byte-deterministic.

**Result: generated ≈ hand-written spike end to end** — 2.36 ms vs 2.43 ms on
1,000 rows, **6 allocations vs 7**. vs the field at 1,000 rows: raw pgx 5,012
allocs, sqlc 5,022, Bun 13,899, GORM 23,937.

**The ±5% gate was mis-specified** (same error class as M0's allocation budget):
it compared a general generator against a hand-tuned special case on a
micro-benchmark worth 0.013% of a query. Restated in PLAN.md as allocation
parity + end-to-end parity + an absolute ns budget. All pass.

**The debugging lesson — do not repeat it.** Two confident hypotheses were both
wrong (the cache probe; the comparison count in bind); a one-entry L1 and an
operator renumbering each changed *nothing*. The real cause was
**value-receiver methods on a ~150-byte Query struct**: `q.op(i)` copied the
whole struct eight times per bind. Emitting the shift inline and taking the
shape by value in a package function: bind 29.8 → 11.7 ns, prepare 42.3 → 22.4
ns. **Isolate per-stage micro-benchmarks before theorising.**

Design notes worth keeping: shape = 4-bit operator nibble per column packed into
a uint64 (M0's flat 64-entry array does not generalise); argument-taking
operators are numbered first so `op-1 < opsWithArg` is one unsigned compare;
`Stmt.slabHint` sizes the next arena exactly.

**Outstanding for M2:** the query IR proper — joins, CTEs, windows, FILTER,
GROUPING SETS, EXISTS, UNION. The read path proves the compilation model only.

### M2 addendum: composable predicates + terminals (2026-08-24)
Typed column handles emit a `Pred` value (union payload, no interface), applied
by a generated switch that writes the correctly-typed Query field — so Query
stays small and nothing is boxed. `Where(p...)` and `WhereIf(cond, p)` are
**zero-allocation** (73.7ns composed vs 37.1ns chained; the variadic slice
stack-allocates). Identical shape and SQL to the chained form, asserted.
`Count` ignores `Limit` by design. Generator verified against every fixture
table (enums, jsonb, arrays, FKs, self-refs, 1:1, exclusion constraints).

**`Or`/`Not` are NOT supported and cannot be** under the current model: one
4-bit operator nibble per column cannot represent disjunction. That needs the
expression-tree IR, which is also what joins need. Do not try to bolt OR onto
the nibble shape.

### M2: `= ANY` + relation loading (2026-08-24)
`= ANY($1)` binds a list to ONE placeholder — 1, 50 and 500 ids give identical
SQL *and* shape (asserted). With `runtime.CountingExecutor`: **50 parents +
25,000 children = exactly 2 round trips**, test fails otherwise. This is the
N+1 guarantee and it does NOT need SQL joins — the two-query `= ANY` batch is
the mechanism.

**Known cost, do not misquote:** `= ANY` with 500 ids costs **1,021 allocs** —
~2 per bound id, from *pgx's array encoder on the parameter side*, not our scan
path. Reading 500 rows is still a handful. Fix would be encoding the array into
a pooled buffer (same trick that killed the `[]any` boxing); unbuilt.

### Expression-tree IR (2026-08-24)
Per-column operator mask replaced by a **postfix token stream**; the stream is
the statement key. Tokens hold only compiler ids (kind/op/col/arity), never
values. **Cache hits compare the tokens, not just the hash** — a 64-bit
collision would return wrong SQL. `Any`/`Not`/`NotAny` now work; precedence
cross-checked against Postgres (333 rows agree), which is the test that catches
a missing paren group since AND binds tighter than OR.

**Cost I introduced:** build-6-predicates + prepare went 49ns → **477ns** (10x).
Query grew ~150 → ~330 bytes (token buffer + per-type arenas) and is a value
type, so six builder calls copy it. Still **0 allocations**; end-to-end scan
unchanged (2.26ms, 6 allocs). 0.7% of a 64us query. **Known fix: make Query a
pointer** — 1 heap alloc is noise vs pgx's 5, and builder copies vanish. Kept
value semantics because a pooled pointer Query has use-after-free hazards.

Overflow of the fixed buffers sets a flag every terminal returns as an error —
never a silently dropped predicate. Chained sugar now generates as
`Where(Col.Op(v))` so the two forms cannot drift.

### Tree IR builder cost, resolved (2026-08-24)
477ns → **257ns chained, 124ns via one `Where(a,b,c,...)` call**, still 0 allocs.
Two fixes: `push`/`leaf` mutate through a **pointer receiver** (only ever called
on a local, so value semantics survive — `q2 := q1.Where(x)` must not mutate q1,
which is why Query is NOT a pointer), and passing all predicates to one Where
costs one copy instead of six.

**Trap that cost a debugging round:** converting `push` to a pointer receiver
while leaving its `Query` return in place. Go allows discarding a return value
in a statement, so `q.push(t)` compiled and silently dropped every mutation —
SQL came out as `WHERE ` with nothing after. When converting value→pointer
receivers, delete the return in the same edit.
