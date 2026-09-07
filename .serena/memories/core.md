# storm — core

Embeddable ORM library for Go. `github.com/gsoultan/storm`, Go 1.27. Imported,
not deployed. Zero CGO. Driver deps isolated one-per-adapter (`pgx/v5` in
`runtime/pgxdrv`).

**Postgres first**, then MySQL/MariaDB → SQL Server → Oracle → MongoDB. The
dialect is a *compile-time parameter*: for an interpreter multi-dialect is a
runtime tax, for a compiler it is a build-time cost. **Model-first** — one Go
schema, generated query API, generated migrations. No CLI command applies one;
`migrate.Auto` does, opt-in, since 2026-09-07 — see [[automigrate]].

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

**v0.7.0 tagged 2026-09-07** — **automigrate**. `migrate.Auto` / `AutoPool`
apply DDL from a running process, and ADR-0001 carries a dated amendment saying
why the ban did not survive contact: the danger it named was *silent* schema
change, and implicit / unserialised / partial / destructive-by-default are each
separately fixable. Advisory lock, plan computed AFTER taking it, one
transaction, `lock_timeout` 3s, no default `statement_timeout`. Three defects
found by its own tests, all in the concurrency the feature exists for — see
[[automigrate]], which is worth reading before touching `migrate/`. It also
fixed a defect **older than this work**: `storm diff` wrote an enum label and
the statement using it into one file, which no runner wrapping a file in a
transaction could apply. Verified against argus's ten hand-written migrations
(rebuilt from nothing) and anubis's real model. `migrate/` stopped being
build-time-only, so the pgx boundary is now machine-checked at the ROOT
package instead of asserted in a comment.
**v0.6.6 tagged 2026-09-07** — `int4[]`. It was the last gap in argus's
schema, and `storm verify` there is now at **ONE** pending change: `CREATE
INDEX` on a foreign key, which is storm's opinion, not a defect. Across three
releases against the same live database: failed outright → 26 → 5 → 2 → 1.
**v0.6.5 tagged 2026-09-07** — an imported model can be VERIFIED. argus went
from `verify` failing outright → 26 pending changes → **2**, neither a defect
(`int4[]` has no Go type, and storm indexes every FK where argus does not).
Serial is now an IR fact distinct from Identity; constraint names are carried;
uniques and checks are emitted rather than listed as lost; every default is
emitted. It also corrected v0.6.4's nullable-slice change, which had made a
nullable jsonb NOT NULL — caught only by running verify against a real
database, which is the whole argument for [[argus_adopter]].
**v0.6.4 tagged 2026-09-07** — `storm import` could not import. Eight defects
found by pointing the on-ramp at argus, a database it had never seen, as step
one of the second-adopter exercise. It refused to run in a module with no
models (the only kind it serves), then emitted a model that did not parse, then
one that did not compile, then one `storm.Build` refused. The gate had only
ever checked that the output PARSES; it compiles and Builds it now, and caught
a ninth defect the same day. **Open: BIGSERIAL does not round-trip** — it comes
back as a literal `nextval()` default that cannot apply to a scratch schema.
See [[argus_adopter]].
**v0.6.3 tagged 2026-09-07** (generated code did not pass `go vet` — the
shape assertion's unkeyed literal, which is the check itself, is what vet
reports for an imported struct type; a local defined type keeps both. Shipped
broken from v0.3.0. Found by upgrading anubis, again). The lesson is about
gates, not vet: `scripts/check/outsider.sh` existed to see storm from outside
and was the one check `make check` did not run. It runs there now.
**v0.6.2 tagged 2026-09-07** (the MySQL dialect's generated package could not
compile; `codegen.TestMySQLGeneratedPackageCompiles` is the gate. Postgres
output unchanged, measured). Verified from the proxy: a fresh outside module
builds and `codegen.Version()` reports `v0.6.2`.
**v0.6.1 tagged 2026-09-06** (two verify fixes found by upgrading anubis off
v0.2.0 — the adopter-upgrade exercise works, do it again after each release).

**anubis is on v0.6.3 as of 2026-09-07** (anubis#15) — the upgrade finally
LANDED; before this it was an unpushed local branch while `dev` stayed on
v0.2.0. It cost two storm releases and found two Go-toolchain consequences in
anubis (`go mod tidy`, and protoc-gen-go reformatting doc comments because it
formats with the toolchain it was BUILT with — storm's `go 1.27` forced the
bump).

**Soaked on the new pin the same day**: p95 208µs (raw pgx 256µs), shapes
1 → 1, flushes 0 → 0, RSS 313 → 598 → 553 → 618 MB across four rounds at
~10,800 decisions/s — a step then flat, which is the plateau. Nothing moved.
Read RSS as a series inside one run; the quiet readings across rows (213.8,
309.5, 17.9 MB) are different processes, not a trend.
**v0.6.0 tagged 2026-09-06** (row locking). v0.5.0 tagged 2026-09-05 (index
grammar, upsert on every unique index, top-N, AnyOf, budgets, statement
pinning). Both verified from the module proxy by a fresh outside module.

**R9 update 2026-09-06:** the dialect seam's MySQL side had NEVER been
compiled — text assertions only — and did not build (missing import, a
neutral generic prefixed with the family package, Decimal arity, fallible
decoders called as infallible). Fixed; `codegen.TestMySQLGeneratedPackageCompiles`
is the gate. M9's remaining cost is the wire-level driver, nothing else.

See [[indexing]] for the index grammar and the three server behaviours it had to learn (2026-09-05).
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
- [[automigrate]] — `migrate.Auto`, why ADR-0001 was amended rather than upheld,
  and the two concurrency defects its own tests found
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
