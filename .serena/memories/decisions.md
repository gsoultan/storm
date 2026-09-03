# storm — decisions & negative results

Four ADRs, all **Proposed** (2026-08-23), in `docs/adr/`. Two were rewritten the
same day when multi-dialect + MongoDB became requirements — the rewrites matter
more than the originals.

- **0001 Model-first, migration-mediated DDL.** *(rewritten; originally
  database-first.)* The Go model is the source of truth. storm emits a numbered,
  forward-only, **reviewable** migration and **never applies one**. The original
  ADR conflated *where schema is declared* with *who applies DDL* — only the
  second is dangerous. Forced by the new targets: **Mongo has no schema to
  introspect**, and five hand-maintained migration sets never stay in sync.
  `storm import` keeps database-first as the on-ramp. Three verify modes:
  `--stale` (code↔model), `--drift` (model↔db), `--pending` (model↔migrations —
  *"changed the model, no migration"* becomes a CI failure).
  **Costs a schema-diff engine** (M1 kill criterion: delegate to Atlas).
  **Requires rewording the `anubis` rule** that SQL lives only in `db/queries/`
  and `migrations/`.
- **0002 Postgres first, targets sequenced.** *(rewritten; originally
  "Postgres only".)* Key insight: **for an interpreter multi-dialect is a
  runtime tax; for a compiler it is a build-time cost.** storm knows the target
  at generate time, so there is no runtime dialect branch and multi-target costs
  the hot path nothing. Sequence v1.0 PG → v1.1 MySQL/MariaDB → v1.2 MSSQL →
  v1.3 Oracle → v2.0 Mongo, ordered by distance from Postgres.
- **0003 No lazy loading — fetch plans in the type system.** Unchanged and
  **not reversible**; the load-bearing decision. Now via **named plans**
  (`plans.go`), see below.
- **0004 MongoDB is a back end, not a dialect.** The IR is a **logical plan**,
  not a SQL AST. Embed-vs-join is *declared* (`OnDocument(storm.Embed(...))`),
  never inferred; silence is a generation error. Gated behind Oracle shipping.

## Design change worth remembering
**Named fetch plans retired risk R3.** The first design generated a type per
`With(...)` combination (2ⁿ per entity) and needed AST scanning of call sites to
stay finite — with a chicken-and-egg, since user code will not type-check until
the generated types exist. Writing the API surfaced the better answer: **the
developer names the plan** in a `plans.go` they own, and the generator emits
exactly those types. No explosion, no AST scanning, and plans become the single
reviewable file listing every load pattern — which `storm lint --plans` can cost
in round trips.

## Rejected — do not rebuild (reasoning > verdict)
- **Runtime DDL / AutoMigrate** — silent production schema change. Emitting a
  migration is fine; *applying* one from library code is not.
- **Ambient persistence context + lazy loading** — root cause of nearly every
  Hibernate pathology. A field access that might do I/O makes performance
  unreviewable.
- **A runtime dialect branch** — *not* multi-dialect itself; that was the first
  draft's error. What forces the slow builder is deciding the dialect at query
  time.
- **`jsonb_agg` nested materialisation as default** — shifts cost to the DB and
  pays a JSON parse. A measured strategy option, never the default.
- **Bytecode VM query planner** — gwaf already measured that closures beat a VM
  in Go. Do not re-litigate.
- **Reflection fallback "for convenience"** — one reflection path becomes *the*
  path and the SLOs become fiction.
- **Active Record (`user.Save()`)** — couples entities to sessions; every unit
  test needs a database.
- **Soft delete by default** — a correctness landmine.
- **An Ent-style `Schema()` DSL as the primary model form** — *rejected on
  review 2026-08-23.* The model is a **plain Go struct**; ~90% of a schema is
  derivable from the type (`*T` = nullable, `XxxID`+`Xxx *Xxx` = FK, `[]T` =
  has-many, named string type = enum, struct = typed jsonb). Struct-as-model and
  codegen are **orthogonal** — the generator reads the struct at build time, so
  simplifying the declaration costs the thesis nothing.
