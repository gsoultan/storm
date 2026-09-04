# storm — core

Embeddable ORM library for Go. `github.com/gsoultan/storm`, Go 1.27. Imported,
not deployed. Zero CGO. Driver deps isolated one-per-adapter (`pgx/v5` in
`runtime/pgxdrv`).

**Postgres first**, then MySQL/MariaDB → SQL Server → Oracle → MongoDB. The
dialect is a *compile-time parameter*: for an interpreter multi-dialect is a
runtime tax, for a compiler it is a build-time cost. **Model-first** — one Go
schema, generated query API, generated migrations storm never applies.

**Thesis:** every other Go ORM builds SQL at runtime; storm builds it at compile
time, *including the dynamic queries*. A dynamic query has a bounded set of
shapes — compile each once, cache under a `uint64` mask, warm calls allocate
nothing to build SQL.

**Status (2026-08-24).** M0, M1 and M2's read path passed; the repo has git
history from this date (it had **none** before — 9,143 lines untracked, tagged
`m2-read-path` at import). Since then: the dialect seam extracted and
CI-enforced, whole-context generation, the M3 plan-type spike passed, and the
single-row write path shipped. `docs/PLAN.md` carries the **P0–P5 execution
sequence**, which deliberately runs writes (M4) before relations (M3).

See [[query_expressiveness]] for where the declared query surface ends and what
is deliberately left to `storm.SQL[T]` (updated v0.5.0, 2026-09-05).
See [[m6_first_adopter]] for the adopter migration (M6 PASSED 2026-08-25 — anubis/authz fully on storm, p95 parity, four storm fixes it forced), [[m0_results]] for the thesis numbers, [[seam_and_codegen]] for R9 and the
generator, [[plan_types]] for why M3 is de-risked, [[write_path]] for M4.

## Production-grade gates (2026-08-25)

**Injection closed end to end 2026-09-03**: `storm.SQL` statements are pinned to
the text `storm generate` PREPAREd (RegisterStatement), and an undeclarable
declaration fails the build. See [[decisions]] for why the scanner-by-row-type
key made this a real vector.

`docs/PRODUCTION-READINESS.md` is the operative plan — the PLAN.md assessment
is superseded in part.

**P0.1 CLOSED 2026-08-26**: storm decodes binary wire format and now says so.
pgxdrv refuses SimpleProtocol/Exec at pool construction AND checks every
result once per statement (3.82ns/8 cols, 0 allocs = 0.0043% of a Get). The
rule is a DENY-list of binary-layout types, never an allow-list: pgx sends
text, varchar, jsonb and **enum labels** as text on a binary connection, so
"everything must be binary" failed four fixture tests. Domains are safe
because PostgreSQL reports their base type's OID. See docs/DEPLOYMENT.md for
the PgBouncer table (transaction pooling: fine; statement pooling: no).

**P1.1 CLOSED 2026-08-26**: TreeCache is bounded by runtime.ShapeCap (1024,
SetShapeCap(0) opts out). Past the cap the map is DROPPED whole — not evicted,
because eviction needs a write on the read path. 100k shapes: 170KB capped vs
27,833KB unbounded (164x); warm path +0.5ns, Get byte-identical. Generated
packages expose ShapeFlushes(); nonzero means a call site mints shapes from
request data.

**P2 CLOSED 2026-08-26**: generated headers carry the storm version; error/SQL
value hygiene is a test (bench/errhygiene_test.go, 13 shapes); tracing recipe
in docs/DEPLOYMENT.md — and writing its proof found that pgx's QueryTracer is
BLIND to batches, so the recipe needs QueryTracer + BatchTracer +
CopyFromTracer or a plan's relation loads go unseen.

**P1.2 CLOSED + v0.1.0 SHIPPED 2026-08-26**: storm is public and tagged
v0.1.0 at d95ac55. anubis consumes it as a PINNED module — no `replace`, no
go.work sibling — verified by building and running its whole suite from the
module proxy alone. Generated headers now read `storm v0.1.0`.

The tag preceded the soak window's close (2026-09-08), so the soak's kill
criterion now opens a **v0.1.1** rather than moving a tag: p95 drift past
2ms, monotonic shape growth, or RSS without a plateau. Signals unchanged.

**Also 2026-08-26**: int8[] and text[] joined uuid[] with fast parameter
codecs (21x/8x, one allocation each — bigserial-keyed schemas bind int8[] on
the same `= ANY($1)` loader path storm's uuid fixtures hid). pgxdrv gained a
coverage floor (85, at 88.4%) because it now holds the wire-format deny-list;
reaching it found pgxdrv.Tx.Exec at ZERO coverage — the "every surface"
transaction test exercised a unit flush, COPY, a plan and a count but never an
update or delete, the two that reach Executor.Exec.

**P4 — the stranger test, 2026-08-26.** After v0.1.0 shipped, a fresh module
OUTSIDE both repos found three first-run defects in five minutes: `generate`
emitted storm's own module path into the caller's module (flagship command,
never worked for anyone else, invisible here because the wrong answer is the
right one); the tool was unreachable (Models in package main, "bootstrap" that
nothing generated) so adopters got no verify/lint/explain at all — now the
importable `storm/tool` with `tool.Main(model.All(), nil)`; and a symlinked
output path was refused. `scripts/check/outsider.sh` makes it a permanent CI
gate, verified to trip both ways.

**Closed at the cause, 2026-08-27 (v0.3.0).** Five lines is still five lines
you have to know to write. `cmd/storm` now discovers models by parsing and
writes the bootstrap itself — see [[discovery]].

See [[m6_first_adopter]] for how the adopter surfaced the reading that found
all of these.

## Read in this order
1. `docs/COMPARISON.md` — Ent/GORM/Bun/Hibernate by mechanism; the five-property gap table
2. `docs/CONCEPT.md` — 8 concepts + the **rejected** list (higher value than the accepted list)
2b. `docs/API.md` — the code design: model DSL, typed columns, named fetch plans, writes, errors
2c. `docs/EXAMPLE.md` — **complete worked sample** (12-table domain): all types, FKs,
    1:1/1:N/M:N-with-payload, self-ref hierarchies, polymorphic (3 strategies),
    transactions, unit of work. The document to read before writing any API code.
2d. `docs/DIALECTS.md` — capability matrix, lowering passes, why Mongo is a back end
3. `docs/ARCHITECTURE.md` — front end → IR → back end; package map; named design patterns
4. `docs/PERFORMANCE.md` — budgets (all *targets*, none measured yet); perf vetoes
5. `docs/PLAN.md` — M0–M8, exit gates, kill criteria, risk register
6. `AGENTS.md` — profile roster (compiler · perf · dba · dx · sec · arch · test)

ADRs 0001 and 0002 were **rewritten** on 2026-08-23 when multi-dialect + Mongo
landed; read [[decisions]] for what changed and why — the rewrites carry the
reasoning.

## Related memories
- [[m0_results]] — the spike result and the three findings that amended the plan
- [[decisions]] — the four load-bearing ADRs and what was rejected
- [[boundaries]] — the scope line
- [[seam_and_codegen]] — R9 finally mitigated; per-context generation; the
  15-column truncation that outlived its design
- [[plan_types]] — M3's ergonomics answered in 3 days, and why `Load(plan)`
  cannot be written in Go
- [[write_path]] — the dirty mask, optimistic locking, and ADR-0005
- [[production_readiness]] — what is solid, and the type-coverage gap that
  blocks adoption

## Standing rule
Never quote a performance number from memory. `bench/RESULTS.md` or nothing.
