# raorm — production readiness (2026-08-24)

**Not ready to adopt.** Correctness is strong; coverage of the type system and
the absence of any adopter are what block it. Full checklist in `docs/PLAN.md`
§"Production readiness".

## The blocker nobody would guess — NOW CLOSED for numeric and jsonb
**`numeric` shipped 2026-08-24 as `raorm.Decimal`** (exact, two words, no
allocation, stdlib-only; 18 significant digits, with a GENERATION error past
that and a decode ERROR rather than a wrong number). **`jsonb` shipped** as
`runtime.JSON` — raw bytes the caller unmarshals, no value predicates.

**`text[]` and `uuid[]` shipped 2026-08-24** — nil vs `'{}'` kept distinct, a
NULL element is `ErrArrayNull` not `""`, no value predicates (equality on an
array is order-sensitive; `@>`/`&&` do not exist yet). Two bugs found before
commit: the scanner clobbered decode errors (later column overwrote the one
that mattered — scan now returns on FIRST failure), and the fuzzer found a
17-byte value whose count field made `make` allocate gigabytes — allocations
are now bounded by the input, never by a field inside it.

**`date`, `interval`, `inet`/`cidr`, `int8[]` shipped 2026-08-24.** The design
calls worth remembering: dates are midnight UTC by stated convention;
`raorm.Interval` keeps months/days/micros APART (a month has no length; a day
is not 24h across DST — flattening at decode would bake in the error Postgres
avoids) with `Duration()` returning ok=false when months present; inet and
cidr are both `netip.Prefix` (the DATABASE polices host bits — two Go types
would double the predicate machinery for a distinction it already enforces);
interval has NO equality predicates (`'24:00' = '1 day'` normalises). Interval
binding bridges through pgxdrv registration, the same pattern as Decimal.

Still missing: time-of-day (needs a pgx binding decision), `numeric[]`. The
generator otherwise binds bool, integer and float
widths, text/varchar, bytea, uuid, timestamptz, enums — and stops. The fixture
no longer has any column the generator omits.

No application handling money can model its own tables. This is the first thing
a real adopter hits, and it is invisible from the benchmarks because the bench
table has none of those types.

**The decision that was taken:** a raorm-defined fixed-point type, keeping the
stdlib-only rule. Pulling in a decimal package would put a third party's type on
every row of every financial table; a caller who wants shopspring/decimal
converts at the edge, which is one line and their choice.

**Do not offer float64 for numeric.** It cannot represent 0.10.

## What IS solid, and why to trust it
- **Injection is structural.** The property asserted is *the SQL is identical
  whatever the value*, not a payload blocklist. A rejected payload must fail as
  a DATA error (length, encoding) — a syntax error would mean it reached the
  statement.
- **Fuzzing earned its place immediately.** 20 seconds found a malformed token
  stream that silently dropped its `WHERE` clause — a filter failing **open**.
  Now fails closed via `Stmt.Err`, which every terminal returns. CI fuzzes on
  every PR, not nightly.
- Migrations converge (diff → apply → verify → diff empty, on a live database).
- Every round-trip claim is asserted with `CountingExecutor`.
- Coverage floors in CI (`scripts/check/coverage.sh`).

## The lesson from testing the CLI
It was at 0% and hid **three** bugs, two able to lose data:
1. Flags after a positional were silently ignored — `raorm diff init -schema
   mine` diffed `public` and proposed dropping its objects.
2. Introspection rendered expressions under the wrong `search_path`, so any
   enum outside `public` reported drift **forever**.
3. `raorm import` printed DDL instead of a model, so the adoption on-ramp
   produced something the adopter already had.

**That is the rate to expect from M6.** Untested surfaces hide data-loss bugs,
and the CLI is the surface every user touches first.

See [[core]], [[write_path]], [[plan_types]], [[seam_and_codegen]].

