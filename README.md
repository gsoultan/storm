# storm

> **Every other Go ORM builds SQL at runtime. storm builds it at compile time —
> including the dynamic queries.**

An embeddable ORM for PostgreSQL and Go. Generated, not reflected. Imported,
not deployed. Zero CGO; the driver lives behind a four-method port.

**Model-first:** one plain Go struct per table — no tags, no DSL. Everything
the type cannot say goes in a `Schema` method using **field pointers**, so the
editor enforces names and refactors follow them. storm emits reviewable
migrations and **never applies DDL**.

> **Renamed from `raorm` (2026-08-27).** The module path is now
> `github.com/gsoultan/storm`, and **v0.2.0 is the first usable version under
> it** — GitHub's redirect makes the older tags *visible* under this path, but
> their `go.mod` declares the old one and Go rejects the mismatch. v0.1.x
> remains available as `github.com/gsoultan/raorm`. See
> [CHANGELOG.md](CHANGELOG.md) for the two-step migration.

## Status

**v0.3.0 is tagged.** The read path, migrations, relations, writes, the typed
escape hatch and the tooling gate are built, benchmarked and hardened; the first
adopter migrated a whole bounded context (M6) and runs on the published module.
v0.3.0 adds model discovery, declared aggregations and joins, full-text search,
range types and typed constraint errors — and a MySQL DDL back end that is
**not** a runtime target
([ADR-0007](docs/adr/0007-mysql-runtime-needs-a-second-decoder-family.md)). The milestone log with every exit gate
is [docs/PLAN.md](docs/PLAN.md), and what would still stop a team adopting
this is written down, with gates, in
[docs/PRODUCTION-READINESS.md](docs/PRODUCTION-READINESS.md).

Every claim below is a test or a benchmark in this repository. The quickstart
is executable: [`examples/blog`](examples/blog) runs as a test in CI, so it
cannot drift from the library the way prose does.

## The sixty-second tour

```go
// The model: a plain struct. *T = nullable, Xxx Yyy = foreign key,
// []T = has-many. Field pointers declare the rest.
type Author struct {
    storm.Model            // uuid id + created_at/updated_at
    Name     string
    Email    string
    Articles []Article
}

func (a *Author) Schema(t *storm.Table)           { t.Unique(&a.Email) }
func (a *Author) Plans(p *storm.Plans)            { p.Named("Feed").With(&a.Articles) }
func (a *Author) Projections(p *storm.Projections) { p.Named("Card", &a.Name, &a.Email) }
```

```go
// Typed queries. A dynamic query has a bounded set of shapes; each compiles
// once, and a warm call allocates NOTHING to build its SQL.
rows, err := article.New().
    Where(article.AuthorID.Eq(id), article.PublishedAt.IsNotNull()).
    Order(article.PublishedAt.Desc()).
    All(ctx, ex, nil)

// The semi-join: authors who HAVE a published article. Child predicates are
// typed by the child's package; one EXISTS probe per row.
store.AuthorHavingArticles(author.New(), article.PublishedAt.IsNotNull())

// The named plan: authors WITH their articles — exactly two round trips
// whatever the row count, and reading an unloaded relation DOES NOT COMPILE.
feed, err := store.AuthorFeed().Limit(10).All(ctx, ex)

// Writes are masked: unset columns take their database defaults, and an
// UPDATE writes only what was assigned. A version column makes stale writers
// lose loudly. Graph writes flush in FK order, one batch, atomic.
n := author.Create(); n.SetName("Ada"); n.SetEmail("ada@example.com")
ada, err := n.Insert(ctx, ex)

// Declared aggregations: GROUP BY without dropping to SQL. The result types
// are PostgreSQL's, not the input's — count is int64, sum(numeric) is a
// NULLABLE Decimal, because over zero rows it IS null. Each declaration hands
// back a handle, so a later clause refers to it by value, not by string.
func (o *Order) Aggregates(a *storm.Aggregates) {
    b := a.Named("ByStatus")
    b.By(&o.Status)
    orders := b.Count("Orders")
    b.Sum(&o.Total, "Revenue")
    b.Having(a.Gt(orders, 0))
}
rows, err := order.New().Where(order.PlacedAt.Gte(t)).AllByStatus(ctx, ex)

// Joins project ACROSS tables, declared with field pointers into a local var.
// LEFT is in the type: anything from the right side is Null[T].
func (o *Order) Joins(j *storm.Joins) {
    var c Customer
    j.Named("WithCustomer").Inner(&c, &o.Customer).
        Take(&o.ID, "OrderID").Take(&c.Email, "Email").OrderDesc(&o.PlacedAt)
}
rows, err := order.New().Where(order.Status.Eq("paid")).AllWithCustomer(ctx, ex)

// Anything PostgreSQL can run, typed, validated against the model at
// generate time — mismatches fail the build naming the column and the fix.
var Top = storm.SQL[TopRow](`WITH ranked AS (...) SELECT ... LIMIT $1`)
var Purge = storm.SQLExec(`DELETE FROM sessions WHERE expires_at < now()`)
```

## The numbers

Measured, never quoted from memory — the methodology and every caveat live in
[`bench/RESULTS.md`](bench/RESULTS.md). At 1,000 rows, allocations per query:

| storm | raw pgx | sqlc | Bun | Ent | GORM |
|---|---|---|---|---|---|
| **6** | 5,012 | 5,022 | 13,899 | 23,016 | 23,934 |

