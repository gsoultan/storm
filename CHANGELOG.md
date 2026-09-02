---
tags: [storm, releases]
updated: 2026-08-28
---

# Changelog

Versions follow [semver](https://semver.org). Until v1.0 the *generated* API
may change with a minor bump; what is promised, and for how long, is
[docs/STABILITY.md](docs/STABILITY.md).

Every entry names what changed and — where it matters — what it cost, because
a release note that cannot be checked is marketing.

## v0.4.0 — 2026-09-02

> **If you are on v0.3.0 and any query carries two list predicates, it is
> returning the wrong rows right now.** No error, no warning. See the first
> entry below. Upgrading is the fix; there is no workaround short of splitting
> the query.
>
> **This release is breaking.** The declaration vocabulary moved off the root
> package onto the builder (`storm.Eq(...)` → `a.Eq(...)`), and declared
> aggregate outputs return handles instead of being named by string. Models
> written against v0.1–v0.3 need those two edits. `storm.Expr` still compiles —
> it is a deprecated alias for `storm.RawSQL`. `docs/STABILITY.md` binds from
> v1.0.0, which is why these changes are being made now rather than after.

### Fixed: two list predicates in one query returned the wrong rows

Present in every version through v0.3.0, silent, and a wrong answer rather than
an error:

```go
product.New().Where(product.Sku.In("a", "b"), product.Name.In("x", "y"))
```

`Query` held ONE field per list slot, so the second `In` overwrote the first,
and the bind loop then appended that one list for both placeholders. The query
ran. It returned the wrong rows. Nothing reported anything.

List values are an arena now — bounded, cursor-indexed in token order, exactly
like every scalar value type, which is what makes more than one of them
representable at all. Past the bound the query errors rather than binding
whatever was there.

Found by writing a test for a NEW feature (an array predicate beside a scalar
one) and getting zero rows, then checking whether the same shape was already
broken on `main`. It was.

### Predicates for types that were storable but not filterable

**`In` and `NotIn` on integer columns.** `opApplies` said integers supported
`In`; `predArraySlot` had no integer slot, so the method was silently skipped.
`WHERE code IN (1,2,3)` could not be written on any integer column, and nothing
said so. Each width gets its own slot — binding an `int16` list as `int64`
hands PostgreSQL an `int8[]` to compare against an `int2` column, a cast the
planner must undo before it can use an index.

**`Contains`, `ContainedBy` and `Overlaps` on array columns** (`text[]`,
`uuid[]`, `int8[]`, `numeric[]`) — `@>`, `<@`, `&&`. An array used to
round-trip and support only `IS NULL`: storable, not filterable, which is worse
than an unsupported type because the column looks supported. Equality is still
refused; `tags = '{a,b}'` is order- and duplicate-sensitive.

**`NotIn`** everywhere `In` applies, as `<> ALL($1)` rather than
`NOT (= ANY)` — one placeholder for the whole list, which is the property `In`
exists for.

**`ILike`**, so nobody writes `lower(col) LIKE lower($1)`, which is the same
query with the index thrown away.

**`Contains`, `ContainedBy`, `HasAnyKey` and `HasAllKeys` on jsonb columns** —
`@>`, `<@`, `?|`, `?&`. A jsonb column used to support only `IS [NOT] NULL`, so
every question about the document went through raw SQL on a column the model
already describes.

Equality stays refused, and jsonb is the clearest case for it: jsonb normalises
key order and drops duplicate keys, so two documents a caller thinks differ can
be equal, and two they think match can differ by whitespace they never wrote.

Plain `?` is absent deliberately. Its argument is a single text value, which
would need the string arena the column already spends on the `@>` argument;
`?|` with one key asks the same question through a storage the column is not
otherwise using. A jsonb column therefore carries an arena for the document
operators and a list slot for the key operators, and the leaf already
dispatches list operators before the arena switch, so nothing new was needed to
keep them apart.

`examples/orders` gained a `Tags []string` column and an `Attrs ProductAttrs`
jsonb column, both GIN-indexed, to exercise all of this against a real server.

### API review before v1.0

`STABILITY.md` binds from v1.0.0, so the surface v0.3.0 added — 154 exported
symbols — gets one pass while it is still free to change. The method was to ask
what actually calls each of them.

**Ten had no call site anywhere.** `Abs`, `Coalesce`, `NullIf`, `Lt`, `Lte`,
`And`, `Or`, `IsNull`, `IsNotNull`, `Col` — not in the example, not in a test,
not internally. Their type resolution had never run. Running it found that
`nullif(amount, 0)` — the division-by-zero guard `NullIf`'s own doc gives as its
reason to exist — was refused, because the literal is `int8` and the column is
`numeric` and the two had to match exactly. PostgreSQL casts it. `coalesce` and
`nullif` now unify within a family and widen to the more capacious type.

**The declaration vocabulary moved onto the builder.** `storm.Eq`, `storm.And`,
`storm.Col` and nineteen more sat at the top level beside the generated query
API, meaning something different: `order.Status.Eq(x)` filters rows at run time,
`storm.Eq(&o.Status, x)` described a filter at declaration time. Two `Eq`s in
scope with different semantics is a question every reader has to answer once.
They are now methods — `a.Eq(...)`, `a.DateTrunc(...)`, `j.OnCols(...)` — so a
declaration constructor cannot be reached from a query, because the builder is
not in scope there. The root package exports **none** of the 22.

**Declared outputs return handles.** `Having(a.Gt(a.Out("Orders"), 0))` named an
output by string, checked at build time. A declaration now returns an `Out` the
compiler checks, and it cannot name an output that does not exist yet because
you do not have one until it has:

```go
b := a.Named("ByStatus")
orders := b.Count("Orders")
b.Having(a.Gt(orders, 0))
```

`Filter` and `OverWindow` moved onto that handle too, so they attach to the
output they name rather than to "the last one declared" — moving a line can no
longer silently move a filter with it. One error case disappeared entirely: a
`Filter` with no aggregate to filter is now unrepresentable.

**`storm.Expr` is `storm.RawSQL`.** It is a raw SQL string, and someone reaching
for "an expression" was landing on the escape hatch rather than on the checked
vocabulary. `Expr` remains as a deprecated alias, so models written against
v0.1–v0.3 keep compiling.

**`Star()` is unexported.** `Count(name)` already means `count(*)`, `CountOf`
takes the column, and every other position refused a star — there was no
argument slot for it, and its doc described one that does not exist.

## v0.3.0 — 2026-09-01

### Benchmarks need re-running before the tag

`bench/RESULTS.md` was measured on Go 1.26.6 and the module now builds on 1.27.
A spot check found the **allocation counts unchanged** — which are the claims
that file says are the honest ones — but `DecodeRow_Offline`, which needs no
database and touches nothing v0.3.0 changed, moved from 31 ns / 48 B / 3 allocs
to ~48 ns / 128 B / 1 alloc. A toolchain difference in `internal/spike`, not a
regression, and exactly what a re-run is for.

The wall-clock table was deliberately NOT rewritten: the spot check ran against
a different container and every figure including the floor was ~1.7× higher, so
a mixed-environment table would be worse than a stale one. `make results` on
the recorded environment closes this.

### CI now runs everything

Three gates existed only when somebody ran them by hand, which means they were
documentation rather than gates:

- **The orders example** is a separate module, so `go test ./...` cannot reach
  it. Its 34 tests are the only end-to-end proof of aggregations, joins, CTEs,
  full-text search, ranges and the typed constraint errors — against a real
  server. Left out of CI they would have rotted within a release.
- **`scripts/check/mysql.sh`**, with a MySQL 8 service. It now works both from
  a local container and from CI, and it applies the SQL rather than diffing it,
  because a golden test cannot tell you a statement is valid.
- **Coverage floors** were failing. `runtime`, `runtime/pgxdrv` and `codegen`
  had all fallen below theirs — this release added a lot of code and not enough
  tests with it. Fixed by writing the missing tests, not by moving the floors:
  range encode/decode round trips including every bound form, `Overlaps` across
  thirteen boundary cases and asserted symmetric, the tstzrange parameter codec
  against a live server, `Retryable`, and the join and aggregate emitters'
  refusals.



### The bootstrap main is gone

Adopting storm no longer means writing a file storm told you to write. Install
the binary and run it:

```console
go install github.com/gsoultan/storm/cmd/storm@latest
go get github.com/gsoultan/storm/tool     # once per module
storm generate internal/store
```

No `cmd/storm/main.go`, no `model.All()` registry. storm parses your module to
find the models, writes the bootstrap itself, runs it, and removes it.

**Nothing semantic moved.** Field pointers still resolve by offset at runtime,
in your process, exactly as before — the parser answers one static question
(which types are models, and where) and nothing else. Discovery is syntactic on
purpose: type-checking your module would mean failing whenever it does not
compile, including the first run, before a store exists to compile against.

**What counts as a model:** a type that embeds `storm.Model`, or declares a
`Schema`, `Plans` or `Projections` method, or carries `//storm:model`.
`//storm:ignore` opts out. **A type embedded in another struct is a mixin, not
a table** — `Auditable` and `SoftDelete` in storm's own fixtures are exported
and have `Schema` methods, and being embedded is the only thing that
distinguishes them from a model.

`storm models` prints what was found, which rule matched, and what was skipped
and why.

**The commands nobody outside this repository was reaching.** The M6 adopter
hand-rolled a `main` and therefore never got `verify -pending`, `lint` or
`explain` — the tool was reachable in principle and unreached in practice.
Discovery makes every command work by default. See
[ADR-0006](docs/adr/0006-discovery-replaces-the-bootstrap.md).

### MySQL: the second back end, and a portability report

`compile/myddl` renders a schema as MySQL 8 DDL, and `storm portable mysql`
answers the question that actually matters before anyone commits to supporting
an engine:

```
storm: this model does not port to MySQL:
  bookings.during is tstzrange: MySQL has no range types, and therefore no
    exclusion constraints — store two DATETIME(6) columns and enforce
    non-overlap in the application, knowing it can race
  bookings: EXCLUDE constraints have no MySQL equivalent — the overlap they
    prevent becomes a race the application cannot win
  products.search is tsvector: MySQL has no tsvector; its full-text search is a
    FULLTEXT INDEX over the source columns, not a materialised column
```

**Every problem at once**, named by column, with what to do instead. That is
what `docs/DIALECTS.md` promised — *"portability constraints become declare-time
errors instead of discoveries on a customer install"* — and it is the half of
multi-dialect that pays for itself immediately, because the answer for most
existing models is "it does not, here is why".

The type mapping is a set of decisions, not defaults, and each is tested:
`DATETIME(6)` rather than `TIMESTAMP` (MySQL's is 1970–2038 and converts
through the session time zone); `BINARY(16)` rather than `CHAR(36)` for a uuid;
`TINYINT(1)` spelled out rather than `BOOLEAN`. An **unbounded numeric is
refused** — MySQL's unspecified `DECIMAL` means `DECIMAL(10,0)`, which
truncates every fraction, and an accounting column that quietly loses its cents
is the worst portability failure there is.

Verified against a real MySQL 8: the DDL for a portable model applies, and
`SHOW CREATE TABLE` confirms the types, the index and the foreign key with its
`ON DELETE CASCADE`. `scripts/check/mysql.sh` is that check, applied through
the container's own client so storm gains no MySQL driver dependency.

Writing it caught a bug a golden test could not: an empty default translation
still emitted the keyword, so every uuid column shipped `DEFAULT  NOT NULL`.
Unit tests accepted it; MySQL rejected it on the first line. That is why the
gate applies the SQL rather than diffing it.

**What this is not, and why it is further away than it looks.** This is the DDL
half: storm cannot generate a *store* for MySQL, only tell you whether it could
and emit the schema.

A driver adapter is **not** the missing piece.
[ADR-0007](docs/adr/0007-mysql-runtime-needs-a-second-decoder-family.md) records
the finding: the `Executor` port hands back raw wire bytes and every generated
scanner decodes PostgreSQL **big-endian binary**, while every Go MySQL driver
delivers values already decoded through `database/sql/driver.Value` — and
MySQL's own protocol is little-endian anyway, so even raw bytes would not fit.
`binary.BigEndian.Uint64` on a MySQL BIGINT reads a byte-reversed number
silently, for every row.

Widening the port to `driver.Value` would make the adapter a weekend's work and
cost one boxing allocation per column per row **on every dialect including
PostgreSQL** — which `AGENTS.md` vetoes by name and `bench/RESULTS.md` measures.
So MySQL at run time is a second decoder family plus a dialect-parameterised
codegen: a milestone of its own, now scoped rather than assumed.

**`runtime/mydec` is that decoder family, and it is built.** Little-endian
integers and floats, MySQL's component-wise DATETIME/DATE/TIME (which carry a
leading length, so every shorter form is legal and a fixed-width read would run
past the value), and its text-encoded DECIMAL. Zero-allocation, stdlib-only,
and the test suite asserts it *disagrees* with the PostgreSQL family on the same
bytes — `runtime.Int8` reads a MySQL 1 as 72057594037927936 — so pointing
codegen at the wrong family cannot pass tests. `codegen.Dialect` names the two.

