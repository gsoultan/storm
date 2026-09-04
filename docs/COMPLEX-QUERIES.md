---
tags: [storm, queries, complex]
updated: 2026-09-05
status: as-built — the declarations here build; the gaps are marked as gaps
---

# Eight queries a backend engineer actually gets asked for

The question this page exists to answer is not "is storm expressive" but
**"where is the line?"** — which of these is a declaration, and which sends you
back to SQL.

Six of the eight are declarations, one is half, and one is not. Saying which is
more useful than a page of examples that all happen to work.

> **How the declarations differ from a query builder.** They live in
> `Aggregates` and `Joins` methods on the model, not in a chain at the call
> site. A `GroupBy(...).Select(...)` assembled at run time has an unbounded set
> of result shapes, and a shape the generator never saw can have neither a
> scanner nor a compiled statement. Call-site **predicates** stay dynamic,
> because those are bounded. See [[API]] §7.

---

## 1. SaaS revenue dashboard — MRR by plan, month over month ✅

> *"Monthly recurring revenue per plan, with the change from last month, and how
> many subscriptions are active versus trialing."*

Date bucketing, conditional counts, and a window over an aggregate — the three
things that usually end a query builder's usefulness.

```go
func (s *Subscription) Aggregates(a *storm.Aggregates) {
    m     := a.Named("MRR")
    month := m.ByExpr("Month", a.DateTrunc("month", &s.StartedAt))
    m.By(&s.Plan)

    mrr := m.Sum(&s.MonthlyCents, "MRRCents")
    m.Count("Active").Filter(a.Eq(&s.Status, "active"))
    m.Count("Trialing").Filter(a.Eq(&s.Status, "trialing"))

    // Last month's MRR for the SAME plan: a window over an aggregate.
    m.Lag(mrr, "PrevCents", a.Over().PartitionBy(&s.Plan).OrderByAsc(month))

    // That month's total across every plan, beside each plan's own — the
    // share-of-group query, with no self-join.
    m.SumOver(mrr, "MonthTotal", a.Over().PartitionBy(month).
        Rows(a.UnboundedPreceding(), a.UnboundedFollowing()))
}
```

The plan's *name* lives in another table, so the aggregation becomes a CTE and
the join reads from it — computed once, not once per row:

```go
func (s *Subscription) Joins(j *storm.Joins) {
    var p Plan
    j.Named("MRRByPlan").
        With("mrr", &Subscription{}, "MRR").
        Inner(&p, &s.Plan).
        LeftWith("mrr", j.OnCols("mrr", "plan_id", &p.ID)).
        Take(&p.Name, "PlanName").
        TakeFrom("mrr", "Month", "Month").
        TakeFrom("mrr", "MRRCents", "MRRCents").
        TakeFrom("mrr", "PrevCents", "PrevCents")
}
```

**What it buys:** `FILTER` instead of `SUM(CASE WHEN …)`, and `LAG` over an
aggregate — both of which GORM and Ent can only reach through raw SQL.

## 2. Churn risk — accounts whose usage is falling ✅

> *"Accounts whose last-30-day event count is below 60% of the 30 days before
> that."*

A `FILTER` is part of the declaration, so its condition is fixed at generate
time — and "the last 30 days" is relative to when the query runs. A **declared
parameter** is what closes that gap:

```go
rate  := a.Named("PaidRate")
since := rate.Param("Since")
rate.By(&o.Status)
recent := rate.Count("Recent").Filter(a.Gte(&o.PlacedAt, since))
rate.Count("RecentPaid").Filter(a.And(
    a.Gte(&o.PlacedAt, since),
    a.Eq(&o.Status, "paid"),
))
rate.Having(a.Gt(recent, 0))
```

```go
rows, err := order.New().
    Where(order.Region.Eq(r)).                       // call-site predicates still compose
    AllPaidRate(ctx, ex, time.Now().AddDate(0, 0, -30))
```

The parameter is `$1`, in the statement's fixed prefix where the `FILTER`
lives; the call-site predicates are numbered from `$2`. Its Go type is inferred
from the column it is compared with.

The ratio itself was never the obstacle: `a.Div(recent, a.NullIf(prior,
a.Lit(0)))` is a declaration and resolves to numeric rather than truncated
integer division.

## 3. Bought X, never bought Y ✅

> *"Customers who ordered from Coffee but never from Equipment — the upsell
> list."*

One statement, one `EXISTS` and one `NOT EXISTS`, both against the same
relation and both taking the child's own typed predicates — no join fan-out, no
`DISTINCT`:

```go
upsell, err := store.CustomerHavingOrders(
    customer.New(), order.Category.Eq(coffee)).
    AndNotHaving(order.Category.Eq(equipment)).
    All(ctx, ex)
```

`AndHaving` chains too, and the probes are independent: two positive probes are
satisfied by *different* child rows, which is what "ordered both of these" means.

