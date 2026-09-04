---
tags: [storm, queries, complex]
updated: 2026-09-04
status: as-built — the declarations here build; the gaps are marked as gaps
---

# Eight queries a backend engineer actually gets asked for

The question this page exists to answer is not "is storm expressive" but
**"where is the line?"** — which of these is a declaration, and which sends you
back to SQL.

Six of the eight are declarations today. Two are not, and saying which is more
useful than a page of examples that all happen to work.

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

## 2. Churn risk — accounts whose usage is falling ❌

> *"Accounts whose last-30-day event count is below 60% of the 30 days before
> that."*

**Still `storm.SQL[T]`**, and the reason is precise: a `FILTER` condition is part
of the declaration, so it is fixed at generate time. "The last 30 days" is
relative to *when the query runs*, and there is no way to say that in a
declared filter.

The arithmetic itself is no longer the obstacle — `a.Div(recent,
a.NullIf(prior, a.Lit(0)))` is a declaration and resolves to numeric, not to
truncated integer division. It is the moving boundary that does not fit.

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

**The gap:** results and facets are two round trips, not one. Combining them
needs a `UNION` of two shapes, and storm generates no `UNION`.

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

## 7. Activity feed from heterogeneous sources ❌

> *"One reverse-chronological feed of comments, follows, and releases."*

**Still `storm.SQL[T]`.** This is a `UNION ALL` of three different tables into
one row shape, and storm generates no `UNION`. Polymorphism helps with the
*storage* side — `storm.OneOfN` and `storm.AnyRef` in [[REFERENCE]] §6 — but not
with merging three tables into one stream.

## 8. Gap-filled time series ❌

> *"Daily signups for the last 90 days — including the days with none."*

**Still `storm.SQL[T]`.** Gap-filling needs `generate_series`, and the
scalar-function allow-list has `date_trunc`, `coalesce`, `nullif`, `abs`,
`lower` and `upper` — a set-returning function is a different thing entirely.

`AllDaily` gives you the days that *have* rows; the empty ones have to come from
somewhere, and today that is SQL.

---

## Where the line actually is

Declarable: anything that is **one table's rows, grouped** — with expressions,
`FILTER`, `HAVING`, grouping sets, windows and frames over them — plus joins
that project across tables, and a CTE that materialises one aggregation for
another to read. Plus the traversals: relations, semi-joins, self-references.

Not declarable, and each for a reason rather than an oversight:

| Missing | Why |
|---|---|
| `UNION` | two shapes into one row type is a shape the generator has not been asked to name |
| probes across two DIFFERENT relations | one child column range, so the lowering cannot route two child packages |
| set-returning functions | `generate_series` is not a scalar function |
| run-time-relative filters | a declared `FILTER` is fixed at generate time |
| recursive CTEs you write yourself | `Descend`/`Ascend` cover the self-reference; anything else is SQL |

And when a query is not declarable, `storm.SQL[T]` is not a downgrade: the
statement is still PREPAREd against the model at generate time, the row type
still gets a generated scanner, and a column that drifts still fails the build
naming the column. What you lose is the composable predicate, not the typing.
