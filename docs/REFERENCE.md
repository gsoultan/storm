---
tags: [storm, reference, model]
updated: 2026-09-04
status: as-built — the modelling surface, verified against the shipped API
---

# Reference: declaring the model

[[EXAMPLE]] is the one-page introduction and [[API]] is the query surface. This
document is the other half: everything you can say in a `Schema` method, and the
relationship shapes storm knows how to generate.

> **Rewritten 2026-09-04.** The previous version of this file was a design
> sketch that predated the implementation, and most of its call shapes were
> never built — `t.ForeignKey`, `t.Inverse`, `t.Plan`, `t.Set`, `user.Get(...)`,
> `Between`, a `GroupBy(...).Select(...)` chain. It is easier to trust a short
> document than a long one with a warning on top, so the fiction is gone rather
> than annotated. What is genuinely not built is listed at the end.

## 1. Type mapping

The Go type is the column type. Nothing to annotate:

| Go | PostgreSQL |
|---|---|
| `bool` | `bool` |
| `int16`, `uint16` | `int2` |
| `int32`, `uint32` | `int4` |
| `int`, `int64`, `uint`, `uint64` | `int8` |
| `float32` / `float64` | `float4` / `float8` |
| `string` | `text` |
| `[]byte` | `bytea` |
| `[16]byte`, `storm.UUID` | `uuid` |
| `time.Time` | `timestamptz` |
| `storm.TimeOfDay` | `time` |
| `storm.Decimal` | `numeric` — unbounded unless you say `.Numeric(p, s)` |
| `storm.Interval` | `interval` |
| `storm.TstzRange` | `tstzrange` |
| `storm.TSVector` | `tsvector` |
| `netip.Prefix` | `inet`, or `cidr` with `.Cidr()` |
| `[]T` of a scalar | `T[]` |
| `map[string]string` | `hstore` |
| any other `map` or struct | `jsonb` |
| a named string type with declared values | an enum |
| `*T` | the same, nullable |

`storm.Decimal` is unbounded by default on purpose: `numeric` with no bounds is
legal and means "as much as you need". Money says so — `.Numeric(19, 4)` — and
the generator refuses a precision a `Decimal` cannot carry.

`netip.Prefix` covers both `inet` and `cidr`. An inet is an address that may
carry host bits; a cidr is a network where the *database* forbids them. One Go
type, and `.Cidr()` opts into the stricter one.

## 2. The `Schema` method

Everything the type cannot say. Declared with **field pointers**, so the editor
enforces the names and renaming a field refactors its declaration with it.

```go
func (u *User) Schema(t *storm.Table) {
    t.Col(&u.Email).Size(320)
    t.Col(&u.Balance).Numeric(19, 4)
    t.Col(&u.Status).Default("'pending'")
    t.Col(&u.Org).OnDelete(storm.Cascade)

    t.Unique(&u.Email)
    t.Index(&u.Org, storm.Desc(&u.CreatedAt))
    t.Check(storm.RawSQL(`balance >= 0`))
}
```

### On a column — `t.Col(&f)`

| | |
|---|---|
| `.Size(n)` | `varchar(n)` instead of `text` |
| `.Numeric(p, s)` | precision and scale |
| `.Date()` | `date` rather than `timestamptz` |
| `.Cidr()` | `cidr` rather than `inet` |
| `.Default(sql)` | a database default, e.g. `storm.Now()` |
| `.Generated(expr)` | a generated column — the `tsvector` case |
| `.NotNull()` / `.Nullable()` | override what the type implies |
| `.Unique()` | a single-column unique constraint |
| `.Immutable()` | refuse updates to this column |
| `.Version()` | the optimistic-locking column |
| `.OnDelete(a)` / `.OnUpdate(a)` | FK actions: `storm.Cascade`, `storm.Restrict`, … |
| `.Named(s)` | override the derived column name |
| `.Comment(s)` | a `COMMENT ON COLUMN` |
| `.Index()` | a single-column index |
| `.AcknowledgeNoFK(why)` | required on `storm.AnyRef` — see §6 |

### On the table