> **Measured on Go 1.26.6.** The allocation counts above were re-checked on 1.27
> and are unchanged — they are what this table is about. The wall-clock figures
> have not been, and one offline benchmark did move; the note at the top of
> [`bench/RESULTS.md`](bench/RESULTS.md) has the detail.

Wall clock is round-trip-dominated for every ORM — the honest claims are
allocations, GC pressure (storm 21 GCs vs pgx's 102 on the 2M-row workload),
and the plans: `Exists()` is a `LIMIT 1` probe, relation loads carry no
useless `ORDER BY`, per-parent limits lower to `LATERAL` (measured 33× over
`row_number()` at 100 parents), and projections make index-only scans
possible.

## Tooling

Install it, run it, write no bootstrap. There is no registry to maintain and
no `main` to keep in step with your models — storm finds them by parsing
([ADR-0006](docs/adr/0006-discovery-replaces-the-bootstrap.md)):

```console
go install github.com/gsoultan/storm/cmd/storm@latest
go get github.com/gsoultan/storm/tool     # once per module

storm generate [dir]    # one package per table + the context package
      watch <dir>       # regenerate on save; leave it running while you edit
      models            # what discovery found, and which rule matched
      diff <name>       # a reviewable migration; never applied by storm
      verify            # drift: model vs database
      verify -stale     # generated code vs model (no database, unless you declare storm.SQL)
      verify -pending   # model vs migrations — "forgot to diff" fails CI
      lint              # every named plan costed in round trips, budgeted
      explain           # every statement planned by the server; large seq scans flagged
      import            # an existing database, written back as a model draft
      portable <engine> # what in this model does NOT cross to another engine
```

A type is a model when it embeds `storm.Model`, or declares a `Schema`, `Plans`
or `Projections` method, or carries `//storm:model`; `//storm:ignore` opts out.
A type embedded in another struct is a **mixin**, not a table. `storm models`
prints the verdict and the rule behind it.

Field pointers still resolve at runtime — storm writes the bootstrap it used to
ask you for, runs it, and removes it. The hand-written
`tool.Main(model.All(), model.Queries())` is still supported and generates
byte-identical code ([EXAMPLE §2](docs/EXAMPLE.md)).

**You do not have to remember to regenerate.** The step cannot be removed — Go
has no build hook — but two things remove the remembering. `storm watch` keeps
the tree current as you save. And generated code carries a **shape assertion**,
so a model that gained, lost, renamed or reordered a field stops the build
naming that field, instead of silently missing it:

```
store/shape.gen.go:57:2: too few values in struct literal of type model.Product
```

Changes inside `Schema`, `Plans` or `Projections` are method bodies the type
system cannot see; `storm verify -stale` is still the check for those.

## A worked service

[`examples/orders`](examples/orders) is a Go kit microservice on storm, in its
own module: catalogue, checkout that reserves stock under concurrency, order
retrieval, and a finance report. Its concurrency test puts 12 goroutines
against a stock of 20 and asserts nothing is oversold.

```console
$ cd examples/orders && storm generate store && go test ./orders/
```

## Design documents

[CONCEPT](docs/CONCEPT.md) · [ARCHITECTURE](docs/ARCHITECTURE.md) ·
[DIALECTS](docs/DIALECTS.md) · [COMPARISON](docs/COMPARISON.md) ·
[PLAN](docs/PLAN.md) — the ADRs, including the rejected-ideas list, are under
[docs/adr](docs/adr). Some API sketches in the design docs describe planned
surface (joins with cross-table rows, aggregate projections); the example and
the generated code are the as-built truth.

## Scheduling without a race

```go
During storm.TstzRange                       // in the model
t.Exclude(storm.With(&b.Room, storm.OpEq),
          storm.With(&b.During, storm.OpOverlaps))

booking.New().Where(booking.During.Overlaps(window)).All(ctx, ex, nil)
```

Half-open by default, so `[09:00, 11:00)` and `[11:00, 12:00)` abut rather than
clash. The overlap is enforced by a GiST exclusion constraint — two concurrent
bookings for one room cannot both commit — and the loser gets
`runtime.ErrExclusionViolation`. **No other Go ORM models exclusion
constraints**, and they are the correct answer to booking, scheduling and
rate-plan overlap.

## Full-text search

```go
Search storm.TSVector          // in the model
t.Col(&p.Search).Generated(storm.RawSQL(`to_tsvector('english', name)`)).Index()

product.New().Where(product.Search.WebSearch(q)).All(ctx, ex, nil)
```

Filterable, never readable: a tsvector is index support, so it is absent from
`Row` and from writes. The term is bound, so a search for `'); DROP TABLE --`
is a search for those words. Optional filters compose without a nil check:
`q = product.WhenSet(q, f.MinPrice, product.Price.Gte)`.

## Errors you can switch on

Constraint violations are typed at the driver boundary, so a handler never
decodes a SQLSTATE:

```go
_, err := n.Insert(ctx, ex)
switch {
case errors.Is(err, runtime.ErrUniqueViolation):     // 409, and ce.Constraint says which
case errors.Is(err, runtime.ErrForeignKeyViolation): // 400
case runtime.Retryable(err):                          // 40001/40P01 — run it again
}
```

`ConstraintError` names the constraint, table and column and carries **no bound
value**; PostgreSQL's own diagnostic does, and it stays reachable through
`Unwrap`. Anything storm has no opinion about comes back unchanged.

## Boundaries

No applied DDL. No lazy loading — ever; unloaded relations do not compile. No
runtime dialect branch. No reflection under `runtime/`. Every "no" is a year
of maintenance not spent; the reasoning lives in the ADRs.
