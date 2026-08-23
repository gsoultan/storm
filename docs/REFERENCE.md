---
tags: [raorm, reference, model]
updated: 2026-08-23
status: proposed — illustrative design, not implemented
---

# Reference: the parts you reach for later

[[EXAMPLE]] is the one-page introduction — read that first. This document is the
lookup table for everything it left out: the full type mapping, cascade rules,
one-to-one, many-to-many with payload, self-referential hierarchies, polymorphic
associations, unit-of-work graph writes, and optimistic locking.

The model is always a plain Go struct, and **there are no struct tags** —
per-column settings go through `t.Col(&field)`, table-level ones are direct
calls. Nothing here is a Go identifier written as a string.

## Two structs, one concept

You declare `model.User`. Queries return `user.Row`. They are related by one
mechanical rule:

> **`user.Row` is your struct with each relation replaced by its scalar foreign
> key, and `*T` rewritten to `raorm.Null[T]`.**

So `Org Org` in the model becomes `OrgID uuid.UUID` in the row — you declare the
relationship once and read the id without loading anything.

Relations are dropped because that is what makes N+1 impossible — a `Posts`
field that always exists cannot be a compile error when unloaded (ADR-0003).
`*T` becomes `Null[T]` because a pointer costs an allocation per non-nil field
per row, and the row type is on the hot path. The declaration is never
instantiated at runtime, so it stays idiomatic Go.

---

# Part 1 — The model

## 1.1 Type mapping

| Go type in your struct | Postgres | Notes |
|---|---|---|
| `uuid.UUID` | `uuid` | `raorm.UUID` (`[16]byte`) if you want zero dependencies |
| `string` | `text` | `t.Col(&u.Name).Size(120)` → `varchar(120)` |
| `bool` | `boolean` | |
| `int16` `int32` `int64` `int` | `smallint` `integer` `bigint` `bigint` | |
| `float32` `float64` | `real` `double precision` | never for money |
| `raorm.Decimal` | `numeric` | `t.Col(&u.Bal).Numeric(18, 4)` |
| `[]byte` | `bytea` | `nil` = NULL |
| `time.Time` | `timestamptz` | |
| `raorm.Date` | `date` | |
| `raorm.TimeOfDay` | `time` | |
| `raorm.Interval` | `interval` | **not `time.Duration`** — see below |
| a named `string` type with constants | native `enum` | inferred |
| any other struct type | `jsonb` | inferred; typed scan, no `[]byte` |
| `[]string` `[]int32` `[]uuid.UUID` | `text[]` `integer[]` `uuid[]` | |
| `map[string]string` | `hstore` | |
| `netip.Addr` / `netip.Prefix` | `inet` / `cidr` | stdlib |
| `net.HardwareAddr` | `macaddr` | |
| `raorm.TSVector` | `tsvector` | usually a generated column |
| `raorm.Range[T]` | `int4range` `tstzrange` … | |
| `*T` (any of the above) | the same, `NULL`able | |

Three that bite:

- **`interval` is not `time.Duration`.** Postgres intervals carry months, days
  and microseconds separately, and a month is not a fixed number of seconds.
  `raorm.Interval` keeps all three. `t.Col(&u.TTL).AsDuration()` opts into the
  lossy mapping explicitly.
- **A struct field becomes typed `jsonb`.** `Prefs Prefs` scans straight into
  your struct with a generated codec — never `[]byte` you decode by hand.
- **A named string type becomes a native enum**, and its constants become the
  enum labels. Nothing to declare.

Anything raorm does not know is an explicit escape, never a silent `[]byte`:

```go
Location geo.Point                                          // in the struct
t.Col(&u.Location).Raw("geography(Point,4326)", geo.Codec)  // in Schema
```

## 1.2 Mixins are embedded structs

```go
// internal/model/mixins.go
package model

type TenantScoped struct {
    Tenant Tenant                             // → tenant_id uuid NOT NULL
}

func (ts *TenantScoped) Schema(t *raorm.Table) {
    t.Col(&ts.Tenant).OnDelete(raorm.Restrict)
}

type Timestamps struct {
    CreatedAt time.Time
    UpdatedAt time.Time
}

func (ts *Timestamps) Schema(t *raorm.Table) {
    t.Col(&ts.CreatedAt).Default(raorm.Now()).Immutable()
    t.Col(&ts.UpdatedAt).Default(raorm.Now()).AutoUpdate()
}

type Auditable struct {
    Version   int32                           // optimistic locking
    UpdatedBy *User                           // → updated_by uuid NULL
}

func (a *Auditable) Schema(t *raorm.Table) {
    t.Col(&a.Version).Version()
    t.Col(&a.UpdatedBy).OnDelete(raorm.SetNull)
}
```

Embed them and the columns inline:

```go
type Post struct {
    raorm.Model
    TenantScoped
    Timestamps
    Auditable

    Title string
    Body  string
}
```

An embedded struct's `Schema` runs against the same table, so mixins compose.
`.Immutable()` means the generator emits no setter — `post.CreatedAt.Set(...)`
becomes a compile error rather than a code-review comment.

## 1.3 Foreign keys and cascade rules

A field whose type is another model **is** the foreign key. You never declare
the id column.

```go
type Post struct {
    raorm.Model

    Author User   // → author_id NOT NULL
    Org    Org    // → org_id    NOT NULL
    Editor *User  // → editor_id NULL
}

func (p *Post) Schema(t *raorm.Table) {
    t.Col(&p.Author).OnDelete(raorm.Cascade)
    t.Col(&p.Org).OnDelete(raorm.Restrict).OnUpdate(raorm.Cascade)
    t.Col(&p.Editor).OnDelete(raorm.SetNull)
}
```