**The decode site is parameterised too.** `codegen.Options.Dialect` selects the
family, and every decoder reference routes through `decoders.q`, which renames
before it qualifies. Fallibility became a dialect question in the process:
MySQL's DATETIME reads a leading length and can be handed one that does not
match, where PostgreSQL's fixed-width read cannot, so `fallibleIn` asks the
family rather than the type. A column the dialect cannot decode is refused at
generation, matching `compile/myddl`'s DDL refusal so the two halves cannot
disagree.

**PostgreSQL output is byte-identical across the change** — every generated
file hashed before and after. That check also caught a real bug: the context
file built its emitter without a family, so it emitted a bare `.Int8`, and an
earlier verification had passed only because the failing regeneration was
silenced and the check compared stale files.

Still missing: the wire client. Everything else is done — the decoders are
unit-tested against hand-built MySQL wire values — but nothing can execute a
MySQL query through storm until a client exposes binary result rows. That is
now the only remaining piece.

### Range types, and the scheduling case

`storm.TstzRange` is a PostgreSQL `tstzrange`: an interval with explicit
bounds, so "do these two bookings overlap" is a question the **database**
answers with a GiST index rather than four comparisons in Go that get the
boundary cases wrong.

```go
type Booking struct {
    storm.Model
    Room   int32
    Guest  string
    During storm.TstzRange
}

func (b *Booking) Schema(t *storm.Table) {
    t.Exclude(
        storm.With(&b.Room, storm.OpEq),
        storm.With(&b.During, storm.OpOverlaps),
    )
}
```

