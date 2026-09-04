---
tags: [storm, example, quickstart]
updated: 2026-09-04
status: as-built — this is [`examples/blog`](../examples/blog), which runs as a test in CI
---

# The quickstart

Two models, from declaration to query. This is the 80% path; everything
exotic — polymorphic associations, many-to-many with a payload, self-referential
hierarchies — lives in [[REFERENCE]] and you can ignore it until you need it.
[[API]] is the same ground in more depth.

This file tracks [`examples/blog`](../examples/blog) deliberately. That module
runs as a test in CI against a real PostgreSQL, so if the two ever disagree,
it is right and this is a bug.

## 1. The model is a struct

```go
// model/model.go
package model

import (
    "time"

    "github.com/gsoultan/storm"
)

type Author struct {
    storm.Model            // uuid id + created_at/updated_at, database defaults

    Name  string
    Email string

    Articles []Article     // has-many — declare it only if you traverse this way
}

type Article struct {
    storm.Model

    Title       string
    Body        string
    PublishedAt *time.Time // nullable

    Author Author          // foreign key → authors.id
}
```

That is the whole schema. What the generator infers:

| From | It infers |
|---|---|
| type name `Article` | table `articles` |
| field name `PublishedAt` | column `published_at` |
| `string`, `time.Time`, `storm.Decimal`, … | the column type |
| `*time.Time` | the column is nullable |
| `Author Author` | `author_id uuid` + a foreign key |
| `[]Article` | a has-many, loaded through a named plan |
| `storm.Model` | `id`, `created_at`, `updated_at`, with database defaults |

Anything the type cannot say goes in a `Schema` method, using **field
pointers** — so the editor enforces the names, and renaming a field refactors
its declaration with it:

```go
func (a *Author) Schema(t *storm.Table) {
    t.Col(&a.Email).Size(320)
    t.Unique(&a.Email)
}

func (ar *Article) Schema(t *storm.Table) {
    t.Col(&ar.Title).Size(300)
    t.Col(&ar.Author).OnDelete(storm.Cascade)
}
```

Two more optional methods declare the reads whose shape must be known at
build time:

```go
// One round trip per relation, whatever the row count.
func (a *Author) Plans(p *storm.Plans) {
    p.Named("Feed").With(&a.Articles)
}

// A column subset — two columns instead of the row, and a covering index away
// from an index-only scan.
func (a *Author) Projections(p *storm.Projections) {
    p.Named("Card", &a.Name, &a.Email)
}
```

## 2. Generate

Storm finds the models by parsing your module, so there is nothing to configure
and nothing to point it at:

```console
$ storm generate store
  → store/article/article.gen.go (55364 bytes)
  → store/author/author.gen.go   (51899 bytes)
  → store/store.gen.go           (1828 bytes)
  3 package(s) from 2 table(s)
```

One package per table, plus a context package for the reads that span tables.
Commit the output; `storm verify -stale` fails CI if it drifts, and needs no
database to say so.

For the schema itself:

```console
$ storm diff create_blog        # writes db/migrations/0001_create_blog.up.sql
```

storm **never applies DDL** ([ADR-0001](adr/0001-model-first-migration-mediated-ddl.md)).
It writes a migration you review and run with whatever you already use.

## 3. Query

```go
published, err := article.New().
    Where(article.AuthorID.Eq(ada.ID), article.PublishedAt.IsNotNull()).
    Order(article.PublishedAt.Desc()).
    All(ctx, ex, nil)

one, ok, err := author.New().IDEq(id).One(ctx, ex)
n,      err := author.New().Count(ctx, ex)
```

Columns are typed values, not strings, so a rename is a compile error and an
illegal predicate does not exist:

```go
article.Title.ILike("%engine%")   // ok
article.AuthorID.ILike("%x%")     // compile error: UUIDCol has no method ILike
```

### Dynamic filters

The part that looks boring and is the entire point:

```go
q := article.New().Where(article.AuthorID.Eq(id))
q = q.WhereIf(f.PublishedOnly, article.PublishedAt.IsNotNull())
q = q.WhereIf(f.Search != "", article.Title.ILike("%"+f.Search+"%"))

rows, err := q.Order(article.CreatedAt.Desc()).Limit(50).All(ctx, ex, nil)
```

Each `Where` appends a compiler-generated id — for the column and the operator,
never the value. A given combination of filters compiles to SQL **once, ever**,
and a warm call allocates nothing to build it. Values travel as bound arguments,
so there is no string in the statement to escape and no injection surface to
defend.