- **Struct tags — removed entirely** *(2026-08-23, at the user's direction).*
  Everything the type cannot say goes in an optional `Schema(t *storm.Table)`
  method using **field pointers**: `t.Col(&u.Email).Unique().Size(320)`,
  `t.Index(&u.Org, storm.Desc(&u.CreatedAt))`, `t.Inverse(&o.Children, &o.Parent)`.
  The receiver is a **value** (`u User`) so fields are addressable. Resolution is
  AST-level at generation (works before the package compiles) and offset-from-base
  at runtime (for `verify --drift`). Stronger than a generator-validated string,
  because the *editor* enforces it and refactoring follows it.
  **Only three strings survive**, none a Go identifier: a database column name
  you are assigning (`.Named("reviewer_id")`), a database type
  (`.Raw("geography(Point,4326)", codec)`), and prose (`.AcknowledgeNoFK(reason)`).
  Plus `storm.RawSQL(...)` as a conspicuous, `PREPARE`-checked escape surfaced by
  `storm lint --expr`.
- **Pretending Mongo and Postgres are interchangeable** — one model can serve
  both where that genuinely makes sense; storm's job is making the divergence
  visible at build time, not hiding it.

## Open risks
- **R1 thesis wrong** → M0 kill criterion: >1.30× raw pgx after two optimisation
  passes ends the project. Two weeks, not five months.
- **R9 dialect seam rots** while only Postgres exercises it → the no-conditional-
  outside-`compile/` check is CI-enforced from M2, not added at M9.
- **R10 diff engine becomes its own project** → delegate to Atlas.
- **R11 Mongo's shape mismatch leaks into the SQL back ends** → logical-plan IR
  from M2; Oracle (M11) gates Mongo (M12).

See [[core]], [[boundaries]].

## The declaration vocabulary is builder methods, not package functions (2026-09-02)

`storm.Eq`, `storm.And`, `storm.Col` and nineteen more were package-level, in
scope beside the generated query API where `Eq` means something else:
`order.Status.Eq(x)` filters at run time, the package-level one described a
filter at declaration time. They are now methods on `Exprs`, embedded in
`*Aggregates` and `*Joins`, so a declaration constructor is unreachable from a
query. The root package exports none of the 22.

Declared aggregate outputs return an `Out` handle rather than being named by
string, and `Filter`/`OverWindow` hang off that handle rather than off "the last
term declared". A `Filter` with no aggregate is now unrepresentable.

`storm.Expr` is `storm.RawSQL`; `Expr` survives as a deprecated ALIAS (not a
defined type), so nothing that returns `RawSQL` breaks a v0.1-v0.3 caller. Live
API returns `RawSQL` — returning the deprecated name would flag SA1019 at every
call site of `storm.Now()`.

Chosen by the user from an options set, all three breaking, all three cheaper
before `STABILITY.md` binds at v1.0.0.

## Only a declared statement runs — storm.SQL is pinned (2026-09-03)

The ordinary read path was never an injection surface: a predicate is a stream
of compiler-generated ids, values travel as bound args, and no caller string is
present in the SQL text. `storm.SQL` was, and the reason is easy to miss —
**`RegisterScanner` keys by ROW TYPE**, so a scanner declared for one query
answers for any query returning that type. `storm.SQL[EarnerRow](fmt.Sprintf(…))`
therefore ran, on the strength of a scanner it never declared.

Fix is two layers:

- **Runtime pin (the guarantee).** `storm generate` emits a
  `storm.RegisterStatement` per PREPAREd statement; a declaration whose SHA-256
  is not registered is refused at the call, before the executor. Cached in an
  `atomic.Bool` per declaration so the warm path stays an atomic load (the perf
  profile vetoes a map lookup per query). A miss is deliberately NOT cached —
  every registration is in an `init()`, so a miss is permanent.
- **Build lint (the good error).** `storm generate`/`watch` fail on a
  `storm.SQL` call that is not a package-level var — never discoverable, so
  never registerable. Detected by SUBTRACTION (mark the legitimate package-level
  values, report every other call site), so a declaration nested in a closure or
  composite literal cannot slip through.

Registration takes statement TEXT, not a digest, so the generated `init()` is
reviewable — the thing a reader needs from it is which statements may run.

Migration cost: a test that hand-registers a scanner must hand-register its
statement too. See `internal/planspike/sqlquery_test.go` for the shape (SQL as a
`const` so declaration and registration cannot drift).

Not covered, deliberately: `storm.RawSQL` in check constraints and generated
columns still reaches DDL verbatim — mitigated by ADR-0001, storm never applies
DDL, so it is reviewed schema and not request data.