That emits `EXCLUDE USING gist ("room" WITH =, "during" WITH &&)` — and the
`btree_gist` extension it needs, guarded against a concurrent `CREATE`. Two
overlapping bookings for one room cannot both commit, and the loser gets
`runtime.ErrExclusionViolation`, which the example maps to a 409.

Predicates: `Overlaps` (`&&`), `ContainsRange` (`@>`), `ContainedBy` (`<@`).
`<` and `>` are deliberately absent — PostgreSQL defines an ordering for
sorting, and almost everyone who reaches for it means `Overlaps`.

**Half-open by default.** `storm.NewTstzRange(lower, upper)` builds
`[lower, upper)`, which is what scheduling wants: `[09:00, 11:00)` and
`[11:00, 12:00)` abut and do **not** collide. Asserted in both directions —
that the database accepts the abutting booking, and that the Go `Overlaps`
agrees with `&&` row for row, so a check on either side cannot disagree with
the other.

The range is encoded and decoded in **binary**, with its own arena and its own
pgx codec for the same reason `Decimal` has one: an untyped parameter takes its
type from context, and `WHERE during && $1` resolves that context before the
value exists — so the parameter has to *be* a tstzrange.

`runtime.Since(t)` and `runtime.Until(t)` build the unbounded forms.

### Full-text search, and the optional-filter idiom