**The limit is one relation per chain**, and it is enforced by the type rather
than documented. Two probes against different relations would rebase both
children past the same `runtime.ChildColBase`, and the composite lowering routes
on that range alone — it could not tell one child package's fragments from the
other's. So `AndHaving` exists only on the composer for the relation you
started with, and "has orders but no refunds" is still two round trips.

**Read the anti-join carefully.** `NotHaving` means *"has no child row matching
these predicates"*, not *"has a child row that does not match"*. The two differ
for any customer holding both kinds of order, and SQL spells them the same way
round — which is why the generated doc comment says so at the call site. With no
predicates at all it means "has none".

## 4. Double-booking prevention ✅

> *"Is this room free for this window? And make it impossible to book it twice
> even under concurrency."*

The check is a query; the *guarantee* is a constraint, and only one of those
survives two callers arriving at once:

```go
func (b *Booking) Schema(t *storm.Table) {
    t.Exclude(
        storm.With(&b.Room, storm.OpEq),
        storm.With(&b.During, storm.OpOverlaps),
    )
}
```

```go
free, err := booking.New().
    Where(booking.Room.Eq(roomID), booking.During.Overlaps(window)).
    Exists(ctx, ex)
```

A losing writer gets `runtime.ErrExclusionViolation` — a classified error, not a
SQLSTATE to decode. Storing two timestamps and checking in Go loses the boundary
cases and races anyway.

## 5. Faceted search — results and facet counts ⚠️

> *"Search products, and give me the counts per category and per brand."*

The facets are one declaration and one pass — `GROUPING SETS`, rather than one
query per facet:

```go
func (p *Product) Aggregates(a *storm.Aggregates) {
    f := a.Named("Facets")
    f.By(&p.Category)
    f.By(&p.Brand)
    f.Sets([]string{"Category"}, []string{"Brand"}, nil)   // and the grand total
    f.Count("N")
    f.GroupingOf("CategoryIsSubtotal", &p.Category)
}
```

```go
facets, err := product.New().
    Where(product.Search.Matches(q)).             // the same filter as the page
    AllFacets(ctx, ex)
```

Every grouping column is nullable here, because a subtotal row carries NULL for
what it aggregated over, and `GroupingOf` tells that NULL from one in the data.

**The gap:** results and facets are two round trips, not one. A union could
merge them, but a union's branches must project the **same shape** — and a
product row and a facet count are not the same shape. Making them one means
padding both into a widest-common row of mostly-NULL columns, which is a thing
you can do in SQL and not a thing worth generating.

## 6. Permission inheritance down an org tree ✅

> *"Every org reachable from this one, including sub-orgs."*

A self-referential foreign key generates two traversals, each a single
`WITH RECURSIVE`:

```go
subtree, err := org.Descend(ctx, ex, [][16]byte{rootID}, 10)
```

The depth bound is **required**. A foreign key does not stop A pointing at B
pointing at A, and the generated statement additionally carries a path array and
refuses a row already on it — so a cycle in the data cannot hang the connection,
which is the failure mode a hand-written recursive CTE usually ships with.

Rows come back unordered, on purpose: a tree has no total order, and every row
carries its `parent_id` for the caller to reassemble.

## 7. Activity feed from heterogeneous sources ✅

> *"One reverse-chronological feed of comments, follows, and releases."*

The merge is a declaration now. A union has no driving table, so it is declared
as a package-level var rather than on one of the models
([ADR-0008](adr/0008-union-has-no-driving-table.md)):

```go
var Activity = storm.Union("Activity", func(u *storm.UnionSpec) {
    var o Order
    orders := u.From(&o)
    orders.Take(&o.PlacedAt, "At")
    orders.Take(&o.UpdatedBy, "Actor")
    orders.Const("Kind", "order")
    orders.Where(storm.Exprs{}.Ne(&o.Status, "cancelled"))

    var b Booking
    bookings := u.From(&b)
    bookings.Take(&b.CreatedAt, "At")
    bookings.Take(&b.Guest, "Actor")
    bookings.Const("Kind", "booking")

    u.OrderDesc("At")
})
```

```go
recent, err := store.Activity(ctx, ex, 20)   // the 20 most recent THINGS
```

The ordering and the cap apply to the **merge**, which is the whole point: 20
rows is the twenty most recent events, not twenty of each source. `Const` is how
a row says which branch it came from — without it the sources are
indistinguishable once merged.

A **declared parameter** narrows it to one actor. It reaches every branch that
names it as the same placeholder, so "this actor's feed" cannot accidentally
mean two different actors:

```go
actor := u.Param("Actor")
orders.Where(storm.Exprs{}.Eq(&o.UpdatedBy, actor))
bookings.Where(storm.Exprs{}.Eq(&b.Guest, actor))
```

```go
recent, err := store.Activity(ctx, ex, actorID, 20)
```

The parameter's Go type is inferred from the column it is compared with, so the
signature cannot disagree with what it filters. Polymorphism helps with the
*storage* side — `storm.OneOfN` and `storm.AnyRef` in [[REFERENCE]] §6 — but not
with merging three tables into one stream.

