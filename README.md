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

**v0.1.0 is tagged, and M0–M8 have passed.** The read path, migrations,
relations, writes, the typed escape hatch and the tooling gate are built,
benchmarked and hardened; the first adopter migrated a whole bounded context
(M6) and runs on the published module. The milestone log with every exit gate
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

Wall clock is round-trip-dominated for every ORM — the honest claims are
allocations, GC pressure (storm 21 GCs vs pgx's 102 on the 2M-row workload),
and the plans: `Exists()` is a `LIMIT 1` probe, relation loads carry no
useless `ORDER BY`, per-parent limits lower to `LATERAL` (measured 33× over
`row_number()` at 100 parents), and projections make index-only scans
possible.

## Tooling

The tool is a library you give a five-line `main`, because the commands need
your models and an installed binary cannot see them
([EXAMPLE §2](docs/EXAMPLE.md)):

```go
func main() { tool.Main(model.All(), nil) }   // cmd/storm/main.go
```

```console
go run ./cmd/storm generate [dir]   # one package per table + the context package
                     diff <name>    # a reviewable migration; never applied by storm
                     verify         # drift: model vs database
                     verify -stale  # generated code vs model (CI, no database)
                     verify -pending# model vs migrations — "forgot to diff" fails CI
                     lint           # every named plan costed in round trips, budgeted
                     explain        # every statement planned; large seq scans flagged
                     import         # an existing database, written back as a model draft
```

## Design documents

[CONCEPT](docs/CONCEPT.md) · [ARCHITECTURE](docs/ARCHITECTURE.md) ·
[DIALECTS](docs/DIALECTS.md) · [COMPARISON](docs/COMPARISON.md) ·
[PLAN](docs/PLAN.md) — the ADRs, including the rejected-ideas list, are under
[docs/adr](docs/adr). Some API sketches in the design docs describe planned
surface (joins with cross-table rows, aggregate projections); the example and
the generated code are the as-built truth.

## Boundaries

No applied DDL. No lazy loading — ever; unloaded relations do not compile. No
runtime dialect branch. No reflection under `runtime/`. Every "no" is a year
of maintenance not spent; the reasoning lives in the ADRs.