**`storm.TSVector`** is a search column: declare one and PostgreSQL gets a
`tsvector`, the generated API gets `Matches` and `WebSearch` on it.

```go
type Product struct {
    storm.Model
    SKU, Name string
    Search    storm.TSVector
}

func (p *Product) Schema(t *storm.Table) {
    t.Col(&p.Search).
        Generated(storm.Expr(`to_tsvector('english', coalesce(name,'') || ' ' || coalesce(sku,''))`)).
        Index()
}

hits, err := product.New().Where(product.Search.WebSearch(q)).All(ctx, ex, nil)
```

**Filterable but not readable.** A tsvector is index support, not data — nobody
wants one in a Go struct, and decoding one on every read would be pure cost. It
is absent from `Row`, from writes and from keyset cursors, and present as a
column handle. Nothing else in storm has that shape, which is the point: a
column you can only ask questions of.

`Matches` lowers to `plainto_tsquery` — terms ANDed, syntax ignored, the safe
default for something a user typed. `WebSearch` lowers to
`websearch_to_tsquery`, which understands quoted phrases, `or` and a leading
`-`, because that is what a search box is expected to do. Both take the
server's `default_text_search_config`, so the language is a database setting
rather than a constant compiled into every query.

The term is **bound, never interpolated**: `widget'); DROP TABLE products; --`
is a search for those words, asserted by a test that then checks the table is
still there. And two searches differing only in the term share one compiled
statement, because a term is a value and values are not part of a shape.

`Eq` is deliberately absent — comparing two tsvectors for equality asks whether
two documents have identical lexeme vectors, which nobody means.

**`WhenSet`** is the optional-filter idiom:

```go
q = product.WhenSet(q, f.MinPrice, product.Price.Gte)
q = product.WhenSet(q, f.Sku, product.Sku.Eq)
```

`WhereIf` could not do this. It takes an already-built `Pred`, so the caller has
to evaluate `product.Price.Gte(*f.MinPrice)` *before* the condition is tested —
which panics on exactly the nil the condition was checking for. `WhenSet` takes
the constructor, so nothing is dereferenced unless it is there.

### Typed constraint errors

A duplicate email used to come back as an opaque driver error. Telling it from
a genuine failure meant type-asserting `*pgconn.PgError` and comparing the
string `"23505"` — in a handler, which is driver knowledge leaking straight
through the port that exists to stop it. So the difference between a 409 and a
500 was work every adopter did again, or did not do.

```go
if errors.Is(err, runtime.ErrUniqueViolation) { … 409 … }
if runtime.Retryable(err)                     { … retry … }
```

Classified at the driver boundary, which is the only place a SQLSTATE is read:

| | |
|---|---|
| `ErrUniqueViolation` | 23505 |
| `ErrForeignKeyViolation` | 23503 |
| `ErrCheckViolation` | 23514 |
| `ErrNotNullViolation` | 23502 |
| `ErrExclusionViolation` | 23P01 — the booking-conflict case |
| `ErrSerializationFailure` · `ErrDeadlock` | 40001 · 40P01, both `Retryable` |

`ConstraintError` also carries the constraint, table and column names, so a
handler can say *which* one — and carries **no bound value**. PostgreSQL's own
diagnostic does (`Key (email)=(ada@example.com) already exists`); that message
belongs to the server, so it is preserved by `Unwrap` and deliberately not
folded into storm's text. Logging a storm error cannot leak what logging the
driver's would (P2.3).

**Anything unrecognised is returned unchanged.** A wrapper that renamed every
error would hide the ones storm has no opinion about, which are the ones worth
reading verbatim — asserted by a test that a missing table stays a missing
table.

Every exit path is classified, including `rows.Err()`: pgx *defers* a
statement's error to the drain, so a constraint violation on a `RETURNING`
insert arrives there and not from `Query`. Classifying only the immediate
return would have missed the write path — which is the path constraints are on.

`examples/orders` maps them: a duplicate is a 409 naming the constraint, a
serialization failure is a 409 marked `retryable: true`.

### Joins across tables, and CTEs

The last two items of [docs/COMPLEX-QUERIES.md](docs/COMPLEX-QUERIES.md).

```go
func (o *Order) Joins(j *storm.Joins) {
    var c Customer

    j.Named("WithCustomer").
        Inner(&c, &o.Customer).            // the FK relation says how to join
        Take(&o.ID, "OrderID").
        Take(&c.Email, "CustomerEmail").
        Where(storm.Ne(&o.Status, "cancelled")).   // a caller cannot widen this
        OrderDesc(&o.PlacedAt)

    // A CTE: each customer's lifetime spend, computed ONCE and joined against,
    // instead of a correlated subquery per row.
    j.Named("VsLifetime").
        With("spend", &Order{}, "ByCustomer").
        Inner(&c, &o.Customer).
        LeftWith("spend", storm.OnCols("spend", "customer_id", &c.ID)).
        Take(&o.Total, "Total").
        TakeFrom("spend", "Lifetime", "Lifetime")
}
```

**Declared with field pointers into local variables.** `&c.Email` is a checked
reference, so renaming `Customer.Email` is a compile error in the join too.
Every registered instance occupies its own address range, which is what lets a
pointer say which table it belongs to without a single string.

**A join projects; it does not load entities.** The output is a flat row of
scalars, so it lives in the declaring table's package and reuses the entire
existing read path — the same token stream, tree cache, binder and
zero-allocation warm build. When you want the entities, that is still a Plan.
Measured: **9 allocations per call against 18 for a plain full-row read**.

