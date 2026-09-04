---
tags: [storm, api, dx]
updated: 2026-09-04
status: as-built — every call shape here appears in a test that runs in CI
---

# The API, by example

Every code sample below is the API storm actually generates. The two worked
modules are the check: [`examples/blog`](../examples/blog) runs as a test in CI,
and [`examples/orders`](../examples/orders) is a separate module whose statements
are additionally sent through `EXPLAIN` by `scripts/check/explain.sh`. Where this
document and those disagree, they are right and this is a bug — prose drifts and
a test does not.

> For a complete worked domain — every scalar type, foreign keys, one-to-one,
> many-to-many with payload, self-referential hierarchies, polymorphic
> associations — see [[EXAMPLE]] and [[REFERENCE]]. Both are still marked as
> design sketches; treat their call shapes with suspicion until they carry the
> same as-built note this one does.

## Design principles

1. **Read like SQL.** The closer the API is to SQL, the less there is to learn.
2. **Identifiers are typed values, never strings.** A column rename is a compile
   error, not a 2 a.m. page.
3. **The fetch plan is visible at the call site.** You can count the queries a
   function runs by reading it.
4. **A shape storm has not seen cannot run.** Everything with an unbounded set
   of result shapes is *declared* in the model, not chained at the call site, so
   every statement has a generated scanner and a compiled statement.
5. **Errors name the query and the fix.**
6. **Generated code is code a human reviews**, not a wall to scroll past.

---

## 1. Declare the model

A plain struct per table. No tags, no DSL. `*T` is nullable, `Xxx Yyy` is a
foreign key, `[]T` is has-many.

```go
type Author struct {
    storm.Model            // uuid id + created_at/updated_at, database defaults

    Name  string
    Email string

    Articles []Article
}

// Everything the type cannot say goes in Schema, using FIELD POINTERS — so the
// editor enforces the names and a rename refactors them with the field.
func (a *Author) Schema(t *storm.Table) {
    t.Col(&a.Email).Size(320)
    t.Unique(&a.Email)
}

type Article struct {
    storm.Model

    Title       string
    Body        string
    PublishedAt *time.Time      // nullable

    Author Author               // foreign key → authors.id
}

func (ar *Article) Schema(t *storm.Table) {
    t.Col(&ar.Title).Size(300)
    t.Col(&ar.Author).OnDelete(storm.Cascade)
}
```

Five optional methods declare the rest, each covered in its own section below:
`Plans`, `Projections`, `Aggregates`, `Joins`, and `Schema`.

## 2. Generate

There is no config file. Storm parses your module to find the models
([ADR-0006](adr/0006-discovery-replaces-the-bootstrap.md)), so there is nothing
to point it at:

```console
$ storm generate store
  → store/article/article.gen.go (55364 bytes)
  → store/author/author.gen.go   (51899 bytes)
  → store/store.gen.go           (1828 bytes)
  3 package(s) from 2 table(s)
```

The commands you will actually use:

```console
$ storm models                  # what discovery found, and which rule matched
$ storm diff add_author_status  # write a migration from the live schema to the model
$ storm verify -stale           # fail if generated code is stale (needs no database)
$ storm verify -pending         # fail if the model has changes no migration carries
$ storm explain                 # plan every statement; flag large seq scans
$ storm watch ./store           # regenerate on save
```

`storm diff` writes a migration and **never applies one** — that is
[ADR-0001](adr/0001-model-first-migration-mediated-ddl.md), and no runtime code
path can alter a schema.

## 3. Typed columns, not strings

Each generated package declares its own typed column handles. The *kind* of the
handle is what makes an illegal predicate fail to compile:

```go
// store/article — generated
var (
    ID          = UUIDCol{0}
    CreatedAt   = TimeCol{1}
    Title       = TextCol{3}
    PublishedAt = NullTimeCol{5}     // *time.Time in the model
    AuthorID    = UUIDCol{6}
)
```

