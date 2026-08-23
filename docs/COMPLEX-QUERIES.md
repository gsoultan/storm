---
tags: [raorm, queries, complex]
updated: 2026-08-23
status: proposed — illustrative design, not implemented
---

# Complex queries, as objects

Eight queries a backend engineer actually gets asked for, none of them
expressible in GORM or Ent without dropping to raw SQL. All string-free, all
composable, all compiled at build time.

The argument for objects is not aesthetics. **A string cannot be composed,
reused, unit-tested, or checked.** An object can be returned from a function,
stored in a variable, passed across a package boundary, and reused in five
queries that stay correct when the column is renamed.

---

## 1. SaaS revenue dashboard — MRR by plan, month over month

> *"Monthly recurring revenue per plan, with the change from last month, and how
> many subscriptions were active vs. trialing."*

Date bucketing, conditional aggregation, and a window function over an
aggregate — the three things that usually end a query builder's usefulness.

```go
type MRRRow struct {
    Month     raorm.Date
    PlanName  string
    MRRCents  int64
    Active    int64
    Trialing  int64
    PrevCents raorm.Null[int64]
}

month := raorm.DateTrunc(raorm.Month, sub.StartedAt)

rows, err := sub.Query().
    Join(plan.T, raorm.On(sub.PlanID.EqCol(plan.ID))).
    Where(sub.Tenant.Eq(tid), sub.StartedAt.Gte(since)).
    GroupBy(month, plan.ID, plan.Name).
    Select(raorm.Into[MRRRow](
        month.As(&MRRRow{}.Month),
        plan.Name,
        raorm.Sum(sub.MonthlyCents).As(&MRRRow{}.MRRCents),

        // FILTER (WHERE …) — cleaner and faster than SUM(CASE WHEN …)
        raorm.Count(sub.ID).Filter(sub.Status.Eq(StatusActive)).As(&MRRRow{}.Active),
        raorm.Count(sub.ID).Filter(sub.Status.Eq(StatusTrialing)).As(&MRRRow{}.Trialing),

        // a window OVER an aggregate — last month's MRR for the same plan
        raorm.Lag(raorm.Sum(sub.MonthlyCents)).
            Over(raorm.PartitionBy(plan.ID).OrderBy(month.Asc())).
            As(&MRRRow{}.PrevCents),
    )).
    OrderBy(month.Desc(), plan.Name.Asc()).
    All(ctx, db)
```

`.As(&MRRRow{}.Month)` binds the projection to a **field pointer**, so a rename
of `MRRRow.Month` is a compile error and a column with no field fails
generation. `raorm.Into[MRRRow]` checks the set is complete.

**What it buys:** `FILTER` instead of `SUM(CASE WHEN …)`, and `LAG` over an
aggregate — both of which GORM and Ent can only reach through raw SQL.

---

## 2. Churn risk — accounts whose usage is falling

> *"Accounts on a paid plan whose last-30-day event count is below 60% of the
> 30 days before that, with at least 100 events historically."*

Two CTEs and a `HAVING` over a ratio. The CTE is a **variable**, not a name:

```go
recent := raorm.CTE(event.Query().
    Where(event.OccurredAt.Gte(raorm.NowMinus(30 * raorm.Day))).
    GroupBy(event.AccountID).
    Select(raorm.Into[Bucket](event.AccountID, raorm.Count(event.ID).As(&Bucket{}.N))))

prior := raorm.CTE(event.Query().
    Where(event.OccurredAt.Between(raorm.NowMinus(60*raorm.Day), raorm.NowMinus(30*raorm.Day))).
    GroupBy(event.AccountID).
    Select(raorm.Into[Bucket](event.AccountID, raorm.Count(event.ID).As(&Bucket{}.N))))

ratio := raorm.Div(recent.Col.N, raorm.NullIf(prior.Col.N, 0))

atRisk, err := account.Query().
    With(recent, prior).
    Join(recent, raorm.On(account.ID.EqCol(recent.Col.AccountID))).
    Join(prior,  raorm.On(account.ID.EqCol(prior.Col.AccountID))).
    Where(account.Plan.NotEq(PlanFree), raorm.Gte(prior.Col.N, 100)).
    Having(raorm.Lt(ratio, 0.6)).
    Select(raorm.Into[RiskRow](
        account.ID, account.Name,
        recent.Col.N.As(&RiskRow{}.Recent),
        prior.Col.N.As(&RiskRow{}.Prior),
        ratio.As(&RiskRow{}.Ratio),
    )).
    OrderBy(ratio.Asc()).
    All(ctx, db)
```

`recent` and `prior` are ordinary Go values. Put them in a function, share them
between the dashboard query and the alerting job, and there is one definition of
"a 30-day bucket" in the codebase. `recent.Col.N` is typed.