**LEFT is in the type.** Anything taken through a `Left`/`LeftWith` is
`runtime.Null[T]` whatever its own constraint says — including a `count`, which
is never NULL in a `GROUP BY` but is NULL when a LEFT join finds no row. Typing
it otherwise would decode a missing match as a zero.

**A declared `Where` cannot be widened.** It is ANDed with the call site's
predicates by `runtime.SpliceTreeWhere`, with the caller's side parenthesised so
a disjunction cannot escape it. Asking for `status = 'cancelled'` against a join
that declared `status <> 'cancelled'` returns nothing, which is the point of
declaring it there.

`Order()` on a join is refused by name: a bare column in a multi-table result is
ambiguous, so the ordering is declared.

### Fixed: two spellings of one name rule

`customer_id` became `CustomerID` in generated structs and `CustomerId` in the
declaration side, because `exportName` and `exportIdent` were separate
implementations of the same rule. A grouped read on a foreign key emitted a
scanner that assigned to a field that did not exist. There is now one
`schema.GoName`, which both call — found by the first aggregation to group by a
foreign key, and exactly the drift the comment on the duplicate had warned about.

### Expressions, FILTER, HAVING, grouping sets and window functions

Five more of [docs/COMPLEX-QUERIES.md](docs/COMPLEX-QUERIES.md), on one new
foundation: a typed **expression IR** (`schema.Expr`) that resolves its own
result type and nullability at declaration time, plus a separate **declared
predicate** tree (`schema.Cond`) for the conditions that never vary.

```go
func (o *Order) Aggregates(a *storm.Aggregates) {
    a.Named("Daily").
        ByExpr("Day", storm.DateTrunc("day", &o.PlacedAt)).   // group by an expression
        Count("Orders").
        Count("Big").Filter(storm.Gte(&o.Total, storm.Lit(d))). // FILTER (WHERE …)
        Sum(&o.Total, "Revenue").
        RowNumber("Rank", storm.Over().OrderByDesc(storm.Out("Revenue"))).
        Lag(storm.Out("Revenue"), "Prev", storm.Over().OrderByAsc(storm.Out("Day"))).
        Having(storm.Gt(storm.Out("Orders"), 0))

    a.Named("Facets").
        By(&o.Status).ByExpr("Day", storm.DateTrunc("day", &o.PlacedAt)).
        Sets([]string{"Status"}, []string{"Day"}, nil).   // one pass, every facet
        Count("Orders").
        GroupingOf("StatusIsSubtotal", &o.Status)
}
```

- **Grouping by an expression** — `date_trunc('day', …)`, `coalesce`, `nullif`,
  `abs`, over an allow-list rather than a string, because the generated field's
  type is whatever the expression resolves to.
- **FILTER** — `count(*) FILTER (WHERE …)`, which is both clearer and faster
  than `count(CASE WHEN …)`.
- **HAVING** — filters the groups, after aggregation, as against a call-site
  `Where` which filters the rows going into them. `storm.Out("Orders")`
  references a declared output and expands to its expression, because
  PostgreSQL resolves SELECT aliases *after* grouping and cannot see them in a
  HAVING.
- **GROUPING SETS, ROLLUP and CUBE** — every facet in one pass instead of one
  query per facet. Every grouping column becomes `runtime.Null[T]` in the row
  type, because a subtotal row carries NULL for what it aggregated over, and
  `GroupingOf` tells that NULL from one that was in the data. Subtotals sort
  above their detail (`NULLS FIRST`).
- **Window functions** — `row_number`, `rank`, `dense_rank`, `lag`, `lead`,
  `first_value`, and any aggregate `OverWindow(...)`. `lag`/`lead`/`first_value`
  produce nullable fields because they are NULL at the partition edge however
  non-null the column is.

**The rule that makes it safe.** In a grouped query every column must be a
grouping expression or inside an aggregate; PostgreSQL raises otherwise at
*execution*, from a report that may only run at month end. storm now refuses at
declaration:

```
orders: aggregate "Daily" reads column placed_at in "Rank", but groups by
something else — a column in a grouped query must be one of the grouping
expressions or inside an aggregate
       group by it, wrap it in an aggregate, or reference a declared output
       with storm.Out(...)
```

That check caught a real mistake in the example while it was being written. It
also has to be *exactly* right in both directions: an aggregate's arguments and
its FILTER may read any column (asserted against a live server), while its OVER
clause may not; and an expression that appears in the GROUP BY is usable whole
even though its arguments alone are not. Both directions are tested.

Also refused at declaration time: a rank over an unordered window (it ranks by
nothing and returns a different answer each run), `storm.Star()` anywhere but
`Count`, `GROUPING` without grouping sets, and a `Filter` with no aggregate.

**Still zero-allocation**: a warm aggregation builds and binds in **~62 ns, 0
allocs**, unchanged by the richer surface.

**The escape hatch shrank.** `examples/orders` no longer needs `storm.SQL[T]`
for its daily revenue report — the declared `Daily` aggregation replaced it,
and gained a rank and a previous-day comparison it did not have.

### Not yet: joins projecting across tables, and CTEs

The two remaining items from that document are a different problem — multi-table
scope, alias management, and row types that live in the context package rather
than a table package — and they are a milestone of their own rather than a
smaller version of this one. They remain `storm.SQL[T]`, validated against the
model at generate time.

### Declared aggregations — GROUP BY, without dropping to SQL