```go
article.Title.Like("On %")           // ok
article.Title.Gt("M")                // ok — text is ordered
article.PublishedAt.IsNotNull()      // only on a nullable column
article.AuthorID.Like("%x%")         // compile error: UUIDCol has no method Like
article.Titel.Eq("x")                // compile error: undefined
```

| Handle | Predicates |
|---|---|
| every kind | `Asc` `Desc` `AscNullsFirst` `DescNullsLast` |
| `UUIDCol` | `Eq` `NotEq` `In` `NotIn` |
| `TextCol` | `Eq` `NotEq` `In` `NotIn` `Gt` `Gte` `Lt` `Lte` `Like` `ILike` |
| `TimeCol`, `DecimalCol`, numeric | `Eq` `NotEq` `Gt` `Gte` `Lt` `Lte` |
| `BoolCol` | `Eq` `NotEq` |
| nullable (`NullTimeCol`, …) | + `IsNull` `IsNotNull` |
| `TSVectorCol` | `Matches` `WebSearch` |
| array (`TextArrayCol`, …) | `Contains` `ContainedBy` `Overlaps` |
| jsonb (`JSONCol`) | `Contains` `ContainedBy` `HasAnyKey` `HasAllKeys` |
| `TstzRangeCol` | `Eq` `NotEq` `Overlaps` `ContainsRange` `ContainedBy` |

There is no `Between`, no `HasPrefix`/`HasSuffix`, and no jsonb `Path` — the
list above is the whole vocabulary, and it is generated from the column's type
rather than written by hand.

Compare Ent's free functions (`user.AgeGTE(18)`, a flat namespace of hundreds)
and Bun/GORM's `"age >= ?"`. Methods on typed handles give a smaller namespace,
better autocomplete, and stricter checking than either.

## 4. Reads

`New()` starts a query. The terminals take a `context` and an `Executor`:

```go
rows, err := article.New().
    Where(article.AuthorID.Eq(id), article.PublishedAt.IsNotNull()).
    Order(article.PublishedAt.Desc()).
    Limit(50).
    All(ctx, ex, nil)        // dst: pass a slice to reuse its capacity

one, ok, err := author.New().IDEq(id).One(ctx, ex)
n,      err := author.New().Count(ctx, ex)
ok,     err := author.New().Where(author.Email.Eq(e)).Exists(ctx, ex)
```

The per-column shorthands (`IDEq`, `CreatedAtGte`, …) are generated for every
column, so the common single-predicate read is one call.

`Executor` is a four-method port. A transaction satisfies it, so the same
generated code runs inside one and there are no `XxxTx` duplicates:

```go
tx, _ := pool.Begin(ctx)
txe := pgxdrv.Tx{T: tx}
_, err := na.Insert(ctx, txe)      // same call, inside the transaction
tx.Rollback(ctx)
```

Keyset pagination takes the last row you got back — there is no cursor string to
encode, forge, or decode:

```go
page1, _ := article.New().Order(article.Title.Asc(), article.ID.Asc()).
    Limit(2).All(ctx, ex, nil)
page2, _ := article.New().Order(article.Title.Asc(), article.ID.Asc()).
    After(page1[len(page1)-1]).Limit(2).All(ctx, ex, nil)
```

## 5. Dynamic filters — this is the whole thesis, and it looks boring

```go
q := article.New().Where(article.AuthorID.Eq(id))

q = q.WhereIf(f.PublishedOnly, article.PublishedAt.IsNotNull())
q = q.WhereIf(f.Search != "", article.Title.ILike("%"+f.Search+"%"))
if f.Recent {
    q = q.Where(article.CreatedAt.Gte(cutoff))
}

rows, err := q.Order(article.CreatedAt.Desc()).All(ctx, ex, nil)
```

`Where` is variadic AND; `Any` is OR; `Not` and `NotAny` negate. Each call
appends a token to an inline array — a compiler-generated id for the column and
the operator, never the value. **The assembled SQL for that combination is
compiled once, ever**, and a warm call allocates nothing to build it.