## M7 closed (2026-08-24)
`lint` (plan round-trip budgets from the IR), `explain` (GENERIC_PLAN validity
gate everywhere + seq-scan gate where statistics exist — the CI-database trap
is documented, not hidden), `verify -pending` (replay migrations into scratch,
diff against model; the failure prints the missing migration's SQL). 

**pgx trap worth remembering:** GENERIC_PLAN needs the raw wire. Both extended
AND "simple" pgx modes sanitise `$n` client-side and refuse zero arguments;
`PgConn().Exec` ships text untouched.

**Floors caught dead code:** codegen fell 0.7 under its floor and the gap was
four zero-caller functions dead since the tree-IR migration. Deleted, not
tested — a floor cannot tell dead from untested, and neither can a reader.

What now stands before production: the M6 adopter run, M8 release policy, the
machine-blocked unix-socket benchmark, and P5 expressiveness (joins,
projections, CTEs) for M5.

## M5 closed (2026-08-25), and two fresh-environment lessons
`raorm.SQL[T]` ships both halves: runtime (typed rows, atomic scanner cache,
arg-count check before the server) and generation (PREPARE against the MODEL
in a scratch schema — needs a server, NOT a schema snapshot; a drifted dev DB
cannot vouch for a query the model would reject). Matching is by NAME, both
directions surplus-checked. Unbuilt: raw fragments as join sources (needs P5
IR joins).

**The fresh-container harvest — dev databases are never fresh, and that is the
gap's camouflage:**
1. `pgddl` never emitted `btree_gist`; every generated migration failed on a
   fresh cluster and worked everywhere developers looked. Emitted now, as a DO
   block because `CREATE EXTENSION IF NOT EXISTS` **races with itself** across
   connections (pg_extension_name_index).
2. "The output compiles" as a test bar found: import paths glued from absolute
   dirs; module root ≠ working directory (under `go test` and for users in
   subdirs — walk up to go.mod); generate and verify -stale deriving imports
   separately and disagreeing.

## Dev environment (this machine, 2026-08-25)
Go binaries get EHOSTUNREACH dialing the Apple container's 192.168.64.x IP
(macOS local-network privacy; nc/ping fine). **raorm-pg now publishes
127.0.0.1:5433** (5432 is another project's forward). Run tests with
`RAORM_DSN=postgres://raorm:raorm@127.0.0.1:5433/raorm`; the Makefile's
container-IP DSN derivation is broken for Go on this machine.

## Perf pass (2026-08-25): the Query struct diet
**A value type's size is part of its API.** Type-coverage work emitted every
arena into every table's Query → 704B (history: ~150 mask, ~330 tree), builder
regressed 257→~450ns and NO TEST CAUGHT IT — allocations have AllocsPerRun
tripwires, sizes had none. Fix: `slotsFor` emits arenas/Pred slots/opIn
branches only for kinds the table has. genuser.Query 480B, Pred 120B; builder
−24.5% geomean (chained 174.6ns beats the old record), all 0-alloc, p=0.000
n=10. **TestQuerySize now pins sizes.** Profile says stop: remaining cost is
the documented value-semantics copies, 0.35% of a round trip. `make db` now
publishes loopback :5433 (Makefile DSN fixed for this machine's EHOSTUNREACH).

## The emitted-SQL audit (2026-08-25) — read the queries like a DBA
EXPLAIN ANALYZE on 50k rows convicted three shapes; all fixed, pinned on
CAPTURED SQL: **Exists was One() discarded** (top-N sort of 16,667 matches →
`SELECT 1 … LIMIT 1`, 2.95ms→0.017ms, ~170×); **loaders paid for orders their
bucketing destroyed** (external merge sort, 5MB disk spill → `Unordered()`,
~50ms→10.4ms; After() on unordered = error); **Count+Offset bound mismatched
args** (sliced one arg off the end → bindPreds). Confirmed fine: =ANY index
scans, LATERAL, keyset row comparisons, explicit columns, statement cache.
**Semi-join predicates shipped** (`HasPosts()`/`HasNoPosts()`): constant EXISTS
fragments as pseudo-columns past nCols — composition free from the token
stream, inner table ALWAYS aliased (self-reference capture). Gap recorded:
inverse one-to-one never reaches IR Relations. Second shared-fixture race
fixed: scratch schemas are per-process (pid-suffixed) — a literal name shared
across test binaries had one process dropping it mid-apply of another.

## P5 progress (2026-08-25): semi-joins + filtered Having + projections
**Semi-joins**: `HasPosts()` constant-EXISTS pseudo-columns; **filtered tier**
`store.UserHavingPosts(q, post.Pred…)` — child-typed predicates cross the
package boundary as CONCATENATED TOKEN STREAMS (child cols rebased past
`runtime.ChildColBase`, composite FragOf routes by range, ONE splice numbers
placeholders across). Table packages export composition seams (FragOf,
PredToks, BindPreds…) — ids and constant fragments only. Inner table ALWAYS
aliased (self-ref capture, proven live).
**Projections**: `p.Named("Contact", &u.Email, &u.Name)` → AllContact/Into.
Honest numbers: −26% client bytes / 5 allocs; wall PARITY on sort-bound bench
query (not a go-faster-stripe); the real claim measured — covered projection =
Index Only Scan, Heap Fetches 0, 4.8× vs full row, a plan full-row reads
foreclose by construction. First bench run was harness skew (fresh vs reused
slab/slice, 17 allocs/103KB) — the exact capacity-mismatch the perf veto
exists for; kept in RESULTS.md.
Remaining P5: cross-table joins/rows, CTEs, windows, FILTER, GROUPING SETS,
UNION ALL — all reachable via SQL[T] meanwhile.