Reporting reads no longer have to be `storm.SQL[T]`. A model declares them the
way it declares plans and projections:

```go
func (o *Order) Aggregates(a *storm.Aggregates) {
    a.Named("ByStatus").
        By(&o.Status).
        Count("Orders").
        Sum(&o.Total, "Revenue").
        Avg(&o.Total, "AvgOrder").
        Max(&o.PlacedAt, "LastOrderAt")
}
```

```go
rows, err := order.New().
    Where(order.PlacedAt.Gte(since)).   // predicates still compose
    AllByStatus(ctx, ex)                // []order.ByStatusRow
```

**Declared, not composed at the call site**, for the reason the library exists:
a `GroupBy(...).Select(...)` chain assembled at run time has an unbounded set
of result shapes, and a shape storm has not seen can have neither a generated
scanner nor a compiled statement. Naming it keeps the whole thing inside the
compilation thesis. The predicates stay dynamic because those *are* bounded.

**The generated row types are the point.** PostgreSQL's aggregate result type
is usually not the input type, and every aggregate but `count` returns NULL
over zero rows:

```go
type ByStatusRow struct {
    Status      string                          // the grouping column
    Orders      int64                           // count → int8, never NULL
    Revenue     runtime.Null[runtime.Decimal]   // sum(numeric) → numeric, NULL over no rows
    AvgOrder    runtime.Null[runtime.Decimal]   // avg(numeric) → numeric, not a float
    LastOrderAt runtime.Null[time.Time]
}
```

Had `Revenue` been a plain `Decimal`, NULL would have decoded as zero and "no
orders" would be indistinguishable from "orders totalling nothing" — a wrong
answer with no error. The mapping (`sum(int4)→int8`, `sum(int8)→numeric`,
`avg(int)→numeric`, `avg(float4)→float8`) is asserted against a live server by
`TestAggregateResultMatchesPostgres`, not taken from the documentation.

**Refused at build time, not at month end.** PostgreSQL has no `sum(text)`, no
`max(uuid)` and no `min(bool)`; the allow-list comes from `pg_aggregate` on a
running server, so a new type defaults to refused rather than to
`function max(uuid) does not exist` raised from a report that runs once a month.

**Performance is the same budget as every other read**: a warm aggregation
builds and binds in **58 ns with zero allocations**
(`BenchmarkAggregate_BuildAndBind_Warm`), asserted by
`TestAggregateWarmPathAllocatesNothing`.

`Order()` on an aggregation is refused by name rather than silently ignored —
its rows are groups, and the result is already ordered by its grouping columns,
because PostgreSQL promises no order for a `GROUP BY` and an unordered report
shuffles between requests.

**What is still `storm.SQL[T]`:** joins that project across tables, window
functions, CTEs, `FILTER`, `HAVING`, grouping sets, and grouping by an
expression such as `date_trunc('day', …)`. The orders example keeps its
`DailyRevenue` raw for exactly that last reason. The designed surface for all
of it is in [docs/COMPLEX-QUERIES.md](docs/COMPLEX-QUERIES.md); this is the
single-table slab of it, complete rather than sketched.

### Go 1.27

The module now declares `go 1.27`, and CI builds on it.

**This raises the floor for consumers.** The `go` directive is a minimum, so a
project pinned to `GOTOOLCHAIN=local` on Go 1.26 will not build storm until it
upgrades; with the default `GOTOOLCHAIN=auto` the 1.27 toolchain is fetched
automatically and nothing is needed. Storm is pre-1.0 and the toolchain floor
moves with it.

Nothing in storm changed to accommodate it: the full suite passes under
`-race -shuffle=on`, every boundary gate holds, `govulncheck` is clean, and
**generated output is byte-identical** across the two toolchains — regenerating
under 1.27 produces the same files 1.26 did, which is the determinism guarantee
doing its job.

`bench/RESULTS.md` still records **Go 1.26.6** in its environment line, and
deliberately so: those numbers were measured on that toolchain. It will change
when the benchmarks are re-run, not before.

### The generate step stops being something to remember

Go has no build hook, so the step cannot be removed — but *remembering* it can
be, and that was the actual cost. A stale store used to **compile**: add a
field, skip `generate`, and the project built cleanly with the column simply
absent from the API. You found out when you finally typed `product.Barcode`,
possibly much later and somewhere else. Silence was the problem, not the
command.

**`storm watch <dir>`** regenerates on save. Leave it running and editing a
model is the whole workflow — no dependency added, it polls with the same walk
rules discovery uses, debounces editor write-bursts, and never exits on a
broken model:

```console
$ storm watch store
storm: watching example.com/app → store
storm: ctrl-c to stop
storm: 5 model(s) → store (1.3s)
storm: 5 model(s) → store (871ms)     ← you saved model.go
```

**A compile-time staleness check.** Generated code now carries a shape
assertion per model — an unkeyed composite literal rebuilt from the struct's
own fields — so drift stops the build and names the field:

```
store/shape.gen.go:57:2: too few values in struct literal of type model.Product
store/shape.gen.go:67:5: m.OnHand undefined (type model.StockItem has no field or method OnHand)
```

It catches a field **added, removed, renamed or reordered**. It does *not*
catch a field's type changing, or any change inside `Schema`, `Plans` or
`Projections` — those are method bodies the type system cannot see, and
`verify -stale` remains the check for them. Models with an unexported field get
no assertion, because an unkeyed literal of one is illegal outside its own
package; the generated file says so by name.