**Value = required, pointer = optional**, the same rule as scalars. The column
name comes from the *field* name plus `_id`, so `Author User` and `Editor *User`
coexist with no ambiguity. `t.Col(&p.Editor).Named("reviewer_id")` overrides it;
`t.Col(&u.Org).References(raorm.In(func(o *Org) raorm.Set { return raorm.Of(&o.Slug) }))`
points at a column other than the primary key.

`ondelete=` accepts `cascade`, `restrict`, `setnull`, `setdefault`, `noaction`.
**`setnull` on a value (non-pointer) relation is a generation error** — the
column is `NOT NULL`, so the action could never fire.

Composite foreign keys use the method form, still without strings:

```go
func (u *User) Schema(t *raorm.Table) {
    t.ForeignKey(&u.TenantID, &u.OrgCode).
        References(raorm.In(func(o *Org) raorm.Set { return raorm.Of(&o.TenantID, &o.Code) }))
}
```

Two models that each require the other (`A{B B}` and `B{A A}`) will not compile
in Go, and should not: a mutually-required FK pair cannot be inserted without
deferred constraints. Make one side a pointer.

## 1.4 One-to-one

A **unique** foreign key is what makes it one-to-one:

```go
type Profile struct {
    raorm.Model

    User    User
    Bio     *string
    Website *string
}

func (p *Profile) Schema(t *raorm.Table) {
    t.Col(&p.User).Unique().OnDelete(raorm.Cascade)   // unique → 1:1, not 1:N
    t.Col(&p.Bio).Size(2000)
}

type User struct {
    raorm.Model
    Profile *Profile                        // has-one, inferred from the unique FK
}
```

## 1.5 Self-referential hierarchies

```go
type Org struct {
    raorm.Model
    Name string

    Parent   *Org    // pointer is forced → optional → this org is a root
    Children []Org
}

func (o *Org) Schema(t *raorm.Table) {
    t.Col(&o.Parent).OnDelete(raorm.Cascade)
    t.Inverse(&o.Children, &o.Parent)      // both field pointers, same receiver
}

type Comment struct {
    raorm.Model
    Body string

    Post    Post
    Parent  *Comment
    Replies []Comment
}

func (c *Comment) Schema(t *raorm.Table) {
    t.Col(&c.Post).OnDelete(raorm.Cascade)
    t.Col(&c.Parent).OnDelete(raorm.Cascade)
    t.Inverse(&c.Replies, &c.Parent)
}
```

A self-reference **must** be a pointer or the type would be infinitely sized —
which conveniently makes it optional, and a hierarchy root has no parent anyway.

`t.Inverse(&o.Children, &o.Parent)` pairs the two sides — both are field pointers
on the same receiver, so a rename of either is a compile error. It is needed only
when a model has more than one relation to the same type; elsewhere the inverse
is inferred.

## 1.6 Many-to-many

Plain — declare the slice on both sides and the join table is generated:

```go
type Post struct {
    raorm.Model
    Tags []Tag
}

type Tag struct {
    raorm.Model
    Name  string
    Posts []Post
}
// → post_tags(post_id, tag_id) with a composite PK and both FKs
```

**With payload** — when the join carries its own columns, make it a model:

```go
type UserRole struct {
    User User
    Role Role

    GrantedAt time.Time
    GrantedBy *User
    ExpiresAt *time.Time
}

func (ur *UserRole) Schema(t *raorm.Table) {
    t.PrimaryKey(&ur.User, &ur.Role)
    t.Col(&ur.User).OnDelete(raorm.Cascade)
    t.Col(&ur.Role).OnDelete(raorm.Restrict)
    t.Col(&ur.GrantedAt).Default(raorm.Now())
}

type User struct {
    raorm.Model
    Roles []Role
}

func (u *User) Schema(t *raorm.Table) {
    t.Through(&u.Roles, UserRole{})
}
```

You get both APIs: `u.Roles` (`[]role.Row`) and `u.Roles[i].Grant` carrying
`GrantedAt` and `ExpiresAt`. The second appears only when the join table has
columns beyond its two foreign keys.

## 1.7 Polymorphic associations

The type you choose **is** the strategy.

### `raorm.OneOf[…]` — exclusive arc (the default)

One nullable FK column per variant plus a `CHECK` that exactly one is set.
**Referential integrity is preserved** — every variant is a real foreign key.

```go
type Attachment struct {
    raorm.Model
    TenantScoped

    Filename string
    MimeType string
    Size     int64

    Subject raorm.OneOf[Post, Comment, User]
}

func (a *Attachment) Schema(t *raorm.Table) {
    t.Col(&a.Subject).OnDelete(raorm.Cascade)
}
```

Generated:

```sql
post_id    uuid REFERENCES posts(id)    ON DELETE CASCADE,
comment_id uuid REFERENCES comments(id) ON DELETE CASCADE,
user_id    uuid REFERENCES users(id)    ON DELETE CASCADE,
CONSTRAINT ck_attachments_subject CHECK (
    (post_id IS NOT NULL)::int + (comment_id IS NOT NULL)::int
  + (user_id IS NOT NULL)::int = 1)
-- plus a partial index per variant
```

Use `*raorm.OneOf[...]` for "at most one" — the `CHECK` becomes `<= 1`.

### `raorm.AnyRef` — discriminator (Rails / GORM style)

Two columns, unbounded variants, **no foreign keys possible**:

```go
type AuditLog struct {
    raorm.Model
    TenantScoped

    Action  Action
    Diff    map[string]any                  // → jsonb
    Subject raorm.AnyRef
}

func (a *AuditLog) Schema(t *raorm.Table) {
    t.Col(&a.Subject).AcknowledgeNoFK("audit rows outlive their subjects by design")
}
```

Without that acknowledgement, generation warns:

```
raorm: internal/model/audit.go:9
  AnyRef gives up referential integrity — orphan rows are possible and no
  database constraint will prevent them.
  Prefer raorm.OneOf[...] (≤ 8 variants), or call .AcknowledgeNoFK("<reason>")
```

The reason string lands in the model file, where a reviewer sees the trade-off.

### Choosing

| | Integrity | Variants | Write cost |
|---|---|---|---|
| `OneOf[…]` | ✓ full | ≤ ~8 (a column each) | plain insert |
| `AnyRef` | ✗ none | unbounded | plain insert |
| `OneOf[…]` + supertype table | ✓ full | unbounded | 2 inserts |

Default to `OneOf`. If you expect variants to keep growing, the supertype table
is the honest choice and the extra join is its price.

### Reading a polymorphic field

```go
switch s := a.Subject.(type) {
case attachment.SubjectPost: _ = s.Post.Title
// …
}
```

Go type switches are not exhaustive — add a variant and every switch quietly
falls through to `default`. So the generator also emits `Match`, where **adding
a variant breaks every call site at compile time**:

```go
label := attachment.MatchSubject(a.Subject,
    func(p post.Row) string    { return "post: " + p.Title },
    func(c comment.Row) string { return "comment on " + c.PostID.String() },
    func(u user.Row) string    { return "avatar of " + u.Name },
)
```

Use `Match` for anything that must stay correct as the model grows — the same
instinct as ADR-0003: turn a runtime failure into a compile error.

## 1.8 Table-level constraints

Everything above used `t.Col(&field)` for per-column settings. Table-level
constraints are direct calls on the same object:

```go
func (u *User) Schema(t *raorm.Table) {
    t.Unique(&u.Tenant, raorm.Lower(&u.Email))
    t.Index(&u.Org, raorm.Desc(&u.CreatedAt))
    t.Index(&u.Scopes).Using(raorm.GIN)
    t.Index(&u.Status).Where(raorm.NotEq(&u.Status, StatusSuspended))   // partial
    t.Check(raorm.Between(&u.Age, 0, 150))
    t.Generated(&u.Search, raorm.ToTSVector(raorm.English, &u.Name, &u.Email))
    t.Immutable(&u.CreatedAt)
    t.Default(&u.Status, StatusPending)
    t.Size(&u.Name, 120)
}
```

### Why field pointers, and how they resolve

`&u.Email` is an ordinary Go expression. Rename the field and every reference is
a compile error; mistype it and it never compiles. That is strictly stronger
than a string a generator validates, because the *editor* enforces it and
refactoring tools follow it.

The receiver must be a **pointer** (`u *User`). A value receiver copies the
struct before the method runs, so `&u.Email` points into the copy and cannot be
resolved — raorm rejects that at build time rather than emitting a wrong schema.
Two resolution paths, both real:

- **Generation** reads the method at AST level and resolves `u.Email` to a field
  by name — no execution, so it works on a package that does not yet compile.
- **Runtime** (`raorm verify --drift`) allocates one zero value, calls `Schema`,
  and maps each pointer back to a field by offset from the base address.

### What is still a literal, and why that is fine

`0`, `150`, `StatusPending`, `120` are **values, not identifiers**. They cannot
be stale, and a rename cannot break them. Their types are checked against the
field at generate time. `raorm.English` is a typed constant, not the string
`"english"`.

### The only strings that remain

Three, and none of them is a Go identifier:

| String | Example | Why it cannot be an object |
|---|---|---|
| a **database** identifier you are naming | `t.Col(&p.Editor).Named("reviewer_id")` | you are naming a column, not referring to a Go field |
| a **database type** | `t.Col(&u.Loc).Raw("geography(Point,4326)", geo.Codec)` | it is Postgres syntax, not Go |
| **prose** | `.AcknowledgeNoFK("audit rows outlive their subjects")` | it is documentation |

Plus one deliberate escape, `raorm.Expr(...)`, for SQL raorm does not model.
It is `PREPARE`-checked at generate time and listed by `raorm lint --expr`, so
every occurrence in the codebase is visible in review.

**No string in the model can be stale.** A renamed Go field breaks the build;
these three name things that live in the database or in English.

```go
t.Check(raorm.Expr(`tstzrange(starts_at, ends_at) <> 'empty'`))
```

Every construct above exists so that you almost never reach for that one.

# Part 2 — Generate

```console
$ raorm generate
  tenants  → internal/store/tenant       (4 columns)
  orgs     → internal/store/org          (6 columns, 2 relations: Parent, Children)
  users    → internal/store/user         (22 columns, 5 relations, 3 plans)
  profiles → internal/store/profile      (7 columns, 1 relation)
  posts    → internal/store/post         (10 columns, 4 relations)
  comments → internal/store/comment      (9 columns, 4 relations)
  tags     → internal/store/tag          (4 columns, 1 relation)
  roles    → internal/store/role         (3 columns)
  attachments → internal/store/attachment (11 columns, polymorphic: 3 variants, ExclusiveArc)
  audit_logs  → internal/store/auditlog   (8 columns, polymorphic: 4 variants, Discriminator ⚠ no FK)
  ✓ 12 tables · 0 unsupported types · deterministic output

$ raorm migrate diff initial_schema
  → db/migrations/0001_initial_schema.up.sql   (34 statements, non-destructive)
  → db/migrations/0001_initial_schema.down.sql
  review and commit — raorm never applies a migration
```