That is also why storm has no injection surface in ordinary reads: there is no
caller string in the statement text to escape. The values travel as bound
arguments, and `internal/planspike/injection_test.go` asserts the property
directly — the SQL is byte-identical whatever the value, which is stronger than
any corpus of payloads because it holds for payloads nobody thought of.

## 6. Relations: named fetch plans

Base row types have **no relation fields at all**:

```go
a, _, _ := author.New().IDEq(id).One(ctx, ex)
a.Articles      // compile error: undefined (type author.Row has no field Articles)
```

To load relations you name a plan, in the model:

```go
func (a *Author) Plans(p *storm.Plans) {
    p.Named("Feed").With(&a.Articles)
}
```

```go
feed, err := store.AuthorFeed().Limit(10).All(ctx, ex)
feed[0].Articles        // []article.Row — exists only on the plan's row type
```

Exactly **two round trips whatever the row count**, and reading an unloaded
relation does not compile. An N+1 is not something you have to remember to
avoid; it is not expressible.

The semi-join — "authors who *have* a published article" — is a generated
function taking the child's own typed predicates:

```go
n, err := store.AuthorHavingArticles(
    author.New(),
    article.PublishedAt.IsNotNull(),
).Count(ctx, ex)
```

And its opposite, the anti-join — "authors with **no** published article":

```go
n, err := store.AuthorNotHavingArticles(
    author.New(),
    article.PublishedAt.IsNotNull(),
).Count(ctx, ex)
```

`NotHaving` means "has no child matching these", not "has a child that does not
match" — a distinction that matters for any parent holding both, and one SQL
spells the same way round.

Both chain, against the same relation, into **one** statement — which is the
upsell query, and the reason having only one half was worth little:

```go
store.AuthorHavingArticles(author.New(), article.PublishedAt.IsNotNull()).
    AndNotHaving(article.Title.Eq("Notes")).
    All(ctx, ex)
```

Two positive probes are satisfied by different child rows, so `AndHaving` reads
as "wrote both of these". Chaining across two different relations is not
offered: the composer type is per relation, so it does not compile.

## 7. Declared reports: projections, aggregations, joins