**A file that does not parse now says so.** It used to be skipped silently,
which meant a typo in your only model file reported "no models found" and sent
you looking for a missing declaration:

```
storm: no models found, because 1 file(s) did not parse:
       model/model.go:146:1: expected declaration, found this
```

### Fixed: every predicate on a `bool` column

`bool` shared the `int64` value arena, so a predicate's value reached pgx as
`*int64` — which has no encode plan for OID 16. Every `Where(x.Flag.Eq(true))`
failed at execution with `cannot find encode plan`, on all rows, not some.

`bool` now has its own arena, which is the fix `TimeOfDay` already needed for
exactly the same reason. **No fixture in this repository had a bool column**,
which is the whole explanation for how it shipped; `internal/testmodel` has one
now, and `TestBoolPredicateBindsAsBool` asserts the two halves partition the
table — so a bound zero fails as loudly as a bind error.

While fixing it, the four hand-maintained lists that had to agree about arenas
(binder fields, Query fields, cursor declarations, bind switch) became one
`arenaTable`. Missing one of the four is what produced the bug.

### Fixed: a declared plan could collide with the automatic one

Every relation already generates a plan, so `Order.Lines` produces
`OrderWithLines`. Declaring `p.Named("WithLines")` produced the *same type name
a second time*, in the same package — and the failure surfaced as a
redeclaration error in the adopter's build, in a generated file, naming a type
they never typed.

`storm generate` now refuses, names the plan and the relation it conflicts
with, and says what to do: delete the declaration, because the relation tier
already provides that plan with a per-child `Order` and `Top` the declared one
does not have.

Both were found by [`examples/orders`](examples/orders), the first thing
outside this repository to use a bool column or to declare a plan over a
relation of the same name.

### New: examples/orders — a Go kit service

A separate module (its own `go.mod`, so go-kit stays out of storm's dependency
graph) implementing order fulfilment: catalogue, checkout that reserves stock
under concurrency, order retrieval, and a finance report. It runs against real
PostgreSQL and its concurrency test puts 12 goroutines against a stock of 20 to
prove the version column prevents overselling.

It is also the adoption path with nothing to bootstrap — no `cmd/storm`, no
`model.All()`.

### Not a breaking change

`tool.Main(model.All(), model.Queries())` still works, is still supported, and
is still the answer when storm's rules would not find your models. Both paths
generate **byte-identical** code, asserted in `scripts/check/outsider.sh` —
which grew a second stranger who writes no bootstrap at all, because everything
the first one exercises would keep passing if discovery were broken.

### One new failure, and what it says

Nothing in your source imports `storm/tool` any more, so `go mod tidy` cannot
record its dependencies and the first run fails with go's generic `updates to
go.mod needed`, pointing at a file you did not write. storm detects that exact
failure and prints the fix:

```
storm: the bootstrap needs storm's tool package recorded in your go.mod, and nothing in
       your source imports it. Run this once:

           go get github.com/gsoultan/storm/tool
```

## v0.2.0 — 2026-08-27

### Renamed: raorm is now **storm**

The module path changed, which for Go is a breaking change even though not a
line of API moved:

    github.com/gsoultan/raorm  →  github.com/gsoultan/storm

For a consumer the migration is two mechanical steps and nothing else:

```console
go mod edit -droprequire github.com/gsoultan/raorm
go get github.com/gsoultan/storm@v0.2.0
# then: sed -i '' 's|gsoultan/raorm|gsoultan/storm|g' on your imports
```

Everything else is a straight substitution — the package qualifier
(`raorm.SQL[T]` → `storm.SQL[T]`), the error prefixes, and the DSN environment
variable **`RAORM_DSN` → `STORM_DSN`**. Generated code carries the tool name
in its header, so **regenerate after upgrading**; `verify -stale` will tell
you if you forget.

The old path is not withdrawn and cannot be: `github.com/gsoultan/raorm`
v0.1.0 and v0.1.1 stay resolvable through the module proxy forever. They
simply stop receiving changes.

**One trap worth knowing about.** GitHub redirects the old repository name to
the new one, so `go list -m -versions github.com/gsoultan/storm` reports
`v0.1.0 v0.1.1 v0.2.0` — but the first two **cannot be used under this path**.
Their `go.mod` declares the old module path, and Go refuses the mismatch:

```
module declares its path as: github.com/gsoultan/raorm
        but was required as: github.com/gsoultan/storm