## What you get, per table

```go
package user   // internal/store/user — generated

Given this declaration:

```go
type User struct {
    raorm.Model                     // ID, CreatedAt, UpdatedAt
    TenantScoped                    // TenantID, Tenant
    Auditable                       // Version, UpdatedBy

    Email       string
    DisplayName string
    Status      Status                        // named string type → enum
    Prefs       Prefs                         // struct → typed jsonb
    Scopes      []string
    Age         *int16
    CreditBalance *raorm.Decimal
    LastLoginAt *time.Time
    BirthDate   *raorm.Date
    SessionTTL  *raorm.Interval
    LastIP      *netip.Addr
    AvatarThumb []byte
    SubscriptionPeriod *raorm.Range[raorm.Date]

    Org     Org                               // → OrgID in the Row
    Profile *Profile
    Posts   []Post
    Roles   []Role
}

func (u *User) Schema(t *raorm.Table) {
    t.Col(&u.Email).Unique().Size(320)
    t.Col(&u.DisplayName).Size(120)
    t.Col(&u.CreditBalance).Numeric(18, 4)
    t.Col(&u.Org).OnDelete(raorm.Restrict)
    t.Through(&u.Roles, UserRole{})
}
```

you get — the same struct with relations replaced by their scalar foreign keys
and `*T` rewritten to `raorm.Null[T]`:

```go
// Row: what a full read returns. No relation fields — see plans.
type Row struct {
    ID           raorm.UUID
    Email        string
    DisplayName  string
    Status       model.Status
    Prefs        raorm.JSON[model.Prefs]
    Scopes       []string
    Age          raorm.Null[int16]
    CreditBalance raorm.Null[raorm.Decimal]
    LastLoginAt  raorm.Null[time.Time]
    BirthDate    raorm.Null[raorm.Date]
    SessionTTL   raorm.Null[raorm.Interval]
    LastIP       raorm.Null[netip.Addr]
    AvatarThumb  []byte
    SubscriptionPeriod raorm.Null[raorm.Range[raorm.Date]]
    OrgID        raorm.UUID
    TenantID     raorm.UUID
    CreatedAt    time.Time
    UpdatedAt    time.Time
    Version      int32
    UpdatedBy    raorm.Null[raorm.UUID]
}

// Required: NOT NULL with no default. The constructor takes exactly these.
func New(email, displayName string, orgID, tenantID raorm.UUID, opt ...Option) Draft

// Options exist only for columns that are nullable or DB-defaulted.
func WithStatus(model.Status) Option
func WithPrefs(model.Prefs) Option
func WithAge(int16) Option
// … no WithID, WithCreatedAt, WithVersion — the database owns those.

// Typed columns
var (
    ID          = raorm.OrdCol[raorm.UUID]{…}
    Email       = raorm.TextCol{…}
    Status      = raorm.EnumCol[model.Status]{…}
    Prefs       = raorm.JSONCol[model.Prefs]{…}
    Scopes      = raorm.ArrayCol[string]{…}
    Age         = raorm.NullOrdCol[int16]{…}
    Search      = raorm.TSVectorCol{…}
    // …
)
```

**On mandatory fields, honestly:** Go cannot enforce "every field of a struct
literal was assigned" — `user.Row{}` compiles. The generated **positional
constructor is the only compile-time guarantee available**, so `New` takes the
required set positionally and everything else as options. Most tables have one
to four truly-required columns, which keeps that readable. Three layers back it
up: the constructor (compile time), `raorm lint` flagging zero-valued required
columns in raw literals (build time), and `NOT NULL` (run time, always).

---

# Part 3 — Fetch plans

Every load pattern in the system, in one reviewable file.

```go
// internal/store/plans.go
package store

var (
    // Light: for lists.
    UserCard = user.Plan("Card").With(user.Org)

    // Heavy: a profile page.
    UserPage = user.Plan("Page").
        With(user.Org.With(org.Parent)).
        With(user.Profile).
        With(user.Roles.Payload()).            // include user_roles columns
        With(user.Posts.
            Where(post.PublishedAt.IsNotNull()).
            OrderBy(post.PublishedAt.Desc(), post.ID.Asc()).
            Top(10).                                // 10 per user, not 10 total
            With(post.Tags).
            With(post.Comments.
                Where(comment.Parent.IsNull()).     // top-level only
                OrderBy(comment.CreatedAt.Asc(), comment.ID.Asc()).
                Top(5).                             // 5 per post
                With(comment.Author).
                With(comment.Replies.Top(3))))

    // Polymorphic: loads all three variants, batched.
    PostWithFiles = post.Plan("WithFiles").
        With(post.Attachments).
        With(post.Author)
)
```

```console
$ raorm lint --plans
  UserCard       1 round trip    users ⋈ orgs
  UserPage       7 round trips   users ⋈ orgs ⋈ orgs(parent) → profiles → user_roles ⋈ roles
                                 → posts → post_tags ⋈ tags → comments ⋈ users → comments(replies)
  PostWithFiles  2 round trips   posts ⋈ users → attachments (3 variants, 1 batch)
  ⚠ UserPage exceeds the configured limit of 5 round trips
```

That warning is the point. The plan is one object, its cost is one number, and
CI can hold a line on it.

Using a plan:

```go
u, err := user.Query().Where(user.ID.Eq(id)).Load(UserPage).One(ctx, db)

