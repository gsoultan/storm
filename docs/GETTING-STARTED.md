---
tags: [storm, getting-started, onboarding]
updated: 2026-11-06
status: as-built — every command and output below was run against PostgreSQL 16 on this date
---

# Getting started

From an empty Go module to a running query, against PostgreSQL. It takes about
fifteen minutes, and every step below was run as written: the console output is
what storm printed, not a sketch of it.

[EXAMPLE.md](EXAMPLE.md) is the tour of what storm can do once you are set up,
and it is backed by [`examples/blog`](../examples/blog), which CI runs. This
page is the part that tour skips: installing the tool, applying the schema,
opening a connection, and the CI checks that keep everything in step.

Coming from another library? Read this page first, then
[MIGRATING-FROM-GORM.md](MIGRATING-FROM-GORM.md) or
[MIGRATING-FROM-SQLC.md](MIGRATING-FROM-SQLC.md).

## What you need

- **Go 1.27** or newer (storm's `go.mod` says `go 1.27`).
- **PostgreSQL 16 or 18**, the two versions CI runs
  ([STABILITY.md](STABILITY.md#which-server-versions)). If you have Docker,
  `make db` in this repository starts one on port 5433. Any PostgreSQL you can
  reach works.
- A connection string in `STORM_DSN`. Every storm command that needs a database
  reads it, and `-dsn` overrides it:

```console
$ export STORM_DSN='postgres://storm:storm@127.0.0.1:5433/storm'
```

## 1. Install the tool and the library

```console
$ go install github.com/gsoultan/storm/cmd/storm@latest
$ go mod init example.com/notes
$ go get github.com/gsoultan/storm/tool      # once per module
```

The second `go get` matters. `storm` finds your models by parsing your module,
then writes a small program that imports them along with `storm/tool`, runs it,
and deletes it ([ADR-0006](adr/0006-discovery-replaces-the-bootstrap.md)). That
program builds inside your module, so `storm/tool` has to be one of your
dependencies. There is no config file and nothing to register.

## 2. Write the model

One plain struct per table, with no tags. Put the structs in any package; this
page uses `model/`.

```go
// model/model.go
package model

import (
    "time"

    "github.com/gsoultan/storm"
)

// User owns notes.
type User struct {
    storm.Model // id uuid, created_at, updated_at, all with database defaults

    Email string
    Name  string

    Notes []Note // has-many; never loaded unless you ask for it
}

func (u *User) Schema(t *storm.Table) {
    t.Col(&u.Email).Size(320)
    t.Unique(&u.Email)
}

// Note belongs to a user.
type Note struct {
    storm.Model

    Title    string
    Body     string
    PinnedAt *time.Time // a pointer means nullable

    User User // a foreign key: user_id → users.id, indexed
}

func (n *Note) Schema(t *storm.Table) {
    t.Col(&n.User).OnDelete(storm.Cascade)
}
```

The Go type decides the column type, a pointer makes a column nullable, a field
of another model's type is a foreign key, and a slice of one is a has-many. The
full mapping is [REFERENCE.md §1](REFERENCE.md#1-type-mapping). Anything the
type cannot express goes in the `Schema` method. It refers to fields by
pointer, so when you rename a field, its declarations follow it.

`storm.Model` is optional. It gives a uuid primary key filled by
`gen_random_uuid()`, plus two timestamps. For an integer key, declare it
yourself:

```go
type Invoice struct {
    ID    int64
    Total storm.Decimal
}

func (i *Invoice) Schema(t *storm.Table) {
    t.PrimaryKey(&i.ID)
    t.Col(&i.ID).Serial()        // bigserial; or .Identity()
    t.Col(&i.Total).Numeric(12, 2)
}
```

Check what storm found before generating anything:

```console
$ storm models
module example.com/notes

2 model(s):
  example.com/notes/model.Note
      embeds storm.Model
      …/model/model.go:30:6
  example.com/notes/model.User
      embeds storm.Model
      …/model/model.go:11:6
```

A type counts as a model when it embeds `storm.Model`, declares a `Schema`,
`Plans` or `Projections` method, or carries a `//storm:model` comment. A type
embedded in another model is a mixin, not a table. `//storm:ignore` opts a type
out.

## 3. Generate the code

```console
$ storm generate internal/store
→ …/internal/store/note/note.gen.go (79463 bytes)
→ …/internal/store/shape.gen.go (741 bytes)
→ …/internal/store/store.gen.go (18649 bytes)
→ …/internal/store/user/user.gen.go (70001 bytes)
4 package(s) from 2 table(s)
```

You get one package per table (`user`, `note`) and a context package
(`store`) for the reads that span tables. Commit the output. It is
byte-for-byte deterministic, so it diffs cleanly in review.

Run `storm generate` again whenever a model changes, or leave
`storm watch internal/store` running while you edit. If you forget, the build
tells you. The generated code includes a shape assertion, so adding a field
without regenerating fails to compile:

```
internal/store/shape.gen.go:19:3: cannot use m.PinnedAt (variable of type *"time".Time) as bool value in struct literal
```

## 4. Create the schema

storm writes migrations. It does not apply them unless you ask it to. There
are two ways to get the tables into the database.

**The reviewed path, for anything with data you care about.** `storm diff`
compares the model with the live database and writes the difference as SQL:

```console
$ storm diff create_notes
→ db/migrations/0001_create_notes.up.sql (3 steps)
review and commit — storm diff applies nothing
```

```sql
-- generated by storm; review before applying
-- 3 step(s)

CREATE TABLE "notes" (
    "id" uuid DEFAULT gen_random_uuid() NOT NULL,
    "created_at" timestamptz DEFAULT now() NOT NULL,
    "updated_at" timestamptz DEFAULT now() NOT NULL,
    "title" text NOT NULL,
    "body" text NOT NULL,
    "pinned_at" timestamptz,
    "user_id" uuid NOT NULL,
    PRIMARY KEY ("id")
);
CREATE INDEX "ix_notes_user_id" ON "notes" ("user_id");
CREATE TABLE "users" (
    …
    CONSTRAINT "uq_users_email" UNIQUE ("email")
);
ALTER TABLE "notes" ADD CONSTRAINT "fk_notes_user_id" FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON DELETE CASCADE;
```

Apply the file with the migration tool you already use (golang-migrate, goose,
Atlas, or `psql -f`). storm writes only `.up.sql` files, with no down
migrations. Files are numbered, and a change that cannot run inside a
transaction, such as `CREATE INDEX CONCURRENTLY`, gets a file of its own.

`storm diff` refuses to write a step that can lose data until you have read it.
Here is what happens when a `bool` field is added to a table that already has
rows:

```console
$ storm diff add_archived
-- storm:destructive adding NOT NULL column archived with no default fails if the table has rows
ALTER TABLE "notes" ADD COLUMN "archived" bool NOT NULL;
storm: plan contains steps that can lose data; re-run with -allow-destructive once you have read them
```

The fix is to give the column a default (`t.Col(&n.Archived).Default("false")`),
or to pass `-allow-destructive` once you have decided the step is safe.

**The automatic path, for tests, local development and ephemeral
environments.** `migrate.AutoPool` applies the same plan from inside your
program. It takes an advisory lock so that replicas starting together migrate
once. It applies the whole plan in one transaction, and it refuses a
destructive plan unless told otherwise:

```go
s, err := storm.Build(&model.User{}, &model.Note{})
if err != nil {
    return err
}
_, err = migrate.AutoPool(ctx, pool, s, migrate.AutoOptions{Logf: log.Printf})
```

Run it a second time and it logs `storm: schema public already matches the
model` and does nothing. The trade-offs are in [API.md](API.md#automigrate).

## 5. Connect and query

```go
// cmd/notes/main.go
package main

import (
    "context"
    "errors"
    "log"
    "os"
    "time"

    "github.com/gsoultan/storm"
    "github.com/gsoultan/storm/runtime"
    "github.com/gsoultan/storm/runtime/pgxdrv"

    "example.com/notes/internal/store"
    "example.com/notes/internal/store/note"
    "example.com/notes/internal/store/user"
)

func main() {
    ctx := context.Background()

    // Use storm's constructor so its fast parameter encoders are installed.
    // Otherwise this is an ordinary *pgxpool.Pool.
    pool, err := pgxdrv.NewPool(ctx, os.Getenv("STORM_DSN"))
    if err != nil {
        log.Fatal(err)
    }
    defer pool.Close()
    ex := pgxdrv.Pool{P: pool} // the Executor that every generated call takes

    // Insert: set the columns you know. The database fills id and timestamps.
    nu := user.Create()
    nu.SetEmail("ada@example.com")
    nu.SetName("Ada")
    ada, err := nu.Insert(ctx, ex)
    if err != nil {
        log.Fatal(err)
    }

    // A constraint violation comes back as a typed error, so there is no message to parse.
    dup := user.Create()
    dup.SetEmail("ada@example.com")
    dup.SetName("Someone else")
    if _, err := dup.Insert(ctx, ex); errors.Is(err, runtime.ErrUniqueViolation) {
        log.Print("duplicate email refused")
    }

    // Read: typed predicates, an order and a limit.
    notes, err := note.New().
        Where(note.UserID.Eq(ada.ID), note.Title.HasPrefix("E")).
        Order(note.CreatedAt.Desc()).
        Limit(20).
        List(ctx, ex)

    // One row by key. Not found is ok == false, not an error.
    u, ok, err := user.New().IDEq(ada.ID).One(ctx, ex)

    // A relation: users with their notes, in two queries however many users match.
    feed, err := store.UserWithNotes().Where(user.Email.Eq("ada@example.com")).All(ctx, ex)
    log.Print(len(feed[0].Notes))

    // Update: only the columns you set are written.
    m := user.Mutate(u)
    m.SetName("Ada Lovelace")
    err = m.Update(ctx, ex)

    // A transaction: the same calls, given the transaction instead of the pool.
    err = storm.InTx(ctx, ex, func(tx storm.Executor) error {
        pin := note.Mutate(notes[0])
        pin.SetPinnedAt(time.Now())
        return pin.Update(ctx, tx)
    })
    _ = ok
}
```

Five details catch people the first time:

- **A query has a default limit of 1,000 rows.** `New()` starts with
  `LIMIT 1000`, and `List`, `All`, `Each` and `Rows` all apply it, so a table
  with 1,500 rows returns 1,000 without an error. `Count` ignores the limit.
  To read more, set `Limit` yourself, or page through the table with
  `After(lastRow)` ([API.md §4](API.md#4-reads)).
- **Rows are not your model structs.** Each package has its own `Row` type
  (`user.Row`, `note.Row`). A nullable column is a `runtime.Null[T]` with
  `.V` and `.Valid` (or `.Get()`), not a `*T`. A foreign key is its id
  (`note.Row.UserID`), not the parent struct.
- **A plain row has no relation fields.** Reading `u.Notes` on a `user.Row`
  does not compile. Load relations through a plan. storm generates
  `<Table>With<Relation>` for every relation (`store.UserWithNotes`,
  `store.NoteWithUser`). A `Plans` method declares combinations of relations
  under a name of your choosing, and that name must not clash with a generated
  one.
- **An insert writes only what you set.** An unset column gets its database
  default, never Go's zero value. `Update` writes only the columns you
  assigned, and `updated_at` changes only if you call `m.SetUpdatedAtNow()`.
- **Deleting a missing row is an error.** `user.Delete(ctx, ex, id)` returns
  `runtime.ErrNoRow` if the row is not there.

### When the builder is not enough

Write the SQL yourself, as a package-level `var`:

```go
// model/reports.go
type NoteCount struct {
    Email string
    Notes int64
}

var NotesPerUser = storm.SQL[NoteCount](`
    SELECT u.email, count(n.id) AS notes
    FROM users u LEFT JOIN notes n ON n.user_id = u.id
    GROUP BY u.email
    ORDER BY notes DESC
    LIMIT $1`)

var ArchiveOld = storm.SQLExec(`UPDATE notes SET archived = true WHERE created_at < $1`)
```

```go
counts, err := model.NotesPerUser.Query(ctx, ex, 10)   // []model.NoteCount
n, err := model.ArchiveOld.Exec(ctx, ex, cutoff)       // rows affected
```

`storm generate` creates the model in a scratch schema, PREPAREs each
statement there, and writes a scanner for it. Once raw statements exist,
`generate` needs `STORM_DSN`, though not an existing schema. A typo then fails
the build instead of a request:

```console
$ storm generate internal/store
storm: storm.SQL[NoteCount] does not prepare against the model schema:
  ERROR: column u.emial does not exist (SQLSTATE 42703)
```

At run time, only statements that the generator saw are allowed to run.
Statement text built at run time is refused before it reaches the server.
Generated code registers these statements, so import your `store` package
wherever you call them.

## 6. Put the checks in CI

```console
$ storm verify -stale internal/store     # generated code matches the model (no database)
✓ generated code matches the model (3 file(s))
$ storm verify -pending                  # the migrations carry every model change
✓ 1 migration(s) carry every model change
$ storm verify                           # this database matches the model
✓ database matches the model
$ storm lint                             # every named plan's round trips, within budget
```

`-stale` catches "changed the model, forgot to regenerate", and needs a
database only if you declare `storm.SQL`. `-pending` catches "changed the
model, forgot the migration" by replaying `db/migrations` into a scratch
schema. Plain `verify` catches drift in a real environment. Each exits
non-zero on failure. [DEPLOYMENT.md](DEPLOYMENT.md) covers what to check
before production: the binary wire format requirement, PgBouncer, pool
sizing and observability.

## Other databases

The model is the same for every engine. The engine is picked when you
generate, never at run time:

| Engine | Generate | Connect | Schema |
|---|---|---|---|
| PostgreSQL | `storm generate` | `pgxdrv.NewPool` | `storm diff`, `migrate.AutoPool` |
| MySQL 8.4 / MariaDB 11.4 | `-dialect mysql` / `-dialect mariadb` | `mydrv.NewPool` | `storm ddl -dialect …`, applied by your own tool |
| SQL Server 2022 | `-dialect mssql` | `runtime/msdrv` | `storm diff`, `migrate.AutoMSSQL` |
| Oracle (experimental) | `-dialect oracle` | `runtime/sqldrv` + your `database/sql` driver | `storm diff` |
| MongoDB (unreleased) | `-dialect mongo` | `runtime/mgdrv` | — |

What each engine does differently, and what is refused where, is in
[DIALECTS.md](DIALECTS.md). Two limits matter early on MySQL and MariaDB:
`storm.SQL` declarations are refused at generate time because there is no
server-side check for them yet, and `storm diff`, `verify`, `import` and
`explain` have no MySQL form, because each reads a catalogue that storm can
only introspect on PostgreSQL, SQL Server and Oracle.

## Where next

- [EXAMPLE.md](EXAMPLE.md): the tour, covering dynamic filters, keyset paging,
  semi-joins, projections and the unit of work.
- [API.md](API.md): every generated surface in depth, including aggregations,
  joins, upsert, row locks and soft delete.
- [REFERENCE.md](REFERENCE.md): everything the `Schema` method can declare.
- [PRODUCTION-READINESS.md](PRODUCTION-READINESS.md): what is proven, and what
  still is not.
