---
tags: [storm, api, dx]
updated: 2026-08-23
status: proposed — illustrative, not implemented
---

# The API, by example

> **As-built note (2026-08-25).** This document predates the implementation
> and parts of it are still the *design sketch*. Where they differ, the
> generated code and [`examples/blog`](../examples/blog) — which runs as a test
> in CI — are the truth. Known drift, corrected inline below: entry points are
> `user.New()` (not `user.Query()`); reads are `One`/`All` (there is no `Get`
> and no `Iter` yet); inserts are the masked builder `user.Create()` (not an
> `Insert(row)` function), because absence is tracked by a mask, never
> inferred from a zero value; plans and projections are declared in the model
> with **field pointers**, not package-level `Plan(...)` values.


> For a complete worked domain — every scalar type, foreign keys, one-to-one,
> many-to-many with payload, self-referential hierarchies, **polymorphic
> associations in three strategies**, and transactions — see [[EXAMPLE]].
> This document is the tour; that one is the whole map.

> Design goal, stated as a test: **a GORM user should be productive in an hour,
> and should not be able to write an N+1 at all.**

Every snippet below is illustrative design, not shipped code. The ordering is
deliberate — simplest thing first, because that is the order a new user meets it.

## Design principles

1. **Read like SQL.** Bun's lesson: the closer the API is to SQL, the less there
   is to learn.
2. **Identifiers are typed values, never strings.** A column rename is a compile
   error, not a 2 a.m. page.
3. **The fetch plan is visible at the call site.** You can count the queries a
   function runs by reading it.
4. **The common case is one line.** GORM's lesson. `user.Get(ctx, db, id)`.
5. **Errors name the query, the shape, the source line, and the SQL.**
6. **Generated code is code a human reviews**, not a wall to scroll past.

---

## 1. Declare the model

The model is a **plain Go struct** (ADR-0001). No DSL, no `Schema()` method, no
per-entity files.

```go
// internal/model/model.go
package model

type Org struct {
    storm.Model                 // ID uuid.UUID, CreatedAt, UpdatedAt
    Name  string
    Users []User                // has-many
}

type User struct {
    storm.Model

    Email string
    Name  string
    Age   *int                  // *T = nullable

    Org   Org                   // belongs-to → org_id uuid NOT NULL
    Posts []Post                // has-many
}
```

Everything derivable comes from the type: field name → column, `*T` → nullable,
`Org Org` → the FK column `org_id`, `[]T` → has-many, `[]T` on both sides →
many-to-many. A named `string` type becomes a native enum; any other struct type
becomes typed `jsonb`.

**You never write the FK field.** `Org Org` is the whole declaration — the column
name comes from the *field* name, so `Author User` and `Reviewer User` coexist
without ceremony. The scalar is still generated: `user.Row.OrgID`, the typed
column `user.OrgID`, and `user.New(email, name, orgID)`.

Everything else goes in one optional method, and **there are no strings in it**:

```go
func (u *User) Schema(t *storm.Table) {
    t.Col(&u.Email).Unique().Size(320)          // per-column settings
    t.Col(&u.Org).OnDelete(storm.Restrict)

    t.Index(&u.Org, storm.Desc(&u.CreatedAt))   // table-level constraints
    t.Check(storm.Between(&u.Age, 0, 150))
}
```

**No strings.** `&u.Email` is a field pointer, so a rename is a compile error and
a typo never compiles. The receiver must be a **pointer** (`u *User`): a value receiver copies the
struct, so `&u.Email` would point into the copy; the generator resolves each pointer to a column by field offset.

Literals like `0` and `150` stay literals — they are values, not identifiers,
and their types are checked against the field at generate time.

Per-column settings go through `t.Col(&field)`. **There are no struct tags** —
the struct is your domain, the method is the database's opinions about it, and
neither contains a Go identifier written as a string.

Full type table, cascade rules, one-to-one, many-to-many with payload,
self-referential hierarchies and polymorphic associations: [[REFERENCE]] §1.


## 2. Generate

```yaml
# storm.yaml
version: 1
model: ./internal/model
targets:
  - dialect:    postgres
    version:    "16"
    out:        internal/store
    migrations: db/migrations
portability:
  assert: [postgres]      # add mysql8 / oracle19 / mongo7 to check at build time
```

```console
$ storm generate
  users  → internal/store/user   (12 columns, 3 relations, 4 plans)
  posts  → internal/store/post   (9 columns, 2 relations, 2 plans)
  ✓ 21 tables, 0 unsupported types, deterministic output

$ storm migrate diff add_user_status
  → db/migrations/0007_add_user_status.up.sql   (2 statements, non-destructive)
  review and commit — storm never applies a migration
```

Adopting an existing database instead? `storm import` writes the model **from**
the live schema, once, and you own it from there. Database-first is the on-ramp,
not the steady state.