u.Org.Parent.Name                 // typed, guaranteed loaded
u.Profile.Bio                     // raorm.Null[string]
for _, r := range u.Roles {
    r.Key, r.Grant.GrantedAt      // .Grant is the join payload
}
for _, p := range u.Posts {
    for _, c := range p.Comments {
        c.Author.DisplayName
        for _, rep := range c.Replies { _ = rep }
    }
}

u.Attachments                     // compile error: not in the UserPage plan
```

---

# Part 4 — Queries

## 4.1 The one-liners

```go
u,  err := user.Get(ctx, db, id)                       // by PK
u,  err := user.GetByTenantEmail(ctx, db, tid, email)  // by the unique index
us, err := user.All(ctx, db, user.Status.Eq(model.StatusActive))
n,  err := user.Count(ctx, db, user.OrgID.Eq(orgID))
ok, err := user.Exists(ctx, db, user.Email.Eq(e))
```

## 4.2 Predicates across every type

```go
user.Query().Where(
    user.TenantID.Eq(tid),                                    // uuid
    user.Status.In(model.StatusActive, model.StatusPending),  // enum
    user.Age.Between(18, 65),                                 // nullable int → NULLs excluded
    user.Age.IsNotNull(),
    user.CreditBalance.Gte(raorm.Dec("100.0000")),            // numeric, never float
    user.Email.HasSuffix("@corp.com"),
    user.DisplayName.ILike("%ada%"),
    user.Scopes.Contains("admin", "billing"),                 // text[] @>
    user.Scopes.Overlaps("read", "write"),                    // text[] &&
    user.Prefs.Path("theme").Eq("dark"),                      // jsonb ->>
    user.Prefs.HasKey("digest"),                              // jsonb ?
    user.LastIP.InSubnet(netip.MustParsePrefix("10.0.0.0/8")),// inet <<=
    user.BirthDate.Before(raorm.DateOf(2008, 1, 1)),
    user.SubscriptionPeriod.ContainsDate(raorm.Today()),      // range @>
    user.Search.Matches("english", "ada lovelace"),           // tsvector @@
    user.CreatedAt.Gte(cutoff),
)
```

`user.Age` is `NullOrdCol[int16]`, so `.Eq(nil)` does not compile — use
`.IsNull()`. Three-valued logic is in the type, not in a comment.

## 4.3 Composition, boolean logic, and dynamic filters

```go
// Reusable specifications live in your domain layer.
var Active = raorm.And(
    user.Status.Eq(model.StatusActive),
    user.LastLoginAt.Gte(raorm.NowMinus(90*24*time.Hour)),
)

q := user.Query().Where(user.TenantID.Eq(tid), Active)

if f.Search != ""    { q = q.Where(user.Search.Matches("english", f.Search)) }
if f.OrgID != nil    { q = q.Where(user.OrgID.Eq(*f.OrgID)) }
if len(f.Scopes) > 0 { q = q.Where(user.Scopes.Contains(f.Scopes...)) }
if f.MinBalance != nil {
    q = q.Where(raorm.Or(
        user.CreditBalance.Gte(*f.MinBalance),
        user.Scopes.Contains("comp"),
    ))
}

us, err := q.OrderBy(user.CreatedAt.Desc(), user.ID.Asc()).
    Limit(f.Limit).Load(UserCard).All(ctx, db)
```

Each `.Where` sets one bit in a `uint64` and writes into an inline array. The
assembled SQL for that combination is compiled once, ever.

## 4.4 Joins, aggregation, windows, and grouping

```go
// Explicit join with a projection type — no entity round-tripping.
type OrgStat struct {
    OrgID    raorm.UUID
    OrgName  string
    Users    int64
    AvgAge   raorm.Null[float64]
    TopEmail string
}

stats, err := user.Query().
    Join(org.T, raorm.On(user.OrgID.EqCol(org.ID))).
    Where(user.TenantID.Eq(tid)).
    GroupBy(org.ID, org.Name).
    Having(raorm.Count(user.ID).Gt(5)).
    Select(raorm.Into[OrgStat](
        org.ID.As("org_id"),
        org.Name.As("org_name"),
        raorm.Count(user.ID).As("users"),
        raorm.Avg(user.Age).As("avg_age"),
        raorm.FirstValue(user.Email).
            Over(raorm.PartitionBy(org.ID).OrderBy(user.CreatedAt.Asc())).
            As("top_email"),
    )).
    OrderBy(raorm.Count(user.ID).Desc()).
    All(ctx, db)
```

`raorm.Into[OrgStat](...)` is checked at generation time: a column with no
matching field, or a type mismatch, fails the build with the offending line.

## 4.5 Pagination

```go
// Offset paging — fine for small offsets.
page, err := user.Query().Where(Active).OrderBy(user.CreatedAt.Desc()).
    Page(ctx, db, raorm.Offset(200, 50))

// Keyset paging — what you want at scale. Cursor is opaque and signed.
page, err := user.Query().Where(Active).
    OrderBy(user.CreatedAt.Desc(), user.ID.Asc()).
    Page(ctx, db, raorm.After(cursor, 50))

page.Items      // []user.Row
page.Next       // raorm.Cursor, empty at the end
page.Total      // only populated by raorm.Offset — keyset does not count
```

`raorm.After` requires the `OrderBy` to be a strict total order. It is not: a
generation error tells you to add a tiebreaker column.

## 4.6 Streaming

```go
for u, err := range user.Query().Where(user.TenantID.Eq(tid)).Iter(ctx, db) {
    if err != nil { return err }
    if err := export(u); err != nil { return err }
}
```

A Go 1.23 iterator over a server-side cursor. Nothing materialises a slice, so
memory is flat regardless of result size.

## 4.7 Recursive queries over a self-reference

The org hierarchy and the comment tree are both self-referential, so both get a
generated recursive traversal:

```go
// Every descendant org, depth-first, with the path.
subtree, err := org.Query().Where(org.ID.Eq(rootID)).
    Descend(org.Children, raorm.MaxDepth(10)).
    All(ctx, db)

