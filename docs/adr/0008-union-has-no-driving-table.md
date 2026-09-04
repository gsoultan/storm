# ADR-0008 — UNION has no driving table, and the column-id space is the real ceiling

**Status:** Accepted · 2026-09-04 · **partly implemented** — see "What shipped"
**Context for:** [COMPLEX-QUERIES](../COMPLEX-QUERIES.md) §5 and §7, [ADR-0004](0004-mongodb-as-backend.md)

## Context

Two of the eight scenarios in [COMPLEX-QUERIES](../COMPLEX-QUERIES.md) are still
`storm.SQL[T]`, and both are a `UNION`:

- **the activity feed** — comments, follows and releases in one
  reverse-chronological stream;
- **faceted search** — the result page and the facet counts in one round trip
  rather than two.

Every other gap closed this quarter turned out cheaper than it looked, because
the machinery already existed: the anti-join reused `pgsql.NotExistsFrag`, and
chaining probes reused a relation id the tokens had carried since M2. `UNION`
reuses nothing, and it is worth writing down why before someone starts.

### What a declared read looks like today

Every cross-table read storm generates hangs off a **driving table**. A declared
join is a method on that table's query:

```go
order.New().Where(order.PlacedAt.Gte(since)).AllVsLifetime(ctx, ex)
```

`Joins` is declared on the driving model and names the far side; call-site
predicates apply to the driving table, and the generated scanner belongs to its
package. Aggregations, projections and plans are all the same shape.

A union has **no driving table**. In a feed of comments, follows and releases,
none of the three is the one the others hang off — declaring it on `Comment`
because it sorts first alphabetically is arbitrary, and it puts the row type in
a package that has no more claim to it than the other two.

### The ceiling nobody has hit yet

`Tok`'s column field is ten bits, and `runtime.ChildColBase = 512` splits it in
half: the parent's columns below, one child's above. The composite lowering
routes on that boundary alone:

```go
lw.Frag = func(op, col uint32) runtime.Frag {
	if col >= runtime.ChildColBase {
		return child.FragOf(op, col-runtime.ChildColBase)
	}
	return parentFrag(op, col)
}
```

**Two ranges. One parent, one child.** That is why chained existence probes are
restricted to a single relation — two probes against different relations would
rebase both children into the same range and the lowering could not tell one
child package's fragments from the other's.

A three-branch union with call-site predicates on each branch wants *four*
ranges. So this ADR and the cross-relation probe chain are the same problem
wearing different clothes, and whatever answer we pick should serve both.

## Decision

Three questions, and none of them is "how do we render `UNION`" — the SQL is the
easy part.

### 1. Where does the declaration live? — a package-level var, like `storm.SQL`

Not on a model. A union belongs to no table, and forcing it onto one puts the
row type in an arbitrary package.

`storm.SQL[T]` already solves exactly this problem: a package-level `var`, found
by discovery, validated at generate time, with a scanner emitted for its row
type. A union should be the same kind of value — **sketch, none of this exists**:

```go
var Feed = storm.Union[FeedRow](
    storm.From(&Comment{}).Take(...),
    storm.From(&Follow{}).Take(...),
    storm.From(&Release{}).Take(...),
).OrderDesc("OccurredAt")
```

— with the difference that it is checked against the **model** rather than
PREPAREd as a string, so a column that drifts fails the build naming the column
and the branch. Discovery already walks package-level vars for `storm.SQL` and
`storm.SQLExec`; this reuses that pass rather than adding one.

### 2. Where do call-site predicates go? — nowhere, in v1. Declared parameters instead

A predicate on a three-branch union is ambiguous — does `Where(x)` filter one
branch or all of them? — and answering it needs the column-id split above.

The feed's real requirement is narrower than a predicate: it needs **one value**,
the actor, applied to every branch. That is a parameter, not a filter, and
`storm.SQL` already has the shape:

```go
rows, err := Feed.Query(ctx, ex, actorID, 50)
```

Each branch's declared filter refers to a declared parameter; the call supplies
values positionally; the statement text never varies. That keeps a union inside
the compilation thesis — one shape, one compiled statement, one scanner —
without deciding the column-space question first.