## 3. Typed columns, not strings

For table `users`, the generator emits typed handles:

```go
package user // internal/store/user — generated

var (
    ID        = storm.OrdCol[uuid.UUID]{...}
    Email     = storm.TextCol{...}
    Age       = storm.OrdCol[int32]{...}
    Status    = storm.Col[Status]{...}     // enum → generated Go type
    Metadata  = storm.JSONCol{...}
    Tags      = storm.ArrayCol[string]{...}
    CreatedAt = storm.OrdCol[time.Time]{...}
    DeletedAt = storm.OrdCol[*time.Time]{...}
)
```

The column *kind* decides which predicates exist, so autocomplete only ever
offers you legal ones:

| Kind | Predicates |
|---|---|
| `Col[T]` | `Eq` `NotEq` `In` `NotIn` `IsNull` `IsNotNull` |
| `OrdCol[T]` | + `Gt` `Gte` `Lt` `Lte` `Between` `Asc` `Desc` |
| `TextCol` | + `Like` `ILike` `HasPrefix` `HasSuffix` |
| `JSONCol` | + `HasKey` `Path` `Contains` |
| `ArrayCol[T]` | + `Contains` `Overlaps` `Len` |

```go
user.Age.Gte(18)              // ok
user.Age.Eq("eighteen")       // compile error: cannot use string as int32
user.Metadata.Gt(5)           // compile error: JSONCol has no method Gt
user.Emial.Eq("a@b.com")      // compile error: undefined
```

Compare Ent's free functions (`user.AgeGTE(18)`, a flat namespace of hundreds)
and Bun/GORM's `"age >= ?"`. Methods on typed columns give a smaller namespace,
better autocomplete, and stricter checking than either.

## 4. The one-liners

```go
u,  err := user.Get(ctx, db, id)                    // by primary key
u,  err := user.GetByEmail(ctx, db, "a@b.com")      // by unique index
us, err := user.All(ctx, db, user.Status.Eq(user.StatusActive))
n,  err := user.Count(ctx, db, user.Age.Gte(18))
ok, err := user.Exists(ctx, db, user.Email.Eq(e))
```

Generated per table from the primary key and each unique index. Fetching a row
by ID should never require building a query.

## 5. The query builder

```go
us, err := user.Query().
    Where(user.TenantID.Eq(tid), user.Age.Gte(18)).   // variadic = AND
    OrderBy(user.CreatedAt.Desc(), user.ID.Asc()).
    Limit(50).
    All(ctx, db)
```

Terminals: `.All()` `.One()` `.First()` `.Count()` `.Exists()` `.Page()` `.Iter()`.

`.Iter()` returns a Go 1.23 iterator, so streaming a large result never
materialises a slice:

```go
for u, err := range user.Query().Where(...).Iter(ctx, db) {
    if err != nil { return err }
    process(u)
}
```

Composition uses the specification pattern — predicates are values, so they
live in your domain layer where they belong:

```go
// internal/authz/spec.go
var Active = storm.And(user.DeletedAt.IsNull(), user.Status.Eq(user.StatusActive))
func InTenant(t uuid.UUID) storm.Pred { return user.TenantID.Eq(t) }

// call site
user.Query().Where(Active, InTenant(tid), storm.Or(
    user.Age.Gte(18),
    user.Metadata.HasKey("guardian"),
))
```

## 6. Dynamic filters — this is the whole thesis, and it looks boring

```go
q := user.Query().Where(user.TenantID.Eq(tid))

if f.Email != "" {
    q = q.Where(user.Email.Eq(f.Email))
}
if f.MinAge > 0 {
    q = q.Where(user.Age.Gte(f.MinAge))
}
if f.CreatedAfter != nil {
    q = q.Where(user.CreatedAt.Gt(*f.CreatedAfter))
}

us, err := q.OrderBy(user.CreatedAt.Desc()).Limit(f.Limit).All(ctx, db)
```

Or fluently, if you prefer no branches:

```go
us, err := user.Query().
    Where(user.TenantID.Eq(tid)).
    WhereIf(f.Email != "",       user.Email.Eq(f.Email)).
    WhereIf(f.MinAge > 0,        user.Age.Gte(f.MinAge)).
    WhereIf(f.CreatedAfter != nil, user.CreatedAt.Gt(f.After())).
    All(ctx, db)
```

**That is identical to what you would write in GORM or Bun.** The point is what
does *not* happen underneath:

- `Query()` returns a **value type** with an inline `[8]pred` array and a
  `uint64` shape mask. It spills to the heap only past eight predicates.
- Each `.Where()` sets a bit and writes into the array. No allocation, no
  interface boxing, no string building.
- `.All()` reads the shape mask, does one atomic load into the compiled-statement
  table, binds args into a pooled slice, and executes.