for _, o := range subtree {
    fmt.Printf("%*s%s (depth %d)\n", o.Depth*2, "", o.Name, o.Depth)
}

// Ancestors, for a breadcrumb.
chain, err := org.Query().Where(org.ID.Eq(id)).Ascend(org.Parent).All(ctx, db)
```

Both lower to a single `WITH RECURSIVE`. `MaxDepth` becomes a depth predicate in
the recursive term — a cycle in the data cannot hang the query.

## 4.8 Querying polymorphic relations

```go
// Load a post's attachments (any variant) — one batched round trip.
p, err := post.Query().Where(post.ID.Eq(pid)).Load(PostWithFiles).One(ctx, db)
for _, a := range p.Attachments { _ = a.Filename }

// Query attachments directly, filtered to one variant.
imgs, err := attachment.Query().
    Where(attachment.SubjectIsPost(), attachment.MimeType.HasPrefix("image/")).
    All(ctx, db)

// Resolve subjects — exhaustive, so a new variant breaks this at compile time.
for _, a := range withSubjects {
    line := attachment.MatchSubject(a.Subject,
        func(p post.Row) string    { return "post " + p.Title },
        func(c comment.Row) string { return "comment " + c.ID.String() },
        func(u user.Row) string    { return "avatar " + u.DisplayName },
    )
    fmt.Println(line)
}
```

`SubjectIsPost()` lowers to `post_id IS NOT NULL` under ExclusiveArc and to
`subject_type = 'post'` under Discriminator. The call site does not change when
the strategy does.

---

## 4.9 First, last, and top-N *per parent*

The greatest-n-per-group problem: **the newest post for each user**, the first
order per customer, the latest comment on each post.

This is the query every ORM in [[COMPARISON]] gets wrong, and always the same
way. In GORM, Ent, and Bun, a `Limit` inside an eager load applies to the
**whole loaded set**, not per parent — so `Preload("Posts", limit 1)` across 50
users returns *one post total*, not one each. The usual workarounds are loading
everything and slicing in Go (unbounded memory, and it defeats the point) or
dropping to raw SQL and losing the typed result.

### The API

Say what you mean:

```go
var WithLatestPost = user.Plan("WithLatestPost").
    With(user.Posts.Latest(post.CreatedAt))       // newest post PER USER

var WithFirstPost = user.Plan("WithFirstPost").
    With(user.Posts.Earliest(post.CreatedAt))     // oldest post PER USER
```

**Name the plan for what it loads, not for the ordering.** `user.Plan("Latest")`
reads like "the latest user" and is a bug waiting to be misread;
`WithLatestPost` cannot be.

| | Meaning | Field type becomes |
|---|---|---|
| `.Latest(col)` | newest row per parent by `col` | `raorm.Null[post.Row]` |
| `.Earliest(col)` | oldest row per parent by `col` | `raorm.Null[post.Row]` |
| `.LatestN(n, col)` | newest *n* per parent | `[]post.Row`, len ≤ n |
| `.EarliestN(n, col)` | oldest *n* per parent | `[]post.Row`, len ≤ n |
| `.First()` / `.Last()` / `.Top(n)` | the same, for an arbitrary `OrderBy` | as above |

`Latest` and `Earliest` **append the primary key as a tiebreaker automatically**,
so two posts sharing a `created_at` still give a deterministic answer and there
is no total-order error to hit. Use `.OrderBy(...).First()` only when you need an
ordering `Latest`/`Earliest` cannot express.

### Which of these do you actually want?

Four different questions, easy to conflate:

```go
// (a) ONE user, with their newest post
u, _ := user.Query().Where(user.ID.Eq(id)).Load(WithLatestPost).One(ctx, db)
u.Posts                     // a single post.Row — not a slice

// (b) MANY users, each with their OWN newest post   ← greatest-n-per-group
us, _ := user.Query().Where(user.OrgID.Eq(orgID)).Load(WithLatestPost).All(ctx, db)
for _, u := range us { _ = u.Posts }    // 50 users → 50 posts, 2 round trips

// (c) The newest posts overall — no grouping, no plan needed
ps, _ := post.Query().OrderBy(post.CreatedAt.Desc()).Limit(10).All(ctx, db)

// (d) The newest post per author, as a flat list of posts
ps, _ := post.Query().LatestPer(post.Author, post.CreatedAt).All(ctx, db)
```

**(b) is the hard one** — it is what the rest of this section is about. **(c) is
an ordinary ordered query** and needs none of this machinery; reach for a plan
only when you are grouping.

These change the field from a slice to a **single nullable value**, so there is
no `posts[0]` to panic on when a user has never posted:

```go
u, _ := user.Query().Where(user.ID.Eq(id)).Load(UserFeed).One(ctx, db)

if p, ok := u.Posts.Get(); ok {
    fmt.Println("latest:", p.Title)
}
```

### `.Limit()` inside a relation does not compile

Because it is ambiguous, and every ORM that allows it produces the wrong answer:

```
raorm: store/plans.go:12
  Limit(10) inside With(user.Posts) is ambiguous.
  Did you mean 10 posts PER USER, or 10 posts across all users?
    · Top(10)         — 10 per user   (greatest-n-per-group)
    · LimitAcross(10) — 10 in total   (rarely what you want)