These are declared in the model rather than chained at the call site, and that is
[principle 4](#design-principles): a `GroupBy(...).Select(...)` chain assembled
at run time has an unbounded set of result shapes, and a shape the generator
never saw can have neither a scanner nor a compiled statement. **Call-site
predicates stay dynamic**, because those are bounded.

**Projections** — a column subset:

```go
func (a *Author) Projections(p *storm.Projections) {
    p.Named("Card", &a.Name, &a.Email)
}

cards, err := author.New().Order(author.Name.Asc()).AllCard(ctx, ex)  // []author.CardRow
```

**Aggregations** — a `GROUP BY` and the expressions over it:

```go
func (o *Order) Aggregates(a *storm.Aggregates) {
    trend := a.Named("Trend")
    day    := trend.ByExpr("Day", a.DateTrunc("day", &o.PlacedAt))
    orders := trend.Count("Orders")
    paid   := trend.Count("Paid").Filter(a.Eq(&o.Status, "paid"))
    rev    := trend.Sum(&o.Total, "Revenue")

    trend.CountDistinct(&o.Customer, "Buyers")

    // NullIf is the division-by-zero guard, written where the division is.
    // Div resolves to NUMERIC: PostgreSQL's `/` on two integers truncates, so
    // this would otherwise be 0 on every day that was not entirely paid.
    trend.Compute("PaidRate", a.Div(paid, a.NullIf(orders, a.Lit(0))))

    // A seven-day moving average — avg(sum(total)) over a frame. Without the
    // frame PostgreSQL reaches back to the start of the partition and this is
    // a RUNNING average, not a moving one.
    trend.AvgOver(rev, "Revenue7d", a.Over().OrderByAsc(day).
        Rows(a.Preceding(6), a.CurrentRow()))

    trend.PercentRank("RevenuePct", a.Over().OrderByDesc(rev))
    trend.Having(a.Gt(orders, 0))
}
```

```go
rows, err := order.New().
    Where(order.PlacedAt.Gte(since)).    // a WHERE: filters rows INTO the groups
    AllTrend(ctx, ex)                    // []order.TrendRow
```

The generated row type carries the nullability the SQL actually has — `Orders
int64` because count over zero rows is 0, `Revenue Null[Decimal]` because sum
over zero rows is NULL, and every grouping column becomes nullable under
`Rollup`/`Cube`/`Sets`, because a subtotal row carries NULL for what it
aggregated over. `GroupingOf` tells that NULL from one that was in the data.

Also available: `Min`/`Max`/`Avg`, `Sum`/`Min`/`Max`Over, `RowNumber`, `Rank`,
`DenseRank`, `CumeDist`, `Lag`, `Lead`, `FirstValue`, `LastValue`, and
`Rollup`/`Cube`/`Sets` with `GROUPING()`.

**Unions** — several tables merged into one stream. A union has no driving
table, so unlike everything else here it is declared as a package-level var
rather than a method ([ADR-0008](adr/0008-union-has-no-driving-table.md)):

```go
var Feed = storm.Union("Feed", func(u *storm.UnionSpec) {
    var a Author
    authors := u.From(&a)
    authors.Take(&a.CreatedAt, "At")
    authors.Take(&a.Name, "Text")
    authors.Const("Kind", "author")          // which branch a row came from

    var ar Article
    articles := u.From(&ar)
    articles.Take(&ar.CreatedAt, "At")
    articles.Take(&ar.Title, "Text")
    articles.Const("Kind", "article")
    articles.Where(storm.Exprs{}.IsNotNull(&ar.PublishedAt))

    u.OrderDesc("At")
})
```

```go
stream, err := store.Feed(ctx, ex, 20)   // the 20 most recent THINGS
```

The ordering and the cap apply to the **merge**, not to each branch — twenty
rows is the twenty most recent, not twenty of each. Every branch must project
the same names in the same order; a column is nullable if **any** branch can
produce NULL there; and `UNION ALL` is the default, because de-duplicating
means sorting the whole result first.

Only the row cap varies per call: a branch filter is declared, so a union is a
global read. A *per-user* feed still needs `storm.SQL`.

**Joins** — a read that projects across tables and returns a flat row:

```go
func (o *Order) Joins(j *storm.Joins) {
    var c Customer
    j.Named("WithCustomer").
        Inner(&c, &o.Customer).           // the FK relation supplies the ON clause
        Take(&o.ID, "OrderID").
        Take(&c.Email, "CustomerEmail").
        Where(j.Ne(&o.Status, "cancelled")).   // a filter the caller cannot widen
        OrderDesc(&o.PlacedAt)
}
```

`With(alias, model, aggregate)` materialises a declared aggregation as a CTE and
joins against it — once, rather than a correlated subquery per row. Anything
taken through a `Left` join comes back nullable, including a count.

## 8. Writes

Insert is a **masked builder**, not a struct. Absence is tracked by the mask and
never inferred from a zero value, so an unset column takes its database default
rather than a Go zero:

```go
na := author.Create()
na.SetName("Ada")
na.SetEmail("ada@example.com")
ada, err := na.Insert(ctx, ex)      // id and created_at come back filled

na.OnConflictEmail()                // ON CONFLICT (email) — generated per unique index
```

Update is the same idea: only what you assigned is written.

```go
m := author.Mutate(ada)
m.SetName("Ada L.")
err := m.Update(ctx, ex)
```

One compiled statement per distinct dirty mask, so a hundred call sites that set
the same two columns share one statement.

## 9. Unit of work — explicit, batched, FK-ordered

Only for graph writes. Everything above works without it.

```go
u := store.NewUnit()
graceID, artID := newID(), newID()

// Staged in ANY order — the article references an author that does not exist yet.
u.Add(article.Table, article.InsertOp(article.Row{
    ID: artID, Title: "Compilers", AuthorID: graceID,
}))
u.Add(author.Table, author.InsertOp(author.Row{
    ID: graceID, Name: "Grace", Email: "grace@example.com",
}))

_, err := u.Flush(ctx, ex)      // one round trip, FK-ordered, atomic
```

The unit sorts statements by the foreign-key dependencies derived from the
schema, then emits one batch. Correct ordering is proven in tests with deferred
constraints **off**, so it is the ordering that is right, not PostgreSQL being
forgiving.

No dirty checking of everything you ever loaded. No flush-order surprises. No
`LazyInitializationException`, because there is no lazy anything.

## 10. The escape hatch loses nothing

```go
var TopAuthors = storm.SQL[AuthorRow](`
    SELECT u.id, u.name, count(p.id) AS posts
    FROM authors u JOIN articles p ON p.author_id = u.id
    WHERE u.org_id = $1
    GROUP BY u.id, u.name
    ORDER BY posts DESC LIMIT $2`)

rows, err := TopAuthors.Query(ctx, ex, orgID, 10)   // []AuthorRow, generated scanner
```

`storm generate` PREPAREs it against the model in a scratch schema, matches the
result descriptor to `AuthorRow`, and emits the scanner. A column with no field
fails the build naming the column. The placeholder count is checked against the
server's, so a `$1` inside a `$tag$` body cannot become a phantom argument.

The no-rows half is `storm.SQLExec`, with one extra rule: the statement must
return **zero** columns, so "I meant to read those rows" is a generation error
rather than a silently dropped result set.

**Only declared statements run.** `storm generate` emits a `RegisterStatement`
for every statement it PREPAREd, and a declaration whose text is not one of them
is refused before the executor is reached:

```go
// Refused at the call, and at generate time: this is not a package-level var.
q := storm.SQL[AuthorRow](fmt.Sprintf("SELECT ... WHERE name = '%s'", userInput))
```

That check exists because `RegisterScanner` keys by **row type** — a scanner
declared for one query would otherwise answer for any query returning that type,
which makes a run-time-assembled string runnable. It is the one place a caller's
string could reach SQL text, and it is closed.

Still out of reach and still `storm.SQL`: recursive CTEs, and anything outside
the scalar-function allow-list.

## 11. When something goes wrong

Constraint violations arrive classified, not as a five-character SQLSTATE you
decode by hand:

```go
if _, err := na.Insert(ctx, ex); err != nil {
    var ce *runtime.ConstraintError
    if errors.As(err, &ce) {
        switch {
        case errors.Is(err, runtime.ErrUniqueViolation):
            return fmt.Errorf("email %q is taken (%s)", email, ce.Constraint)
        case errors.Is(err, runtime.ErrForeignKeyViolation):
            return errUnknownAuthor
        case errors.Is(err, runtime.ErrExclusionViolation):
            return errRoomAlreadyBooked      // the booking-conflict case
        }
    }
    if errors.Is(err, runtime.ErrSerializationFailure) ||
        errors.Is(err, runtime.ErrDeadlock) {
        // Not bugs — this is what concurrency control looks like when it works.
    }
    return err
}
```

`ce.Constraint` is the constraint's name, so the branch can be specific without
matching on message text.

## 12. Testing

There is no mock layer and nothing to fake. `Executor` is the seam: hand your
code a transaction, roll it back, and the generated code is the code that ran in
production.

```go
tx, _ := pool.Begin(ctx)
defer tx.Rollback(ctx)
svc := NewService(pgxdrv.Tx{T: tx})
```

`examples/blog` and `examples/orders` are both structured this way — a real
server, a namespace per test, `-race -shuffle=on` in CI.

## What this costs you

- **A generate step.** Change the model, run `storm generate`, commit the
  output. `storm verify -stale` fails CI if you forget, and needs no database.
- **Declaring your reports.** A shape that is not declared cannot run. That is
  the price of every statement having a compiled form and a generated scanner.
- **No lazy loading, ever.** If you want the relation, name the plan.
- **PostgreSQL.** MySQL has a DDL back end but is not a runtime target
  ([ADR-0007](adr/0007-mysql-runtime-needs-a-second-decoder-family.md)).