Keyset pagination takes the last row you were given; there is no cursor to
encode or forge:

```go
p1, _ := article.New().Order(article.Title.Asc(), article.ID.Asc()).
    Limit(2).All(ctx, ex, nil)
p2, _ := article.New().Order(article.Title.Asc(), article.ID.Asc()).
    After(p1[len(p1)-1]).Limit(2).All(ctx, ex, nil)
```

## 4. Relations

A base row has **no relation fields at all**:

```go
a, _, _ := author.New().IDEq(id).One(ctx, ex)
a.Articles      // compile error: type author.Row has no field Articles
```

To load the relation, use the plan you declared:

```go
feed, err := store.AuthorFeed().Limit(10).All(ctx, ex)
feed[0].Articles        // []article.Row — this type has the field
```

**Two round trips, whatever the row count.** Ten authors or ten thousand, it is
two queries — the parents, then their articles by a key array. An N+1 is not
something you have to remember to avoid here; reading an unloaded relation does
not compile, so it is not expressible.

The semi-join — "authors who *have* a published article" — takes the child's own
typed predicates and lowers to one `EXISTS` probe, with no join fan-out and no
`DISTINCT`:

```go
n, err := store.AuthorHavingArticles(
    author.New(),
    article.PublishedAt.IsNotNull(),
).Count(ctx, ex)
```

And the projection you declared is a method on the query:

```go
cards, err := author.New().Order(author.Name.Asc()).AllCard(ctx, ex)  // []author.CardRow
```

## 5. Write

Insert is a masked builder. Absence is tracked by the mask and never inferred
from a zero value, so a column you did not set takes its **database default**
rather than a Go zero:

```go
na := author.Create()
na.SetName("Ada")
na.SetEmail("ada@example.com")
ada, err := na.Insert(ctx, ex)      // id and created_at come back filled
```

Update is the same idea — only what you assigned is written, and one statement
is compiled per distinct set of assigned columns:

```go
m := author.Mutate(ada)
m.SetName("Ada L.")
err := m.Update(ctx, ex)
```

For a graph write, the unit of work stages statements in any order and flushes
them in foreign-key order, in one round trip:

```go
u := store.NewUnit()
u.Add(article.Table, article.InsertOp(article.Row{
    ID: artID, Title: "Compilers", AuthorID: graceID,   // author does not exist yet
}))
u.Add(author.Table, author.InsertOp(author.Row{
    ID: graceID, Name: "Grace", Email: "grace@example.com",
}))
_, err := u.Flush(ctx, ex)
```

## 6. Transactions

There is no `WithTx` plumbing and no `XxxTx` duplicates. `Executor` is a
four-method port, a transaction satisfies it, and the same generated code runs
inside one:

```go
tx, _ := pool.Begin(ctx)
txe := pgxdrv.Tx{T: tx}

nb := author.Create()
nb.SetName("Ephemeral")
nb.SetEmail("gone@example.com")
_, err := nb.Insert(ctx, txe)

tx.Rollback(ctx)        // and the row is gone
```

This is also the testing story: hand your code a transaction, roll it back, and
the code under test is the code that runs in production. There is no mock layer
because there is nothing to mock.

## 7. When you need real SQL

```go
var TopAuthors = storm.SQL[AuthorRow](`
    SELECT u.id, u.name, count(p.id) AS posts
    FROM authors u JOIN articles p ON p.author_id = u.id
    WHERE u.org_id = $1
    GROUP BY u.id, u.name
    ORDER BY posts DESC LIMIT $2`)

rows, err := TopAuthors.Query(ctx, ex, orgID, 10)
```

`storm generate` PREPAREs it against the model, matches the result descriptor to
`AuthorRow`, and emits the scanner — so a wrong column name fails the build, not
the request. **Escaping costs you no type safety.**

It costs you no safety either: only statements the generator saw will run, so a
query assembled at run time is refused before it reaches the server. It must be
a package-level `var`, which is also how the generator finds it.

## That is the whole surface

Declare a struct, generate, query, name a plan, write, transact, and drop to SQL
when you need it. Seven things.

What is deliberately absent: lazy loading, a session, dirty tracking of
everything you ever touched, and a string-typed query language. What you give up
for that is a generate step and having to name the reads whose shape has to be
known in advance.

Next: [[API]] for the same ground in depth — including declared aggregations,
window frames and joins across tables — and [[REFERENCE]] for the parts this
page skipped.
