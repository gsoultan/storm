---
tags: [storm, migration, gorm]
updated: 2026-11-06
status: as-built — the schema walkthrough below was run against tables GORM v1.31.2 AutoMigrate created on PostgreSQL 16
---

# Migrating from GORM

GORM and storm differ in where the work happens. GORM reads your struct tags
with reflection, builds the SQL on every call, and assigns columns into your
structs through reflection on every row. storm reads the same structs once,
when you run `storm generate`, and writes Go code that already contains the
SQL and the decoders. Migrating is mostly moving decisions from run time to
build time. A tag becomes a line in a `Schema` method, a `Preload` becomes a
generated plan, and a string condition becomes a typed predicate.

You do not have to migrate everything at once. storm can adopt the tables
GORM created without changing them, and both libraries can run against the
same database while you move one bounded context at a time. Do one, ship it,
then do the next.

Start with [GETTING-STARTED.md](GETTING-STARTED.md) if you have not run storm
before. This guide assumes you have the tool installed.

## Is this worth it?

What you gain, with evidence:

- **Allocations.** Reading 1,000 rows costs storm 6 allocations and 41 KB.
  GORM costs 23,932 allocations and 751 KB. Reading one row by primary key
  costs 6 against 109. These are from the v1.2.0 re-measurement in
  [`bench/RESULTS.md`](../bench/RESULTS.md) (2026-09-25, Go 1.27.1). Wall
  clock is dominated by the database round trip for every library, so expect
  lower GC pressure and memory use rather than a faster single query. Re-run
  `make bench` on your own hardware before quoting any of these.
- **Mistakes found at compile time.** A misspelt column, a predicate that does
  not fit the column type (`Age.Like(...)`), and a relation read without
  loading it all fail the build. In GORM each is a runtime error or a silently
  empty field.
- **No silent schema changes.** GORM's `AutoMigrate` changes the schema as a
  side effect of starting the program. storm writes reviewable migration
  files, and its own automigrate (`migrate.Auto`) refuses any plan that can
  lose data unless you allow it.
- **Writes that cannot zero your data.** GORM's `Updates(struct)` skips zero
  values, and `Save` writes every column. In storm, an update writes exactly
  the columns you set, and a zero value is a value like any other.

What you give up:

- **A generate step.** You run `storm generate` after every model change, or
  leave `storm watch` running. A forgotten regeneration fails the build, so
  it is never silent.
- **A narrower hook surface.** Each table package has `Hooks` with
  `BeforeCreate`, `AfterCreate`, `BeforeUpdate`, `AfterUpdate`,
  `BeforeDelete`, `AfterDelete` and `AfterFind`, and `hooks.Wrap` sees every
  statement. There is no `BeforeSave`, no callback registry to reorder, and no
  hook around batches, bulk loads or set-based writes.
- **Ad-hoc query shapes.** `Select("a, b")`, `Group(...)` and `Joins(...)` at
  a call site have no equivalent. Shapes are declared on the model
  (projections, aggregations, joins), because a generated scanner needs to
  know the shape in advance. The `storm.SQL` escape hatch covers the rest.
- **Maturity.** GORM has a decade of answers on Stack Overflow. storm's API is
  stable under v1, but one team runs it in depth today. MySQL and MariaDB
  are tested on every commit and run in nobody's production yet
  ([PRODUCTION-READINESS.md](PRODUCTION-READINESS.md)).

## What maps to what

### The model