| | |
|---|---|
| `t.PrimaryKey(&a, &b)` | a natural or composite key, instead of `storm.Model` |
| `t.Unique(&a, &b)` | a composite unique constraint |
| `t.Index(&a, storm.Desc(&b))` | an index; `storm.Desc` orders a key descending |
| `t.Check(storm.RawSQL(...))` | a check constraint |
| `t.Exclude(...)` | an exclusion constraint — see §5 |
| `t.Through(&field, &JoinModel{})` | many-to-many with a payload — see §4 |
| `t.Name(s)` | override the derived table name |
| `t.Comment(s)` | a `COMMENT ON TABLE` |

## 3. Mixins are embedded structs

A struct of shared columns, with its own `Schema` method, embedded into real
models:

```go
type Auditable struct {
    Version int32
}

func (a *Auditable) Schema(t *storm.Table) {
    t.Col(&a.Version).Default("0").Version()
}

type Order struct {
    storm.Model
    Auditable          // version, and its declaration, come along

    Total storm.Decimal
}
```

A mixin looks exactly like a model — exported, with a `Schema` method — and
being *embedded* is the only thing that tells them apart. Storm classifies it
that way for exactly that reason, and generates no table for it.

## 4. Relationships

**Foreign key** — name the other struct as a field. The column and the
constraint are inferred:

```go
type Article struct {
    storm.Model
    Author Author        // author_id uuid, references authors(id)
}
```

**Has-many** — a slice of the child. Declare it only if you traverse that way.

```go
type Author struct {
    storm.Model
    Articles []Article
}
```

**One-to-one** is a foreign key that is unique. There is no separate
declaration, because there is no separate concept:

```go
func (p *Profile) Schema(t *storm.Table) {
    t.Col(&p.User).Unique().OnDelete(storm.Cascade)   // unique FK => one-to-one
}
```

**Many-to-many** is a slice on **both** sides. Storm generates the join table:

```go
type Post struct { storm.Model; Tags []Tag }
type Tag  struct { storm.Model; Posts []Post }
// → post_tags(post_id, tag_id)
```

Three round trips at any row count — parents, links, far side — not one per
parent. A join would return the same tag once per post carrying it, which is the
row multiplication a batch loader exists to avoid.

**Self-referential many-to-many** is a slice of the same type. The two columns
cannot both be named for the table, so the second is named for the **field**:

```go
type Post struct { storm.Model; Related []Post }
// → post_related(post_id, related_id), both referencing posts
```

The edge is directed and stored once: inserting A→B does not make B→A, because
"related to" and "follows" are both spelled this way and only one is symmetric.
Two self-referential slices to the same type — `Following` and `Followers` — are
**refused**. They are one relationship seen from both ends, storm cannot tell
that from two, and two generated tables would mean following somebody does not
make you their follower. That is a wrong answer, not a missing feature.

**With a payload**, write the join as a model and name it. Storm generates
nothing — your model *is* the join table:

```go
func (o *Org) Schema(t *storm.Table) {
    t.Through(&o.Members, &Membership{})
}
```

## 5. Table-level constraints

```go
t.Unique(&u.TenantID, &u.Email)          // composite
t.Check(storm.RawSQL(`total >= 0`))
```

An **exclusion constraint** is the one worth spelling out, because the
alternative is a race:

```go
func (b *Booking) Schema(t *storm.Table) {
    // EXCLUDE USING gist (room WITH =, during WITH &&): no two rows may share
    // a room AND overlap in time.
    t.Exclude(
        storm.With(&b.Room, storm.OpEq),
        storm.With(&b.During, storm.OpOverlaps),
    )
}
```

Storing two timestamps and checking in Go loses the boundary cases, and races
anyway. A violation arrives as `runtime.ErrExclusionViolation`.

## 6. Polymorphic associations

Two strategies, and the choice is about referential integrity.

**`storm.OneOfN` — the exclusive arc.** One nullable FK column per variant, with
a check constraint that exactly one is set. The database still enforces every
reference:

```go
type Attachment struct {
    storm.Model
    Subject storm.OneOf2[Post, Comment]     // OneOf2 … OneOf8
}
```

**`storm.AnyRef` — the discriminator.** One `subject_type` + `subject_id` pair,
Rails-style. It cannot have a foreign key, so the database can no longer tell you
the row exists — and storm makes you say that out loud:

```go
type AuditEntry struct {
    storm.Model
    Subject storm.AnyRef
}

func (a *AuditEntry) Schema(t *storm.Table) {
    t.Col(&a.Subject).AcknowledgeNoFK("audit rows outlive their subjects by design")
}
```

