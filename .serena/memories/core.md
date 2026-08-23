# raorm — core

Embeddable ORM library for Go. `github.com/gsoultan/raorm`, Go 1.26. Imported,
not deployed. Zero CGO. Driver deps isolated one-per-adapter (`pgx/v5` in
`runtime/pgxdrv`).

**Postgres first**, then MySQL/MariaDB → SQL Server → Oracle → MongoDB. The
dialect is a *compile-time parameter*: for an interpreter multi-dialect is a
runtime tax, for a compiler it is a build-time cost. **Model-first** — one Go
schema, generated query API, generated migrations raorm never applies.

**Thesis:** every other Go ORM builds SQL at runtime; raorm builds it at compile
time, *including the dynamic queries*. A dynamic query has a bounded set of
shapes — compile each once, cache under a `uint64` mask, warm calls allocate
nothing to build SQL.

**Status (2026-08-23): M0 PASSED — there is running code.** `internal/spike/`
(~350 lines) + `bench/` with a real Postgres via `make db`. See [[m0_results]]
and `bench/RESULTS.md`. M1 is next.

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

## Standing rule
Never quote a performance number from memory. `bench/RESULTS.md` or nothing.