```

`LimitAcross` exists so the rare case is still reachable, and so choosing it is
visible in review.

### Ties, and why a tiebreaker is required

`OrderBy(post.PublishedAt.Desc())` alone is not a total order — two posts
published in the same microsecond make the result nondeterministic between runs.
raorm treats that as a generation error, the same rule as keyset pagination in
§4.5:

```
raorm: store/plans.go:12
  First() needs a strict total order; published_at is not unique.
  Add a tiebreaker: OrderBy(post.PublishedAt.Desc(), post.ID.Asc())
  — or use Latest(post.PublishedAt), which appends the primary key for you.
```

Want the ties instead of an arbitrary winner? `.TopWithTies(n)` lowers to
`rank()` rather than `row_number()`, and the field type stays a slice because
the count is no longer bounded by *n*.

### How it lowers

Still **one round trip for the relation** — the guarantee from ADR-0003 holds.
`internal/cost` picks between three shapes, and the choice is measured, not
assumed ([[PERFORMANCE]]):

```sql
-- DISTINCT ON — n = 1, index on (author_id, published_at DESC)
SELECT DISTINCT ON (p.author_id) p.*
FROM posts p WHERE p.author_id = ANY($1)
ORDER BY p.author_id, p.published_at DESC, p.id;

-- LATERAL — small n, index-driven, does not touch rows it will discard
SELECT p.* FROM unnest($1::uuid[]) AS k(author_id)
JOIN LATERAL (
    SELECT * FROM posts WHERE author_id = k.author_id
    ORDER BY published_at DESC, id LIMIT $2
) p ON true;

-- row_number() — larger n, or when the child scan is happening anyway
SELECT * FROM (
    SELECT p.*, row_number() OVER (PARTITION BY p.author_id
                                   ORDER BY p.published_at DESC, p.id) AS rn
    FROM posts p WHERE p.author_id = ANY($1)
) t WHERE t.rn <= $2;
```

Override the choice when you have measured something raorm has not:
`.Top(10).Using(raorm.Lateral)`.

### The index this needs, and the lint that checks for it

`LATERAL` and `DISTINCT ON` are only fast with an index on
`(parent_fk, sort_col DESC)`. Without it Postgres sorts the whole child
partition per parent, which is the slow shape this section exists to avoid. So
`raorm lint` checks for it:

```console
$ raorm lint --plans
  WithLatestPost  2 round trips   users → posts (DISTINCT ON)
  ⚠ WithLatestPost: Posts.Latest(created_at) orders by (created_at DESC, id)
    but posts has no index on (author_id, created_at DESC, id).
    → t.Index(&p.Author, raorm.Desc(&p.CreatedAt), &p.ID)
```

The suggested fix is the exact line to paste into your `Schema` method.

### Standalone, without a parent load

The same thing as a flat query — the newest post per author, no users loaded:

```go
latest, err := post.Query().
    Where(post.PublishedAt.IsNotNull()).
    FirstPer(post.Author, raorm.By(post.PublishedAt.Desc(), post.ID.Asc())).
    All(ctx, db)
```

```go
ps, err := post.Query().LatestPer(post.Author, post.CreatedAt).All(ctx, db)
```

`LatestPer` / `EarliestPer` take the group and the ordering column;
`FirstPer` / `LastPer` / `TopPer(n, …)` take an explicit ordering. All lower to
the same three shapes. Group by something other than a relation —
`post.Status`, an expression — by passing that column instead.

# Part 5 — Writes

## 5.1 Insert

```go
draft := user.New(
    "ada@corp.com", "Ada Lovelace", orgID, tenantID,   // required, positional
    user.WithStatus(model.StatusActive),
    user.WithPrefs(model.Prefs{Theme: "dark", Locale: "en-GB"}),
    user.WithScopes("read", "write"),
)
u, err := user.Insert(ctx, db, draft)   // returns the full Row via RETURNING
```

Omit `orgID` and it does not compile. Pass `WithID(...)` and there is no such
option — the database owns generated columns.

Bulk, via `COPY`:

```go
n, err := user.InsertMany(ctx, db, drafts)      // one COPY, whatever len(drafts) is
```

## 5.2 Update

```go
n, err := user.Update(ctx, db,
    raorm.Where(user.ID.Eq(id)).At(u.Version),   // optimistic lock
    user.Email.Set("new@corp.com"),
    user.Scopes.Append("billing"),
    user.Prefs.SetPath("theme", "light"),        // jsonb_set, not read-modify-write
    user.CreditBalance.Dec(raorm.Dec("9.99")),
)
if errors.Is(err, raorm.ErrStaleWrite) { /* version moved under you */ }

// user.CreatedAt.Set(...)  → compile error: declared Immutable()
```

`Append`, `SetPath`, and `Dec` are single-statement mutations. There is no
load-mutate-save cycle, so there is no lost-update window to reason about.

## 5.3 Upsert and delete

```go
err := tag.Upsert(ctx, db, draft,
    raorm.OnConflict(tag.TenantID, tag.Name).DoNothing())