Without `AcknowledgeNoFK`, generation fails naming referential integrity and
pointing at `OneOf`. The reason is not paperwork: an `AnyRef` is the one place
storm cannot give the guarantee it gives everywhere else, and a cost you
acknowledged is different from one you did not notice.

## 7. Declared reads

The reads whose *shape* must be known at build time are declared on the model:
`Plans`, `Projections`, `Aggregates` and `Joins`. [[API]] §6 and §7 cover all
four with examples.

A **union** is the exception, and the only declared read that is not a method:
it merges several tables and belongs to none, so it is a package-level
`storm.Union` var passed to `Build` with the models
([ADR-0008](adr/0008-union-has-no-driving-table.md)). See [[API]] §7.

Two things live only here.

### n children per parent

On the plan's query, not in the declaration:

```go
p, err := store.CommentWithReplies().
    ChildOrder(comment.CreatedAt.Desc(), comment.ID.Asc()).
    ChildTop(3).                    // three replies PER COMMENT, not three total
    All(ctx, ex)
```

`ChildTop` requires `ChildOrder`, and that ordering must be a strict total
order — two rows sharing a `created_at` would otherwise give a different answer
per run. `ChildLimit(n)` is the other one and means something different: a cap on
the total children fetched, not per parent.

This is the query every ORM gets wrong the same way. In GORM, Ent and Bun a
`Limit` inside an eager load applies to the whole loaded set, so "one post each"
across fifty users returns one post *total*. Here they are two methods with two
names.

It lowers to a `LATERAL` join by default, chosen by measurement rather than
argument: measured 2026-08-24, LATERAL beat the `row_number()` form at every
parent count and every n — 3.5x at one parent, 33x at a hundred — because the
window form reads every child of every matched parent and discards the ones past
n, so its cost tracks the total child count rather than the rows returned.
`DISTINCT ON` is deliberately absent: it only expresses n = 1, so it would be a
strategy that silently stopped applying when somebody changed `ChildTop(1)` to
`ChildTop(2)`.

### Walking a self-reference

A model whose foreign key points at its own table gets two generated
traversals, each a single `WITH RECURSIVE`:

```go
subtree, err := org.Descend(ctx, ex, [][16]byte{rootID}, 10)  // root + 9 levels
chain,   err := org.Ascend(ctx, ex, [][16]byte{leafID}, 10)   // leaf -> root
```

The roots are included, at depth 1, so `maxDepth` of 1 returns exactly the
roots. A depth bound is **required** — `ErrDepth` otherwise — because
`parent_id` being a foreign key does not stop A pointing at B pointing at A, and
unbounded recursion over a cycle does not return. The generated statement also
carries an explicit path array and refuses a row already on it, so a cycle in
the data cannot hang the connection.

Rows come back in **no guaranteed order**. A tree has no total order, inventing
one would be a lie, and every row carries its `parent_id` — so the caller
reassembles the shape it wanted.

## 8. What is not built

Listed because the previous version of this file documented all of it as though
it were:

- **Streaming / `Iter`.** Reads are `All`, `One`, `Count`, `Exists`. There is no
  iterator and no cursor API.
- **Declaration-time `Latest`/`Earliest`/`Top` on a relation.** The per-parent
  limit is `ChildTop`/`ChildOrder` at the call site, as above.
- **Call-site `GroupBy(...).Select(...)`.** Not missing — refused. An unbounded
  set of result shapes can have neither a generated scanner nor a compiled
  statement, which is the whole thesis. Aggregations are declared; [[API]] §7.
- **`Between`, `HasPrefix`/`HasSuffix`, jsonb `Path`.** The predicate vocabulary
  is generated per column type and is listed in [[API]] §3.
- **Set-based `UPDATE`/`DELETE … WHERE`.** Writes are per row, or batched per
  row. A bulk state transition or a purge is `storm.SQLExec`.
- **Row locking.** No `FOR UPDATE`, no `SKIP LOCKED`. A version column makes a
  stale writer lose loudly, which is the lost update; a queue worker that wants
  to claim a row uses `storm.SQL[T]`.
- **Package-level `user.Get(ctx, db, id)` one-liners.** It is
  `user.New().IDEq(id).One(ctx, ex)`, plus a generated shorthand per column.
- **MySQL at run time.** There is a MySQL DDL back end, but it is not a runtime
  target ([ADR-0007](adr/0007-mysql-runtime-needs-a-second-decoder-family.md)).
