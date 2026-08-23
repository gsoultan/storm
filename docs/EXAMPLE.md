---
tags: [raorm, example, quickstart]
updated: 2026-08-23
status: proposed — illustrative design, not implemented
---

# The whole thing, in one page

Three models, from declaration to query. This is the 80% path. Everything
exotic — every scalar type, polymorphic associations, recursive hierarchies —
lives in [[REFERENCE]] and you can ignore it until you need it.

## 1. The model is a struct

```go
// internal/model/model.go
package model

import (
    "time"
    "github.com/gsoultan/raorm"
)

type Org struct {
    raorm.Model                 // ID uuid.UUID, CreatedAt, UpdatedAt

    Name  string
    Users []User                // has-many — declare only if you traverse this way
}

type User struct {
    raorm.Model

    Email string
    Name  string
    Age   *int                  // *T = nullable

    Org   Org                   // belongs-to, required → org_id uuid NOT NULL
    Posts []Post                // has-many
}

type Post struct {
    raorm.Model

    Title       string
    Body        string
    PublishedAt *time.Time

    Author User                 // → author_id, from the FIELD name
}
```

That is the entire schema. No DSL, no `Schema()` method, no per-entity files.

**What the generator infers, and the whole list of rules:**

| From | It infers |
|---|---|
| field name `PublishedAt` | column `published_at` |
| `string`, `int`, `time.Time`, `uuid.UUID` … | the column type |
| `*T` | nullable; a value type is `NOT NULL` |
| embedded `raorm.Model` | `id uuid PRIMARY KEY DEFAULT uuidv7()`, `created_at`, `updated_at` |
| `Org Org` | FK column `org_id → orgs.id`, `NOT NULL` |
| `Org *Org` | the same FK, nullable |
| `Posts []Post` | has-many; the FK is found on `Post` |
| `[]T` on both sides | many-to-many + join table |
| `t.Col(&u.Email).Unique()` | a unique index |

**Pointer means optional — for scalars and relations alike.** A to-one relation
does not need a pointer for the type to be finite, because the slice on the
other side provides the indirection. Self-references are the exception: `Parent
*Org` must be a pointer, which makes them optional, which is correct — a root
has no parent.

**The FK column name comes from the field name, not the type.** `Author User`
and `Reviewer User` on the same struct give you `author_id` and `reviewer_id`
with nothing to declare. Override with `t.Col(&p.Owner).Named("owner_id")` when
you must — that string names a *database* column, not a Go field.

You never write the scalar yourself, but you still get it: `user.Row` has
`OrgID uuid.UUID`, `user.OrgID` is a typed column for predicates, and
`user.New(email, name, orgID)` takes the id — so setting a foreign key never
requires loading the parent.

Anything the type cannot say goes in one optional method, and **it is objects
all the way down**:

```go
func (u *User) Schema(t *raorm.Table) {
    t.Col(&u.Email).Unique().Size(320)          // per-column settings
    t.Col(&u.Org).OnDelete(raorm.Restrict)

    t.Index(&u.Org, raorm.Desc(&u.CreatedAt))   // table-level constraints
    t.Check(raorm.Between(&u.Age, 0, 150))
}
```

**No strings.** `&u.Email` is a field pointer, so a rename is a compile error and
a typo never compiles. The receiver must be a **pointer** (`u *User`): a value receiver copies the
struct, so `&u.Email` would point into the copy; the generator resolves each pointer to a column by field offset.

Literals like `0` and `150` stay literals — they are values, not identifiers,
and their types are checked against the field at generate time.

Per-column settings go through `t.Col(&field)` — `.Unique()`, `.Size(n)`,
`.Default(v)`, `.Immutable()`, `.OnDelete(...)`. **There are no struct tags.**
The struct holds your domain; the method holds the database's opinions about it.

## 2. Generate

```console
$ go tool raorm generate
  orgs   → store/org    (4 columns, 1 relation)
  users  → store/user   (7 columns, 3 relations)
  posts  → store/post   (7 columns, 1 relation)
  ✓ 3 tables · db/migrations/0001_init.up.sql (review and commit)
```

One command, one output directory you never open, no install step. Generated
code is `// Code generated` — commit it or gitignore it, your call. `raorm
verify` fails CI if it is stale.

## 3. Query

```go
// By primary key.
u, err := user.Get(ctx, db, id)

// A filter.
us, err := user.All(ctx, db, user.Age.Gte(18))

// A real query.
us, err := user.Query().
    Where(user.OrgID.Eq(orgID), user.Email.HasSuffix("@corp.com")).
    OrderBy(user.CreatedAt.Desc()).
    Limit(50).
    All(ctx, db)
```

Columns are typed values, so `user.Age.Eq("old")` and `user.Emial` are both
compile errors.

### Dynamic filters