Sixty-four filter combinations produce sixty-four compiled statements, each
built once for the life of the process and each mapping onto its own server-side
prepared statement. GORM, Ent, and Bun rebuild the string on every call forever.

**You do not trade ergonomics for speed here. That is the entire bet.**

## 7. Relations: named fetch plans

Base row types have no relation fields at all:

```go
type Row struct {                 // user.Row — generated
    ID        uuid.UUID
    Email     string
    Age       int32
    CreatedAt time.Time
}

u, _ := user.Get(ctx, db, id)
u.Posts       // compile error: u.Posts undefined (type user.Row has no field Posts)
```

To load relations you **name a plan**. Plans live in one file you own:

```go
// internal/store/plans.go
package store

var (
    UserFeed = user.Plan("Feed").
        With(user.Posts.
            Where(post.PublishedAt.IsNotNull()).
            OrderBy(post.PublishedAt.Desc()).
            Limit(20).
            With(post.Comments.Limit(3))).
        With(user.Org)

    UserSummary = user.Plan("Summary").With(user.Org)
)
```

`storm generate` emits one type per plan. The plan is the **entry point**, and
it lives in the package that owns `plans.go` — not on `user.Query`:

```go
us, err := store.UserFeed().Where(user.TenantID.Eq(tid)).All(ctx, db)
// us is []store.UserFeed

for _, u := range us {
    u.Org.Name                    // typed, guaranteed loaded
    for _, p := range u.Posts {   // typed, guaranteed loaded, at most 20
        for _, c := range p.Comments {} // typed, at most 3
    }
}
```

Three properties fall out of this, and each one is a thing another ORM gets
wrong:

- **N+1 is unrepresentable.** Not discouraged, not linted — the field does not
  exist unless it was loaded. Hibernate lazy-loads it, Ent hands you a silently
  empty slice, GORM makes you remember `Preload`.
- **No combinatorial type explosion.** You get exactly the plans you name. This
  is what killed the earlier "generate every `With` combination" design — see
  the R3 note in [[PLAN]].
- **Fetch plans are reviewable artifacts.** `plans.go` is the one file where a
  reviewer can see every load pattern in the system, and CI can cost them:

```console
$ storm lint --plans
  UserFeed      3 round trips   users → posts (LATERAL) → comments (= ANY)
  UserSummary   1 round trip    users ⋈ orgs (JOIN)
  ✓ no plan exceeds the configured limit of 4 round trips
```

That output is the `@EntityGraph` idea plus a performance gate, in one command.

> **Corrected 2026-08-24, after the P2 spike.** This section previously read
> `user.Query().Where(…).Load(UserFeed).All(ctx, db)`. That signature cannot be
> built: **Go methods may not have type parameters**, so a method taking a plan
> *value* has no way to vary its return type by plan — every plan would have to
> return the same row type, which defeats the point. The plan therefore has to
> be the entry point (above) or a generated method per plan.
>
> For the same reason a plan cannot live in a table package: it names two
> tables, and a table package importing a sibling reintroduces the import cycle
> that one-package-per-table avoids (`Org` has `Users`, `User` has an `Org`).
> Plans live in the parent package, which imports every table package and is
> imported by none. See `internal/planspike/` and [[PLAN]] §P2.

## 8. Writes

Insert takes a plain struct. No session, no `Save()`, no active record:

```go
id, err := user.Insert(ctx, db, user.Row{Email: "a@b.com", Age: 30})

ids, err := user.InsertMany(ctx, db, rows)   // one COPY on PG
```

Update is a statement, not a load-mutate-flush cycle:

```go
n, err := user.Update(ctx, db,
    storm.Where(user.ID.Eq(id)),
    user.Email.Set("new@b.com"),
    user.LoginCount.Inc(1),
    user.Tags.Append("verified"),
)
```

Upsert:

```go
err := user.Upsert(ctx, db, row,
    storm.OnConflict(user.Email).DoUpdate(user.Age.SetFromExcluded()))
```

Optimistic locking, if a `version` column is declared in the schema:

```go
n, err := user.Update(ctx, db, storm.Where(user.ID.Eq(id)).At(v), ...)
if n == 0 { return storm.ErrStaleWrite }   // version moved under you
```

## 9. Unit of work — explicit, batched, FK-ordered

Only for graph writes. Everything above works without it.

```go
err := db.Unit(ctx, func(u *storm.Unit) error {
    uid := u.Insert(user.Row{Email: "a@b.com"})       // deferred handle
    oid := u.Insert(org.Row{Name: "Acme"})
    u.Insert(member.Row{UserID: uid, OrgID: oid})     // depends on both
    u.Update(counter.Total.Inc(1), storm.Where(counter.Key.Eq("users")))
    return nil
})
```