**Per-branch dynamic predicates are explicitly deferred**, and doing them means
splitting the ten-bit column field into four ranges of 256. That is a real
change: it re-bases every generated package, touches the fuzz corpus, and caps a
table at 256 columns. Worth doing when two features need it; not worth doing for
one that has not shipped.

### 3. How does paging work? — the sort key must be projected, or it is refused

`ORDER BY` and `LIMIT` apply to the union as a whole, which is what makes it one
round trip rather than three.

Keyset paging already takes a typed row rather than an encoded cursor
(`After(r Row)`), so it works here **if and only if** the ordering columns are
part of the projected shape. So: every branch must project the declared sort
key, and a union whose ordering is not a strict total order over projected
columns is refused at generate time — the same rule `ChildTop` already enforces
for per-parent ordering, and for the same reason.

That is a constraint worth having rather than a limitation to apologise for: a
feed ordered by a column two of its three branches do not have is not a feed.

### `UNION ALL` by default

`UNION` de-duplicates, which means sorting or hashing the entire result before
returning a row. A feed never wants that, and neither does a facet count. The
default is `ALL`; `.Distinct()` opts into the expensive one.

**This is the decision most likely to surprise**, because it inverts SQL's
default. It is named here so the generated statement can carry a comment saying
so, rather than being discovered from a row count.

## Alternatives considered

**Do nothing; `storm.SQL[T]` covers it.** The honest baseline, and it is not
bad: the statement is still PREPAREd against the model, the row type still gets
a generated scanner, a drifted column still fails the build. What is lost is
composability, and for a feed — where the only variable is the actor and the
page — that loss is small. *If the parameter design in §2 is where we land, this
alternative is close to free, which is an argument for shipping the raw form and
watching whether anyone wants more.*

**A `Unions` method on one participating model.** Consistent with `Joins`, and
rejected: it makes one of three equal tables special, and puts the row type in
its package.

**A union of two *queries* rather than two tables.** Reads well
(`a.Union(b)`), and is the shape a query builder would take — but it is a
call-site chain, so the result shape is unbounded and there is no scanner to
generate. Same reason `GroupBy(...).Select(...)` is refused.

## What shipped (2026-09-04)

Built as decided, with one part of §2 deferred:

- ✅ **Package-level var, resolved during Build.** `storm.Union(name, func(*UnionSpec))`,
  passed to `Build` alongside the models and set aside there. Discovery finds
  the var the same pass it finds `storm.SQL`.
- ✅ **The sort key must be projected**, and a union with no ordering is
  refused — a merged bag of rows with a `LIMIT` over it returns an arbitrary
  subset that differs between runs.
- ✅ **`UNION ALL` by default**, `Distinct()` to opt in.
- ✅ **Branch filters are declared**, and cannot be widened at a call site.
- ✅ Types widen across branches the way PostgreSQL widens them (text beside
  varchar is text); an enum beside a varchar is refused, because the server
  refuses it too. Nullability ORs: a column is nullable if ANY branch can
  produce NULL there, or one branch's NULL decodes as another's zero value.
- ❌ **Declared parameters are NOT built.** The only value a call site supplies
  is the row cap. That means the motivating query — *this actor's* feed — is
  still out of reach: a branch filter is a constant, so "where actor = $1"
  cannot be said.

The last point matters more than the four ticks above it. A union that cannot
be parameterised is a global feed, and most feeds are somebody's. It was left
out because parameters are a separable piece of work and the merge, the
ordering and the shape checking are not — but nobody should read this ADR as
saying the activity feed is done.

## Consequences

- A fourth declared-read kind, alongside plans, projections, aggregations and
  joins. That is real surface: `schema/union.go`, a `compile/pgsql` renderer,
  a `codegen` emitter, discovery, and `storm explain` coverage.
- The row type lives in the declaring package, not a table package — the first
  generated scanner that does. `storm.SQL` already works this way, so the
  precedent exists.
- **The column-id split is deferred, not avoided.** Cross-relation probe chains
  want it too. When the second feature needs it, do it once and re-base both.
- Cost is comparable to `Joins`, which is the largest single feature in the
  compiler so far. It should not land in the same release as a fail-closed
  security change.