```go
q := user.Query().Where(user.OrgID.Eq(orgID))

if f.Name != ""  { q = q.Where(user.Name.ILike("%" + f.Name + "%")) }
if f.MinAge > 0  { q = q.Where(user.Age.Gte(f.MinAge)) }

us, err := q.Limit(f.Limit).All(ctx, db)
```

Ordinary Go — the same code you would write in GORM. Underneath, each `.Where`
sets a bit in a `uint64`; that combination's SQL is compiled once for the life of
the process instead of rebuilt on every call. **This is the whole thesis, and it
costs you nothing to read.**

## 4. Relations

`user.Row` has no `Posts` field, so the N+1 mistake is not available:

```go
u, _ := user.Get(ctx, db, id)
u.Posts        // compile error: user.Row has no field Posts
```

Name a plan for what you want loaded:

```go
// store/plans.go
var UserWithPosts = user.Plan("WithPosts").With(user.Posts).With(user.Org)
```

```go
u, err := user.Query().Where(user.ID.Eq(id)).Load(UserWithPosts).One(ctx, db)

u.Org.Name                            // typed, guaranteed loaded
for _, p := range u.Posts { … }       // typed, guaranteed loaded — 2 queries, not 51
```

Want only the *newest* post per user rather than all of them?

```go
var UserWithLatestPost = user.Plan("WithLatestPost").
    With(user.Posts.Latest(post.CreatedAt))
```

```go
us, err := user.Query().Where(user.OrgID.Eq(orgID)).
    Load(UserWithLatestPost).All(ctx, db)

for _, u := range us {
    if p, ok := u.Posts.Get(); ok {   // ONE post, not a slice — each user's own
        fmt.Println(u.Name, "→", p.Title)
    }
}
```

50 users give 50 *different* posts, one each, in 2 round trips. GORM, Ent and
Bun all return one post *in total* here, because their `Limit` inside an eager
load caps the whole set rather than each parent. `.Earliest(...)` gives the
first post instead; `.LatestN(3, ...)` gives the newest three per user.

Plans are the one file listing every load pattern in the app, so CI can cost them:

```console
$ raorm lint --plans
  UserWithPosts       2 round trips   users ⋈ orgs → posts
  UserWithLatestPost  2 round trips   users → posts (DISTINCT ON)
```

## 5. Write

```go
u, err := user.Insert(ctx, db, user.New("ada@corp.com", "Ada", orgID))

n, err := user.Update(ctx, db,
    raorm.Where(user.ID.Eq(id)),
    user.Name.Set("Ada Lovelace"),
)

n, err := user.Delete(ctx, db, raorm.Where(user.ID.Eq(id)))
```

`user.New` takes the `NOT NULL` fields that have no default, positionally —
forget one and it does not compile. Everything else is an option:
`user.New("ada@corp.com", "Ada", orgID, user.WithAge(36))`.

## 6. Transactions

```go
err := db.Tx(ctx, func(tx raorm.Tx) error {
    u, err := user.Get(ctx, tx, id)          // tx works anywhere db works
    if err != nil { return err }

    _, err = post.Insert(ctx, tx, post.New("Hello", "…", u.ID))
    return err                                // any error rolls back
})
```

`db` and `tx` satisfy the same interface, so there are no `XxxTx` duplicates and
no `WithTx(...)` plumbing.

## 7. When you need real SQL

```go
var TopAuthors = raorm.SQL[AuthorRow](`
    SELECT u.id, u.name, count(p.id) AS posts
    FROM users u JOIN posts p ON p.author_id = u.id
    WHERE u.org_id = $1
    GROUP BY u.id, u.name
    ORDER BY posts DESC LIMIT $2`)

rows, err := TopAuthors.Query(ctx, db, orgID, 10)
```

`PREPARE`d at generate time, so a wrong column name fails the build, and
`AuthorRow` gets a generated scanner. **Escaping costs you no type safety.**

---

## That is the whole surface

Seven things: declare a struct, generate, query, name a plan, write, transact,
escape. If you know SQL you know the rest.

What is deliberately absent: no `Schema()` DSL to learn, no session or
persistence context, no lazy loading, no `AutoMigrate`, no `Preload` to remember,
no reflection.

One thing worth knowing now: you declare `model.User`, and queries return
`user.Row`. The rule is mechanical — **`user.Row` is your struct with each
relation replaced by its scalar foreign key, and `*T` rewritten to
`raorm.Null[T]`**. So `Org Org` becomes `OrgID uuid.UUID`: you keep the id
without loading anything, and `u.Org` stays a compile error until a plan loads
it.

**Next:** [[REFERENCE]] for the full type table, foreign key cascade rules,
one-to-one, many-to-many with payload, self-referential hierarchies, polymorphic
associations, unit-of-work graph writes, and optimistic locking.

**Or** [[COMPLEX-QUERIES]] if you want to see whether the object form survives
contact with a real reporting query — MRR dashboards, churn cohorts,
"bought X but never Y", double-booking prevention, faceted search.