| GORM | storm |
|---|---|
| `gorm.Model` | `storm.Model` gives a uuid `id` and two timestamps with database defaults. To keep GORM's integer ids, declare `ID int64` with `t.PrimaryKey(&m.ID)` and `t.Col(&m.ID).Serial()`. `storm import` writes this for you. |
| `gorm.DeletedAt` | `DeletedAt *time.Time` plus `t.SoftDelete(&m.DeletedAt)`. See [Soft delete](#2-put-back-what-the-database-cannot-tell-storm). |
| `gorm:"primaryKey"` | `t.PrimaryKey(&a, &b)` |
| `gorm:"column:x"` | `t.Col(&f).Named("x")` |
| `gorm:"size:255"` | `t.Col(&f).Size(255)` |
| `gorm:"type:numeric(12,2)"` | `storm.Decimal` + `t.Col(&f).Numeric(12, 2)`. For other types, `.Date()`, `.Cidr()` or `.Raw(sqlType)`. |
| `gorm:"not null"` / nullable | A non-pointer field is `NOT NULL`, and a `*T` field is nullable. `.NotNull()` and `.Nullable()` override this. |
| `gorm:"default:x"` | `t.Col(&f).Default("x")`. The value is SQL, so a string literal needs quotes: `Default("'pending'")`. |
| `gorm:"unique"` / `uniqueIndex` | `t.Unique(&f)`, or `t.Index(&f).Unique()` for an index |
| `gorm:"index"` | `t.Col(&f).Index()`, or `t.Index(&a, storm.Desc(&b))` for a composite index |
| `gorm:"autoIncrement"` | `.Serial()` or `.Identity()` |
| `gorm:"-"` | Make the field unexported. storm skips unexported fields. |
| `gorm:"embedded"` | Embed the struct. An embedded struct with its own `Schema` method is a mixin ([REFERENCE.md §3](REFERENCE.md#3-mixins-are-embedded-structs)). |
| `TableName()` | `t.Name("…")` |
| `gorm:"foreignKey:…;references:…"` | A field of the parent's type is the foreign key (`Customer Customer` makes `customer_id`). For a composite key, `t.ForeignKey(...).References(...)`. |
| `gorm:"constraint:OnDelete:CASCADE"` | `t.Col(&o.Customer).OnDelete(storm.Cascade)` |
| `has many` (`Orders []Order`) | The same slice field. |
| `many2many:order_tags` | Slices on both sides generate a join table. To keep GORM's existing join table and its constraint names, model it as a struct and declare `t.Through(&o.Tags, &OrderTag{})`, as shown below. |
| polymorphic | [REFERENCE.md §6](REFERENCE.md#6-polymorphic-associations) |
| a `version` column with optimistic-lock plugins | `t.Col(&f).Version()`. A stale writer gets `runtime.ErrStaleWrite`. |

### Reads

`ex` below is a `runtime.Executor`, which is a pool or a transaction.

| GORM | storm |
|---|---|
| `db.First(&u, id)` | `u, ok, err := user.New().IDEq(id).One(ctx, ex)` |
| `errors.Is(err, gorm.ErrRecordNotFound)` | `!ok`. Not found is not an error. |
| `db.Where("email = ?", e).First(&u)` | `user.New().Where(user.Email.Eq(e)).One(ctx, ex)` |
| `db.Where("age >= ? AND name LIKE ?", …)` | `Where(user.Age.Gte(18), user.Name.Like("A%"))` |
| `db.Where(…).Or(…)` | `Any(p1, p2)`, or `AnyOf(user.And(…), user.And(…))` to OR whole groups |
| `db.Not(…)` | `Not(p)`, `NotAny(…)` |
| `db.Where("id IN ?", ids)` | `user.ID.In(ids...)` |
| `db.Find(&users)` | `user.New().List(ctx, ex)`. **This is capped at 1,000 rows by default**; see [What to watch](#what-to-watch). |
| `db.Order("created_at desc").Limit(10).Offset(20)` | `Order(user.CreatedAt.Desc()).Limit(10).Offset(20)`. Prefer `After(lastRow)` for paging. |
| `db.Model(&User{}).Count(&n)` | `n, err := user.New().Count(ctx, ex)` |
| `db.Select("name", "email").Find(&rows)` | Declare a projection, `p.Named("Card", &u.Name, &u.Email)`, then call `AllCard(ctx, ex)`. |
| `db.Pluck("email", &emails)` | A one-column projection |
| `Scopes(Active, Paginate(p))` | Ordinary functions, `func(q user.Query) user.Query`. For optional filters, use `q.WhereIf(cond, p)` or `user.WhenSet(q, f.MinAge, user.Age.Gte)`. |
| `db.Preload("Orders").Find(&cs)` | `store.CustomerWithOrders().All(ctx, ex)`. This is generated for every relation. |
| `Preload("Orders", "status = ?", "paid")` | A plan has no filter on the children. `ChildOrder(…)` orders them, `ChildTop(n)` keeps n per parent, and `ChildLimit(n)` is a guard that fails rather than truncate. For filtered children, query the child table directly: `order.New().Where(order.CustomerID.In(ids...), order.Status.Eq("paid"))`. |
| `Preload("Orders").Preload("Address")` | A named plan, `p.Named("Profile").With(&c.Orders).With(&c.Address)` |
| `db.Joins("Customer").Find(&orders)` | A declared join, `j.Named("WithCustomer").Inner(&c, &o.Customer).Take(…)` ([API.md §7](API.md#7-declared-reports-projections-aggregations-joins)) |
| `Where("EXISTS (SELECT …)")` | `store.CustomerHavingOrders(customer.New(), order.Status.Eq("paid"))`, and `NotHaving` for the anti-join |
| `db.Group(…).Select("sum(…)")` | A declared aggregation ([API.md §7](API.md#7-declared-reports-projections-aggregations-joins)) |
| `FindInBatches` / `Rows()` | `Each(ctx, ex, fn)` or `for r, err := range q.Rows(ctx, ex)`. Both stream one row at a time, and both respect the limit. |
| `db.Raw("…").Scan(&rows)` | `var Q = storm.SQL[Row]("…")` as a package-level var, then `Q.Query(ctx, ex, args...)`. It is validated at generate time. |
| `Clauses(clause.Locking{Strength: "UPDATE"})` | `ForUpdate()`, `ForUpdateSkipLocked()`, `ForShare()` and others ([API.md §8](API.md#row-locking)) |

### Writes

| GORM | storm |
|---|---|
| `db.Create(&u)` | `n := user.Create(); n.SetEmail(e); u, err := n.Insert(ctx, ex)`. Unset columns get their database default, and the row comes back with them filled. |
| `db.CreateInBatches(us, 100)` | `user.InsertAll(ctx, ex, rows)`, which uses COPY in one round trip and needs every key set. For inserts that need database defaults, queue `n.Op()` in a batch. |
| `db.Model(&u).Updates(User{Name: "x"})` | `m := user.Mutate(u); m.SetName("x"); err := m.Update(ctx, ex)`. Zero values are written when you set them. |
| `db.Model(&u).Update("name", "x")` without a loaded row | `m := user.MutateKey(id); m.SetName("x"); m.Update(ctx, ex)` |
| `db.Save(&u)` | No equivalent. Set the columns that changed. |
| `db.Model(&User{}).Where(…).Updates(…)` | `user.New().Where(…).UpdateAll(ctx, ex, m)`. An `UpdateAll` with no filter is refused with `runtime.ErrUnfilteredWrite`. |
| `db.Delete(&u, id)` | `user.Delete(ctx, ex, id)`. A missing row returns `runtime.ErrNoRow`. |
| `db.Unscoped().Delete(&u)` | `user.HardDelete(ctx, ex, id)` on a soft-delete table |
| `db.Where(…).Delete(&User{})` | `user.New().Where(…).DeleteAll(ctx, ex)` |
| `clause.OnConflict{DoNothing: true}` | `n.OnConflictEmail().DoNothing().Insert(ctx, ex)`. When the row exists, this returns `runtime.ErrConflict`. |
| `clause.OnConflict{UpdateAll: true}` | `n.OnConflictEmail().Insert(ctx, ex)`. It updates only the columns you set. |
| `FirstOrCreate` | An upsert, as above |
| `Association("Tags").Append(&t)` | Insert the join row: `ordertag.Create()` with both ids set |
| `errors.Is(err, gorm.ErrDuplicatedKey)` | `errors.Is(err, runtime.ErrUniqueViolation)`. `runtime.ErrForeignKeyViolation` and the other constraint errors work the same way, and `runtime.Retryable(err)` reports a serialization failure or a deadlock. |

### Transactions and the connection

| GORM | storm |
|---|---|
| `gorm.Open(postgres.Open(dsn))` | `pool, err := pgxdrv.NewPool(ctx, dsn)`, then `ex := pgxdrv.Pool{P: pool}` |
| `db.Transaction(func(tx *gorm.DB) error {…})` | `storm.InTx(ctx, ex, func(tx storm.Executor) error {…})`. It rolls back on an error or a panic and commits otherwise. |
| `db.Begin()` / `Commit()` / `Rollback()` | `tx, err := ex.StartTx(ctx)`, then `tx.Commit(ctx)` or `tx.Rollback(ctx)` |
| `db.WithContext(ctx)` | Every call takes `ctx` explicitly. |
| `db.AutoMigrate(&User{})` | `storm diff <name>` for reviewed migrations, or `migrate.AutoPool(ctx, pool, s, migrate.AutoOptions{})` for tests and development |
| `db.Exec("…")` | `var Purge = storm.SQLExec("…")`, then `Purge.Exec(ctx, ex, args...)` |

## The steps

The walkthrough below starts from tables that GORM's `AutoMigrate` created
from this model:

```go
type Customer struct {
    gorm.Model
    Email  string `gorm:"size:320;uniqueIndex;not null"`
    Name   string `gorm:"not null"`
    Orders []Order
}

type Order struct {
    ID         uint `gorm:"primaryKey"`
    CreatedAt  time.Time
    UpdatedAt  time.Time
    CustomerID uint   `gorm:"not null;index"`
    Customer   Customer
    Status     string `gorm:"size:20;not null;default:'pending';index"`
    Total      int64  `gorm:"not null"`
    Note       *string
    Tags       []Tag `gorm:"many2many:order_tags;"`
}

type Tag struct {
    ID   uint   `gorm:"primaryKey"`
    Name string `gorm:"uniqueIndex;not null"`
}
```

### 1. Import the schema

Do not rewrite your GORM structs by hand. Import the database instead:

```console
$ go get github.com/gsoultan/storm/tool
$ storm import -schema public > model/model.go
```

The draft it wrote for the tables above:

```go
// Customer is table customers.
type Customer struct {
    ID        int64
    CreatedAt *time.Time
    UpdatedAt *time.Time
    DeletedAt *time.Time
    Email     string
    Name      string
}

func (m *Customer) Schema(t *storm.Table) {
    t.PrimaryKey(&m.ID)
    t.Col(&m.ID).Serial()
    t.Col(&m.Email).Size(320)
    t.Index(&m.DeletedAt).Named("idx_customers_deleted_at")
    t.Index(&m.Email).Unique().Named("idx_customers_email")
}

// OrderTag is table order_tags.
type OrderTag struct {
    Order Order
    Tag   Tag
}

func (m *OrderTag) Schema(t *storm.Table) {
    t.PrimaryKey(&m.Order, &m.Tag)
    t.Col(&m.Order).ConstraintName("fk_order_tags_order")
    t.Col(&m.Tag).ConstraintName("fk_order_tags_tag")
    t.Col(&m.Tag).NoIndex()
}

// Order is table orders.
type Order struct {
    ID        int64
    CreatedAt *time.Time
    UpdatedAt *time.Time
    Customer  Customer
    Status    string
    Total     int64
    Note      *string
}

func (m *Order) Schema(t *storm.Table) {
    t.PrimaryKey(&m.ID)
    t.Col(&m.ID).Serial()
    t.Col(&m.Customer).ConstraintName("fk_customers_orders")
    t.Col(&m.Status).Size(20).Default("'pending'::character varying")
    t.Index(&m.Customer).Named("idx_orders_customer_id")
    t.Index(&m.Status).Named("idx_orders_status")
}
```

It keeps GORM's index and constraint names (`idx_customers_email`,
`fk_customers_orders`), so adopting the schema renames nothing. Then prove that
the draft describes the database exactly:

```console
$ storm verify -schema public
✓ database matches the model
$ storm diff -schema public adopt
no changes — the database already matches the model
```

Do not continue until both commands report no changes. If the draft lists
anything it could not express in its header comment, declare it in a
`Schema` method first.

### 2. Put back what the database cannot tell storm

The schema does not record GORM's behaviour, so the import cannot see it.
Three things need to be added by hand, and each one has been checked against
the tables above:

```go
type Customer struct {
    // … as imported …
    Orders []Order                     // has-many: add the slice back
}

func (m *Customer) Schema(t *storm.Table) {
    // … as imported …
    t.SoftDelete(&m.DeletedAt)                             // 1: GORM filtered these rows out of every read
    t.Index(&m.Email).Unique().Named("idx_customers_email").
        AcrossDeleted()                                    // 2: keep GORM's unique index unchanged
    t.Col(&m.CreatedAt).Default(storm.Now())               // 3: GORM set these from Go
    t.Col(&m.UpdatedAt).Default(storm.Now())
}

type Order struct {
    // … as imported …
    Tags []Tag                         // many-to-many, through GORM's join table
}

func (m *Order) Schema(t *storm.Table) {
    // … as imported …
    t.Through(&m.Tags, &OrderTag{})
}
```

1. **Soft delete.** `gorm.Model` filters out rows with `deleted_at` set on
   every query. A plain imported table does not, so without
   `t.SoftDelete(...)` your reads would include deleted rows.
2. **Unique indexes on a soft-delete table.** storm treats a unique key on a
   soft-delete table as unique among live rows only, so a deleted user's
   email can be registered again. Turning on soft delete therefore makes
   `storm diff` propose to rebuild GORM's unique index as a partial one:

   ```sql
   DROP INDEX "idx_customers_email";
   CREATE UNIQUE INDEX "idx_customers_email" ON "customers" ("email") WHERE deleted_at IS NULL;
   ```

   That may well be what you want. If you want GORM's behaviour, where every
   row holds its key forever, add `.AcrossDeleted()` and the diff is empty
   again.
3. **Timestamps.** GORM fills `created_at` and `updated_at` in Go, so the
   columns have no database default. storm's insert writes only the columns
   you set, so without a default these columns stay `NULL`. Either call
   `SetCreatedAt` and `SetUpdatedAt` on every insert, or add the defaults
   above to every table GORM created (`orders` needs them too). The
   defaults become a non-destructive migration:

   ```sql
   ALTER TABLE "customers" ALTER COLUMN "created_at" SET DEFAULT now();
   ALTER TABLE "customers" ALTER COLUMN "updated_at" SET DEFAULT now();
   ```

   On update, storm never touches `updated_at` by itself. Call
   `m.SetUpdatedAtNow()` where GORM used to do it for you.

Adding the `Orders` slice and the `Through` declaration changes no DDL, and
`storm diff` stays empty for both.

### 3. Generate and stop GORM migrating

```console
$ storm generate internal/store
6 package(s) from 4 table(s)
$ storm diff -schema public timestamp_defaults    # if you took option 3 above
```

From this point on, the storm model is the schema of record. Remove the
`AutoMigrate` call for the tables storm owns, or the two libraries will fight
over the DDL. Add `storm verify -stale` and `storm verify -pending` to CI
([GETTING-STARTED.md §6](GETTING-STARTED.md#6-put-the-checks-in-ci)).

### 4. Port reads, then relations, then writes

Port reads first, because it is safe: a storm read beside a GORM read on the
same table can be compared row for row in a test. Then port the relation
loads (`Preload` to `store.XWithY`). Port writes last.

The code below runs against the tables above:

```go
pool, err := pgxdrv.NewPool(ctx, dsn)
ex := pgxdrv.Pool{P: pool}

// Create: an integer id from the sequence, and Status from its default.
nc := customer.Create()
nc.SetEmail("ada@example.com")
nc.SetName("Ada")
ada, err := nc.Insert(ctx, ex)              // ada.ID == 1

no := order.Create()
no.SetCustomerID(ada.ID)
no.SetTotal(1000)
o, err := no.Insert(ctx, ex)                // o.Status == "pending"

// Preload("Orders"), and Preload("Tags") through order_tags.
cs, err := store.CustomerWithOrders().Where(customer.ID.Eq(ada.ID)).All(ctx, ex)
withTags, err := store.OrderWithTags().Where(order.CustomerID.Eq(ada.ID)).All(ctx, ex)

// Soft delete: Delete marks the row, and every generated read stops returning it.
err = customer.Delete(ctx, ex, ada.ID)
n, err := customer.New().Count(ctx, ex)     // the deleted row is not counted
```

Both libraries can share one database while you work through this,
because both speak to it through pgx. Moving one bounded context at a time is
the pattern the first adopter used for sqlc, and it applies here unchanged.

## What to watch

- **The default limit of 1,000 rows.** `db.Find(&all)` in GORM returns every
  row. `user.New().List(ctx, ex)` returns at most 1,000, because `New()` starts
  with `LIMIT 1000`. This applies to `All`, `List`, `Each` and `Rows`, and
  there is no error when rows are cut off. Set `Limit` explicitly wherever a
  read can be larger, or page with `After`. `Count` ignores the limit.
- **Rows are generated types, not your structs.** Each package has its own
  `Row`, a nullable column is `runtime.Null[T]` rather than `*T`, and a
  foreign key is an id field (`CustomerID`) rather than a nested struct. Map
  `Row` to your domain type at the repository boundary, which is also where
  GORM code tends to leak `*gorm.DB` today.
- **`updated_at` is not automatic.** See step 2.
- **Hooks move to the table package.** A GORM `BeforeCreate` on the model
  becomes `article.Hooks.BeforeCreate`, set at program start, and it receives
  the `Ins` (call its setters) and the transaction's `Executor`. `BeforeSave`
  is `BeforeCreate` plus `BeforeUpdate`. Hooks that set ids or timestamps
  usually become database defaults instead. Logging and tracing callbacks
  become one `hooks.Wrap` around the pool. See [API.md § 8d](API.md).
- **Preload's cost does not change, but its safety does.** GORM's `Preload`
  issues one extra query per association, and so does a storm plan. The
  difference is that reading a relation you did not load fails to compile in
  storm, instead of returning an empty slice. Per-parent limits (`ChildTop`)
  lower to `LATERAL`, which `Preload` cannot express.
- **Zero values are values.** GORM code often relies on `Updates(struct)`
  skipping zero fields. In storm, `SetActive(false)` writes `false`. Port
  these calls by setting only the fields that actually changed.
- **Integer ids are `int64`.** GORM's `uint` ids come back from the import as
  `int64`, because PostgreSQL's `bigint` is signed.
- **MySQL.** storm generates code for MySQL 8.4 and MariaDB 11.4, but
  `storm import`, `diff` and `verify` have no MySQL form, and `storm.SQL` is
  refused on MySQL at generate time. On MySQL, write the model by hand from
  the GORM structs and manage DDL with `storm ddl -dialect mysql` and your own
  migration tool. Read the MySQL section of
  [DEPLOYMENT.md](DEPLOYMENT.md#mysql-and-mariadb) first, and note that no
  production deployment runs storm on MySQL yet.