## 8. Gap-filled time series ❌

> *"Daily signups for the last 90 days — including the days with none."*

**Still `storm.SQL[T]`.** `AllDaily` gives the days that *have* rows; the empty
ones were never in the data.

Producing them needs `generate_series`, and adding it beside `date_trunc` in the
scalar-function allow-list does not work — those are **scalar** functions, called
on a value and returning a value, while this one returns *rows*. A row source
that is not a table is a different thing in every layer, and every read storm
generates today starts `FROM <table>`.

Two designs, both real work, neither started:
[ADR-0009](adr/0009-gap-filling-needs-a-from-that-is-not-a-table.md).

---

## 9. Top N by a measure — "the ten customers who spend the most" ✅

A grouped read can only be ordered by something in its select list, so until an
output could be the sort key the only orderings available were the ones the
grouping already gave. `OrderDesc` takes the handle the measure returned:

```go
func (o *Order) Aggregates(a *storm.Aggregates) {
    top := a.Named("TopCustomers")
    top.By(&o.Customer)
    spend := top.Sum(&o.Total, "Spend")
    top.Count("Orders")
    top.OrderDesc(spend)
}
```

```go
rows, err := order.New().
    Where(order.PlacedAt.Gte(monthStart)).   // call-site predicates still compose
    Limit(10).
    AllTopCustomers(ctx, ex)
```

```sql
... GROUP BY "customer_id" ORDER BY "spend" DESC, "customer_id" LIMIT $2
```

**The grouping columns are appended as a tiebreak.** A measure is not unique,
and a top-N report is exactly the query that pages: `LIMIT 10 OFFSET 10` over
groups that tie is otherwise free to return one on both pages and another on
neither. The handle has to come from the same declaration — ordering by another
aggregation's output is a build error, not a column PostgreSQL cannot find.

## 10. An advanced-search screen — OR across whole conditions ✅

Each row of a filter panel is a field, an operator and a value, and rows within
a group are ANDed while the groups are ORed. `Any` ORs *single* predicates and
cannot say this; `And` builds a conjunction that `AnyOf` ORs with the others:

```go
rows, err := order.New().
    Where(order.Region.Eq(r)).                 // ANDed with the whole disjunction
    AnyOf(
        order.And(order.Status.Eq("paid"), order.Total.Gte(big)),
        order.And(order.Status.Eq("trial"), order.Total.Gte(small)),
    ).
    All(ctx, ex, nil)
```

```sql
WHERE "region" = $1 AND (("status" = $2 AND "total" >= $3) OR ("status" = $4 AND "total" >= $5))
```

An empty group contributes nothing, so a screen that builds one group per
filled-in row needs no special case for the rows left blank, and a group of one
predicate is that predicate — the SQL carries no parentheses it did not need,
so it caches under the same shape as the equivalent `Where`. `NotAnyOf` negates
the disjunction. Composing one still allocates nothing.

**The budgets are the limit here, not the grammar.** A generated `Query` holds
its predicate tree in fixed buffers so a warm call can build its SQL without
allocating — sixteen predicate nodes, four sort terms and six values of the
commonest type by default. Past them the query returns an error rather than
dropping a predicate. A screen that needs more regenerates with
`codegen.Budgets{Scale: 2}`, which doubles every buffer; the cost is the size of
the `Query` value every builder call copies.

## Where the line actually is

Declarable: anything that is **one table's rows, grouped** — with expressions,
`FILTER`, `HAVING`, grouping sets, windows and frames over them, and an
ordering over any of it — plus joins that project across tables, and a CTE that
materialises one aggregation for another to read. Plus the traversals:
relations, semi-joins, self-references. At the call site, predicates compose to
arbitrary AND/OR/NOT structure within the generated budgets.

Not declarable, and each for a reason rather than an oversight:

| Missing | Why |
|---|---|
| probes across two DIFFERENT relations | one child column range, so the lowering cannot route two child packages |
| set-returning functions | a row source that is not a table — [ADR-0009](adr/0009-gap-filling-needs-a-from-that-is-not-a-table.md) |
| recursive CTEs you write yourself | `Descend`/`Ascend` cover the self-reference; anything else is SQL |
| set-based `UPDATE`/`DELETE … WHERE` | writes are per row or batched per row; a bulk state transition is `storm.SQLExec` |
| row locking — `FOR UPDATE`, `SKIP LOCKED` | a queue worker's read is `storm.SQL[T]`; the version column covers the lost update, not the queue |
| streaming a result set | reads are `All`/`One`/`Count`/`Exists`; an export pages with `After` |
| jsonb path extraction — `->>`, jsonpath | containment and key tests are declared; asking about a nested scalar is SQL |

And when a query is not declarable, `storm.SQL[T]` is not a downgrade: the
statement is still PREPAREd against the model at generate time, the row type
still gets a generated scanner, and a column that drifts still fails the build
naming the column. What you lose is the composable predicate, not the typing.