n, err := comment.Delete(ctx, db, raorm.Where(comment.PostID.Eq(pid)))
```

## 5.4 Unit of work — graph writes, FK-ordered, one batch

```go
err := db.Unit(ctx, func(u *raorm.Unit) error {
    orgID  := u.Insert(org.New("Acme", tenantID))            // deferred handle
    userID := u.Insert(user.New("ada@acme.io", "Ada", orgID, tenantID))
    postID := u.Insert(post.New("Hello", "…", userID, tenantID))

    u.Insert(attachment.New("spec.pdf", "application/pdf", 8241, sum, tenantID,
        attachment.SubjectPostID(postID)))

    for _, name := range []string{"go", "orm"} {
        tagID := u.Upsert(tag.New(name, tenantID),
            raorm.OnConflict(tag.TenantID, tag.Name).Returning())
        u.Insert(posttag.New(postID, tagID))
    }

    u.Update(auditcounter.Total.Inc(1), raorm.Where(auditcounter.Key.Eq("posts")))
    return nil
})
```

`orgID`, `userID`, `postID`, and `tagID` are **deferred values**, not UUIDs —
they resolve from `RETURNING` at flush. The unit topologically sorts statements
by the foreign keys declared in the model, then emits one `pgx.Batch`. Ordering
is proven in tests with deferred constraints **off**, so it is the ordering that
is correct rather than Postgres being lenient.

On MySQL, where there is no `RETURNING`, the same code lowers to `INSERT` +
`LAST_INSERT_ID()` pairs in one batch. The call site is unchanged.

---

# Part 6 — Transactions

```go
err := db.Tx(ctx, func(tx raorm.Tx) error {
    from, err := account.Get(ctx, tx, fromID)      // tx satisfies the same
    if err != nil { return err }                   // interface as db
    if from.Balance.LessThan(amount) {
        return ErrInsufficientFunds                // any error → rollback
    }
    if _, err := account.Update(ctx, tx,
        raorm.Where(account.ID.Eq(fromID)).At(from.Version),
        account.Balance.Dec(amount)); err != nil { return err }

    _, err = account.Update(ctx, tx,
        raorm.Where(account.ID.Eq(toID)), account.Balance.Inc(amount))
    return err
})
```

Isolation, read-only hints, and automatic retry on serialization failure:

```go
err := db.Tx(ctx,
    raorm.Serializable().Retry(3, raorm.ExpBackoff(5*time.Millisecond)),
    func(tx raorm.Tx) error { … })
```

Retry fires only on `40001` (serialization failure) and `40P01` (deadlock). The
callback must therefore be side-effect-free outside the transaction, and that is
stated in its doc comment rather than assumed.

Savepoints nest naturally:

```go
db.Tx(ctx, func(tx raorm.Tx) error {
    mustSucceed(tx)
    if err := tx.Nested(ctx, func(sp raorm.Tx) error {
        return tryOptional(sp)          // rolls back to the savepoint only
    }); err != nil {
        log.Warn("optional step skipped", "err", err)
    }
    return nil
})
```

Two properties worth naming:

- **`db` and `tx` satisfy the same interface**, so every generated function
  takes either. No duplicate `XxxTx` variants, no `WithTx(...)` plumbing.
- **A `Unit` inside a `Tx` joins it** rather than opening its own. Nested
  transaction management is not something the caller has to think about.

---

# Part 7 — When the ORM is not enough

```go
var DormantHighValue = raorm.SQL[DormantRow](`
    WITH per_org AS (
        SELECT u.org_id, u.id, u.email, u.credit_balance,
               percent_rank() OVER (PARTITION BY u.org_id
                                    ORDER BY u.credit_balance DESC) AS pr
        FROM users u
        WHERE u.tenant_id = $1 AND u.status = 'active'
    )
    SELECT p.org_id, p.id, p.email, p.credit_balance, last_seen.at
    FROM per_org p
    JOIN LATERAL (
        SELECT max(c.created_at) AS at FROM comments c WHERE c.author_id = p.id
    ) last_seen ON true
    WHERE p.pr <= 0.10
      AND (last_seen.at IS NULL OR last_seen.at < $2)
    ORDER BY p.credit_balance DESC`)

rows, err := DormantHighValue.Query(ctx, db, tenantID, cutoff)
```

`PREPARE`d at generation time against a dev database (or a checked-in schema
snapshot), so the column list, types, and placeholder count are verified before
the binary exists, and `DormantRow` gets a generated scanner. A mismatch is a
build error naming the column.

Raw fragments also compose into typed queries as join sources:

```go
user.Query().
    Join(DormantHighValue.As("d"), raorm.On(user.ID.EqCol(DormantHighValue.Col.ID))).
    Where(user.TenantID.Eq(tid)).
    Load(UserCard).
    All(ctx, db)
```

---

# What this walkthrough demonstrates

| Requirement | Where |
|---|---|
| Every scalar type, incl. UUID, numeric, interval, inet, ranges, tsvector | §1.1, §1.4 |
| Typed JSONB and enums | §1.4 |
| Mandatory fields | §1.4, Part 2 — positional constructor + lint + `NOT NULL` |
| Foreign keys, cascade / restrict / set-null | §1.3–1.7 |
| One-to-one, one-to-many, many-to-many, M:N with payload | §1.5–1.7 |
| Self-referential hierarchies + recursive traversal | §1.3, §1.6, §4.7 |
| **Polymorphic, three strategies, with integrity constraints generated** | §1.8 |
| Exhaustive variant handling | §1.8 `MatchSubject` |
| Composite primary keys, partial/GIN indexes, check constraints | §1.4, §1.7 |
| Generated columns and full-text search | §1.4, §4.2 |
| Optimistic locking | §1.2, §5.2 |
| Transactions, isolation, retry, savepoints | Part 6 |
| Graph writes with FK ordering and deferred IDs | §5.4 |
| Escape hatch with full typing | Part 7 |

The two claims to keep hold of: **`u.Attachments` does not compile unless the
plan loaded it**, and **every filter combination above compiles its SQL once,
ever**.