```

That is correct behaviour and not fixable from here — a tag's go.mod is
immutable. **Under `storm`, v0.2.0 is the first usable version.** If you want
a v0.1.x, require it under the old path, where it works exactly as it always
did.

### Added

- **`numeric[]`** decodes to `[]storm.Decimal` and encodes back, which closes
  type coverage: every type the model DSL can declare now round-trips.
  Element decoding is fallible — a numeric past the 18 significant digits a
  Decimal holds is an error rather than a wrong number — so `runtime.ArrayErr`
  joins `Array` as the single implementation of the bounds arithmetic a fuzzer
  once found a hole in.
- **`storm.TimeOfDay`** — PostgreSQL `time` (without time zone), microseconds
  since midnight. Its own type rather than a `time.Time`, for the reason
  `Interval` is not a `Duration`: an instant is a point on a calendar in a
  zone, a SQL `time` is none of those, and decoding one into the other forces
  a date to be invented. It gets `Eq`/`Gt`/`Gte`/`Lt`/`Lte` and ordering,
  which an interval cannot have, because 09:00 really is before 17:00.
  24:00:00 is accepted, as PostgreSQL accepts it.

### Fixed

- Adding a type surfaced three latent codegen faults, each of which would
  have hit the next type added: an arena missing from the capacity map read
  as capacity zero and failed the FIRST predicate as "too complex" (now a
  generation error naming the arena); two cursor-name maps produced
  `b.tods[] = q.tods[]`, caught by the parser; and the column handle wrote
  its value to the shared `int64` field while the arena read a different one
  — which compiled, bound zero, and matched every row.

## v0.1.1 — 2026-08-26

The release that makes the tool usable by someone who is not its author.
v0.1.0's `generate` could not produce compiling code in any module but this
one, and the rest of the commands were unreachable, so **v0.1.0 should be
skipped**.

### Fixed — the first-run path, none of which worked outside this repository

Found by doing the thing no gate had done: taking a fresh module outside both
storm and its adopter, running `go get`, and following the README. Detail and
the lesson in [docs/PRODUCTION-READINESS.md](docs/PRODUCTION-READINESS.md) P4.

- **`generate` emitted storm's own module path into the caller's module**, so
  a user's generated context package imported
  `github.com/gsoultan/storm/internal/store/...` and could not compile. The
  import path now comes from the host module's `go.mod`. This is the flagship
  command, and it had never worked for anyone but this repository — where the
  wrong answer happens to be the right one.
- **The tool was unreachable.** `Models` lived in `package main`, which cannot
  be imported, and the "generated bootstrap" its documentation referred to did
  not exist. Adopters could hand-roll `codegen.Package` (anubis did) but had
  no way to get `verify -pending`, `lint` or `explain` at all. The commands are
  now the importable package **`storm/tool`**; your whole tool is:

  ```go
  func main() { tool.Main(model.All(), nil) }
  ```

  `cmd/storm` remains as a stub whose only job is to fail with that
  instruction.
- **An output directory reached through a symlink was refused** ("outside this
  module"). On macOS `/tmp` is a symlink, so this hit anyone using a temporary
  directory. The deepest existing ancestor is now resolved on both sides.

### Added

- **`-raw-schema live`** — validate `storm.SQL[T]` declarations against the
  connected database instead of a scratch apply of the model. Required by any
  adopter whose model is a *projection* of a schema owned by migrations, whose
  queries therefore call functions and read tables the model does not
  describe. The first adopter could not use the tool without it. The default
  is unchanged, and the cost is stated: the model no longer vouches for those
  statements, so point it at a database built from migrations.
- **`storm.SQLExec`** — the no-rows half of the escape hatch, for the `:exec`
  statements that are about a third of a real query file. Same placeholder
  precheck as `SQL[T]`, and generation *requires* a zero-column result, so an
  exec that silently returned rows is a build failure pointing at `SQL[T]`.
- **Fast `int8[]` and `text[]` parameter codecs**, joining `uuid[]`. Measured
  at 500 elements: `int8[]` 466 allocations → 1 (21× faster), `text[]` 503 → 1
  (8×). Schemas with bigserial keys bind `int8[]` on the same `= ANY($1)`
  relation-load path storm's uuid-keyed fixtures hid.
- **`runtime.ShapeCap`** (default 1024) bounds the compiled-statement cache,
  with `SetShapeCap(0)` to opt out and a generated `ShapeFlushes()` gauge per
  package. 100k distinct shapes retain 170 KB instead of 27.8 MB.
- **Wire-format guard** in `runtime/pgxdrv`: pools in
  `QueryExecModeSimpleProtocol`/`Exec` are refused at construction, and every
  result is checked once per statement. Under those modes `false` arrives as
  the byte `'f'` and decoded as **true** — a silent inversion. Costs 3.82 ns
  per statement, zero allocations. See [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)
  for the PgBouncer table.
- **Generated headers carry the storm version**, so "which storm wrote this?"
  is answerable from the file.
- [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) and
  [docs/PRODUCTION-READINESS.md](docs/PRODUCTION-READINESS.md).

### Changed

- `runtime.TextArray` and friends name a text-format array
  (`ErrArrayTextFormat`) instead of misreporting it as multi-dimensional. An
  `enum[]` arrives as text because pgx has no binary codec for one; the error
  now says to declare the column `text[]` or cast it.

## v0.1.0 — 2026-08-26

First tagged release. Postgres only; the dialect seam exists and is
CI-enforced but has one implementation, which makes it a hypothesis rather
than a fact (see [docs/DIALECTS.md](docs/DIALECTS.md)).

- Model-first schema with generated, never-applied migrations; `diff` /
  `verify` / `verify -stale` / `verify -pending`.
- Compile-time query building: a dynamic query's shapes each compile once and
  a warm call allocates nothing to build its SQL.
- Relations as **named plans** with asserted round-trip counts; reading an
  unloaded relation does not compile.
- Writes: masked inserts and updates, optimistic locking, `COPY` bulk load,
  FK-ordered atomic unit-of-work.
- `storm.SQL[T]`: any statement PostgreSQL can run, typed, with a generated
  scanner validated at build time.
- Tooling gates: `lint` (round-trip budgets), `explain` (plans every
  statement), coverage floors, fuzzing, and an injection property that is
  structural rather than a filter.
- **First adopter shipped**: anubis's authz context — 44 queries, sqlc
  removed, authorize p95 unchanged.