`raorm.NullIf(prior.Col.N, 0)` is not decoration — it is the division-by-zero
guard, and it is right there in the expression instead of in a comment.

---

## 3. Bought X, never bought Y

> *"Customers who ordered anything from Coffee but have never ordered from
> Equipment — the upsell list."*

A correlated `EXISTS` and a correlated `NOT EXISTS`. Both take a real query
object, and both are reusable:

```go
func OrderedFrom(c raorm.Ref[Category]) raorm.Pred {
    return raorm.Exists(orderitem.Query().
        Join(order.T,   raorm.On(orderitem.OrderID.EqCol(order.ID))).
        Join(product.T, raorm.On(orderitem.ProductID.EqCol(product.ID))).
        Where(
            order.CustomerID.EqCol(customer.ID),   // the correlation
            product.CategoryID.Eq(c),
            order.Status.Eq(StatusFulfilled),
        ))
}

upsell, err := customer.Query().
    Where(
        customer.Tenant.Eq(tid),
        OrderedFrom(CategoryCoffee),
        raorm.Not(OrderedFrom(CategoryEquipment)),
    ).
    Load(CustomerCard).
    All(ctx, db)
```

`OrderedFrom` is a **function returning a predicate**. That is the composability
argument in one line: the same definition drives the upsell list, a segment
filter, and a permission check, and renaming `product.CategoryID` breaks all
three at compile time.

---

## 4. Double-booking prevention

> *"Is this room free for this window? And make it impossible to book it twice
> even under concurrency."*

Range overlap, plus the constraint that makes the check redundant:

```go
window := raorm.TstzRange(from, to, raorm.HalfOpen)

conflict, err := booking.Query().
    Where(
        booking.Room.Eq(roomID),
        booking.Status.NotEq(StatusCancelled),
        booking.Period.Overlaps(window),        // && operator
    ).
    Exists(ctx, db)
```

The query is the friendly error. The **exclusion constraint** is the correctness
guarantee, declared in the model so it cannot be forgotten:

```go
func (b *Booking) Schema(t *raorm.Table) {
    t.Exclude(
        raorm.With(&b.Room, raorm.OpEq),
        raorm.With(&b.Period, raorm.OpOverlaps),
    ).Where(raorm.NotEq(&b.Status, StatusCancelled))
}
```

That generates `EXCLUDE USING gist (room_id WITH =, period WITH &&) WHERE
(status <> 'cancelled')`. Two concurrent bookings cannot both commit, and the
insert returns a typed `raorm.ErrExclusion` you can map to a 409.

**No other Go ORM models exclusion constraints.** They are the correct answer to
booking, scheduling, and rate-plan overlap, and they are unreachable without one.

---

## 5. Faceted search — results and facet counts in one round trip

> *"Search products, and give me the counts per category and per brand for the
> filters the user has *not* applied yet."*

The classic mistake is N+1 queries for N facets. `GROUPING SETS` does it once:

```go
base := raorm.CTE(product.Query().
    Where(
        product.Tenant.Eq(tid),
        product.Search.Matches(raorm.English, q),
        raorm.WhenSet(f.MinPrice, product.PriceCents.Gte),
        raorm.WhenSet(f.BrandID,  product.BrandID.Eq),
    ))

facets, err := base.Query().
    GroupBy(raorm.GroupingSets(
        raorm.Set(base.Col.CategoryID),
        raorm.Set(base.Col.BrandID),
        raorm.Set(),                             // the grand total row
    )).
    Select(raorm.Into[Facet](
        base.Col.CategoryID, base.Col.BrandID,
        raorm.Count(raorm.Star()).As(&Facet{}.N),
    )).
    All(ctx, db)

page, err := base.Query().
    OrderBy(raorm.TSRank(base.Col.Search, q).Desc(), base.Col.ID.Asc()).
    Page(ctx, db, raorm.After(cursor, 24))
```

`raorm.WhenSet(f.MinPrice, product.PriceCents.Gte)` applies the predicate only
when the pointer is non-nil — the dynamic-filter idiom without an `if`, and it
still sets exactly one bit in the shape mask.

---

## 6. Permission inheritance down an org tree

> *"Every account reachable from this org, including sub-orgs, and which
> ancestor granted the access."*

Recursive CTE with a path and a cycle guard:

```go
tree := raorm.RecursiveCTE[OrgNode](
    // anchor
    org.Query().Where(org.ID.Eq(rootID)).
        Select(raorm.Into[OrgNode](
            org.ID, org.Name,
            raorm.Const(0).As(&OrgNode{}.Depth),
            raorm.ArrayOf(org.ID).As(&OrgNode{}.Path),
        )),
    // recursive term
    func(self raorm.Ref[OrgNode]) raorm.Query {
        return org.Query().
            Join(self, raorm.On(org.ParentID.EqCol(self.Col.ID))).
            Where(
                raorm.Lt(self.Col.Depth, 10),                    // depth bound
                raorm.Not(self.Col.Path.Contains(org.ID)),        // cycle guard
            ).
            Select(raorm.Into[OrgNode](
                org.ID, org.Name,
                raorm.Add(self.Col.Depth, 1).As(&OrgNode{}.Depth),
                raorm.ArrayAppend(self.Col.Path, org.ID).As(&OrgNode{}.Path),
            ))
    },
)

rows, err := account.Query().
    With(tree).
    Join(tree, raorm.On(account.OrgID.EqCol(tree.Col.ID))).
    Select(raorm.Into[Reachable](account.ID, account.Email, tree.Col.Name, tree.Col.Depth)).
    OrderBy(tree.Col.Depth.Asc()).
    All(ctx, db)
```

The depth bound and the cycle guard are **required** by the API — a
`RecursiveCTE` with neither fails generation. A recursive query that can loop
forever is not a query, it is an outage.

For the plain case, §4.7 of [[REFERENCE]] gives you `org.Query().Descend(...)`
and this whole block is unnecessary.

---

## 7. Activity feed from heterogeneous sources

> *"One reverse-chronological feed of comments, follows, and releases."*

`UNION ALL` across three shapes, typed as a sum:

```go
feed, err := raorm.UnionAll[FeedItem](
    comment.Query().Where(comment.Author.Eq(uid)).
        Select(raorm.Into[FeedItem](
            raorm.Const(KindComment).As(&FeedItem{}.Kind),
            comment.ID, comment.CreatedAt, comment.Body.As(&FeedItem{}.Text))),

    follow.Query().Where(follow.Follower.Eq(uid)).
        Join(user.T, raorm.On(follow.TargetID.EqCol(user.ID))).
        Select(raorm.Into[FeedItem](
            raorm.Const(KindFollow).As(&FeedItem{}.Kind),
            follow.ID, follow.CreatedAt, user.Name.As(&FeedItem{}.Text))),

    release.Query().Where(release.Project.In(subscribed...)).
        Select(raorm.Into[FeedItem](
            raorm.Const(KindRelease).As(&FeedItem{}.Kind),
            release.ID, release.PublishedAt, release.Tag.As(&FeedItem{}.Text))),
).
    OrderBy(raorm.Col(&FeedItem{}.CreatedAt).Desc()).
    Page(ctx, db, raorm.After(cursor, 50))
```

Every branch must project into `FeedItem` with matching types, checked at
generation. Add a fourth source with a wrong column and the build fails, not
the endpoint.

---

## 8. Gap-filled time series

> *"Daily signups for the last 90 days — including the days with none."*

The bug in every hand-rolled version of this is that days with zero rows vanish
and the chart lies. `generate_series` fixes it:

```go
days := raorm.GenerateSeries(raorm.NowMinus(90*raorm.Day), raorm.Now(), raorm.Day)

series, err := days.Query().
    LeftJoinLateral(
        user.Query().
            Where(user.Tenant.Eq(tid), user.CreatedAt.WithinDay(days.Col.Day)).
            Select(raorm.Into[DayCount](raorm.Count(user.ID).As(&DayCount{}.N))),
    ).
    Select(raorm.Into[Point](
        days.Col.Day.As(&Point{}.Day),
        raorm.Coalesce(raorm.Col(&DayCount{}.N), 0).As(&Point{}.N),
    )).
    OrderBy(days.Col.Day.Asc()).
    All(ctx, db)
```

---

## What makes these work

Four properties, none of which a string query has:

**Composable.** `OrderedFrom(category)` is a function returning a predicate.
`recent` is a CTE in a variable. Both cross package boundaries and get reused.

**Checked.** `.As(&MRRRow{}.Month)` is a field pointer. Rename the field, or the
column, and the build breaks. A projection missing a field fails generation.

**Testable.** A predicate is a value, so you can assert on the SQL it lowers to
in a unit test — no database needed.

**Compiled.** Every query on this page renders its SQL once per shape, at build
time. The dashboard in §1 does not rebuild that window function on every
request the way GORM and Bun do.

## And when they do not

Some SQL is not worth modelling. `raorm.SQL[T]` takes it, `PREPARE`s it at
generate time, and gives you a generated scanner — see [[EXAMPLE]] §7. Reach for
it when the object form would be longer than the SQL, and note that even then
you keep the typed result and the compile-time column check.

The line: **if it composes or gets reused, make it an object; if it is one
gnarly report that will never be reused, write the SQL.**