`uid` is a **deferred value**, not a UUID — it resolves at flush from
`RETURNING`. The unit sorts statements by FK dependency derived from the schema,
then emits one `pgx.Batch`. Correct ordering is proven in tests with deferred
constraints *off*, so it is the ordering that is right, not Postgres being
forgiving.

No dirty checking of everything you ever loaded. No flush-order surprises. No
`LazyInitializationException`, because there is no lazy anything.

## 10. The escape hatch loses nothing

```go
var TopEarners = storm.SQL[EarnerRow](`
    WITH ranked AS (
        SELECT id, dept, salary,
               row_number() OVER (PARTITION BY dept ORDER BY salary DESC) AS rn
        FROM employees
        WHERE tenant_id = $1
    )
    SELECT r.id, r.dept, r.salary, l.headcount
    FROM ranked r
    JOIN LATERAL (SELECT count(*) headcount FROM employees e WHERE e.dept = r.dept) l ON true
    WHERE r.rn <= $2`)

rows, err := TopEarners.Query(ctx, db, tid, 3)   // []EarnerRow, generated scanner
```

Validated at **build time** by `PREPARE`ing against a dev database (or a
checked-in schema snapshot, so CI needs no live Postgres). If `EarnerRow` does
not match the result descriptor, generation fails:

```
storm: internal/store/reports.go:14  storm.SQL[EarnerRow]
  result column 4 "headcount" (int8) has no field in EarnerRow
  → add `Headcount int64` or alias the column away
```

The no-rows half is `SQLExec` — junction DELETEs, `ON CONFLICT DO NOTHING`
inserts, statements run for their effect. Same declaration discipline, same
`PREPARE`, one extra rule: the statement must return **zero** columns, so "I
meant to read those rows" is a generation error pointing at `SQL[T]`, never a
silent drop of a result set the server actually sent. (A void function call
wraps as `SELECT (fn($1) IS NULL) AS done` and stays `SQL[T]`.)

```go
var DeleteRoleParents = storm.SQLExec(`DELETE FROM role_parents WHERE role_id = $1`)

n, err := DeleteRoleParents.Exec(ctx, db, roleID)   // rows affected
```

And raw fragments compose *into* typed queries as join sources:

```go
user.Query().
    Join(TopEarners.As("te"), storm.On(user.ID.EqCol(TopEarners.Col.ID))).
    Where(user.TenantID.Eq(tid)).
    All(ctx, db)
```

Ent's `.Modify()` and Hibernate's native query both drop you back to untyped
scanning. This does not. **Escaping is a supported path, not a failure.**

## 11. When something goes wrong

```
storm: query failed — user.Query (plan UserFeed)
  at      internal/api/users.go:88
  shape   0x2c  [tenant_id=, age>=, created_at>]
  sql     SELECT u.id, u.email, u.age FROM users u
          WHERE u.tenant_id = $1 AND u.age >= $2 AND u.created_at > $3
          ORDER BY u.created_at DESC LIMIT 50
  args    3 bound (values hidden; set STORM_LOG=debug to include)
  pg      42703  column u.created_at does not exist
```

Source location, shape, the exact SQL, and the driver error — because a
generated ORM knows all four and has no excuse for hiding any of them. Bound
values are never logged above debug level (**sec** veto).

## 12. Testing

```go
func TestFeed(t *testing.T) {
    db := stormtest.New(t)               // transaction per test, auto rollback
    seed(t, db)

    got, err := user.Query().Load(UserFeed).All(ctx, db)
    require.NoError(t, err)
    db.AssertRoundTrips(t, 3)            // your own N+1 guard
    db.AssertNoSeqScan(t)                // EXPLAIN-backed
}
```

The round-trip counter that proves storm's own gates is exported for **your**
tests. An N+1 introduced by a plan change fails your suite, not production.

---

## What this costs you

Honest list, so the tradeoff is visible:

- **A generate step.** `storm generate` must run when migrations change.
  `storm verify` fails CI if generated code is stale.
- **Declaring load patterns up front.** A plan must be named before it is used.
  This is the price of N+1 being impossible, and it is the right price.
- **Generic code over "a user, loaded or not"** needs an interface or a type
  parameter. Rarer than it sounds; awkward when it happens.
- **A schema-diff engine you now depend on.** storm emits migrations from the
  model, so a bad diff can emit a bad migration. Mitigated by review (they are
  ordinary committed files), `--allow-destructive` gating, and golden tests —
  but it is real scope, and ADR-0001 says so.
- **Postgres first.** Other targets are sequenced in [[DIALECTS]]; the
  capability model tells you at build time what will not port.
- **`storm verify` in CI is not optional.** Model, generated code, migrations
  and the live schema are four artifacts that must agree. Three verify modes
  keep them honest; skipping them reintroduces the drift ADR-0001 feared.
