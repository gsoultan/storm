---
tags: [storm, deployment, pgbouncer]
updated: 2026-11-27
---

# Deploying storm

Short, because storm is a library and most of this is the driver's story. The
PostgreSQL driver is [caduceus](https://github.com/gsoultan/caduceus), reached
through `runtime/pgdrv`. The part that is storm's own is the first section, and
it is the one that will bite.

## storm requires the binary wire format

Generated scanners decode the raw bytes Postgres sends. Postgres can send each
column as **binary** or as **text**, and the two are nothing alike for most
types: `false` is one zero byte in binary and the byte `'f'` in text; an
`int8` is eight big-endian bytes or a run of ASCII digits.

caduceus chooses the format per column, from the row description: binary for
every type its codec reads in binary, text for the rest. storm decodes a few
types in binary that caduceus reads as text — `numeric`, `inet`, `cidr`,
`tstzrange` and `numeric[]` — so `pgdrv.Connect` and every `pgdrv.Pool`
connection name those in caduceus's `Options.BinaryResults`, which asks for
them in binary whatever the codec would choose.

Text for the `text` column is not a problem and never was — the bytes are the
string either way. The same is true of `jsonb`, whose binary form is the same
document behind one version byte that `runtime.JSONB` already strips, and of
**enums**, whose label on the wire *is* the value you scan into a string.

So storm refuses the exact, closed set it decodes from a fixed binary layout
— `bool`, the integers and floats, `bytea`, `uuid`, the temporal types,
`numeric`, `inet`/`cidr`, `tstzrange` and the arrays — and passes everything
else through. A user-defined type you add tomorrow does not need storm to be
taught about it. Domains get no free pass either way: PostgreSQL reports the
base type's OID in the row description, so a domain over `int8` is checked as
`int8`.

**One case the guard lets through on purpose, and where it lands instead.**
An *array* of a user-defined type — `my_enum[]` — is sent as text, because
caduceus has no binary codec for one. The executor cannot tell that OID apart
from a scalar enum's, and refusing every unknown OID would refuse working
schemas, so the array reaches the decoder and fails there with
`ErrArrayTextFormat`, which names the two fixes: declare the column `text[]`,
or cast it in the query with `col::text[]`. Decoding the text format was
rejected — it means a second array parser with its own quoting rules to keep
faithful to the first, for a case with two better answers. A plain `text[]` is
unaffected: it is sent binary.

Every result is checked once per statement — before the first row — and a
column that arrived in an undecodable format is an error naming the column, not
a value. A `*caduceus.Conn` you opened yourself with `caduceus.Connect` and
wrapped in `pgdrv.Conn` gets the same check, and fails it on its first
`numeric` column; open it with `pgdrv.Connect` instead.

Before this existed, a `false` from a text-format connection decoded as
**true**, silently. If you are reading this because you hit the error, that
inversion is what it saved you from.

## MySQL and MariaDB

A different adapter, and a different set of things that bite. `runtime/mydrv`
speaks the wire protocol directly and imports nothing outside the standard
library, so an adopter who targets only PostgreSQL links none of it.

```go
pool, err := mydrv.NewPool(ctx, mydrv.Config{
    Addr:     "db.internal:3306",   // host:port, or a path for a unix socket
    User:     "app",
    Password: os.Getenv("DB_PASSWORD"),
    Database: "app",
    TLS:      mydrv.TLSRequired,    // see below
    MaxConns: 16,
})
if err != nil {
    return err
}
defer pool.Close()

// pool is a runtime.Executor. Every generated surface takes one.
rows, err := article.New().Where(article.AuthorID.Eq(id)).All(ctx, pool, nil)
```

Generate for it with `storm generate -dialect mysql` or `-dialect mariadb`, and
emit its schema with `storm ddl -dialect ...`. The two are different targets, not
one with a flag: MariaDB has `INSERT ... RETURNING` and has not got `LATERAL`,
`GROUPING()`, ordered `WITH ROLLUP` or `FOR SHARE`, and its generated column
syntax differs. Code generated for one is a syntax error on the other.

**TLS.** `TLSPreferred` is the default and is opportunistic: it upgrades where
the server offers it, falls back to plaintext where it does not, and does not
verify the certificate. It costs a passive observer the traffic and costs an
active one nothing. **Use `TLSRequired` in production** — it refuses a server
with no TLS and verifies the certificate, and a nil `TLSConfig` there means
verified, not `InsecureSkipVerify`. A private CA goes in `Config.TLSConfig`.

**First connection to a `caching_sha2_password` account.** Before the server has
cached the hash, the protocol requires an exchange that puts the password on the
wire in a recoverable form — cleartext inside TLS, or RSA-encrypted without it,
which stops an eavesdropper and does not stop anyone who can answer as the
server. On a plaintext connection mydrv REFUSES it (`ErrCleartextRefused`)
rather than proceeding. Fix it with TLS. If the socket is genuinely private —
loopback, a unix-domain proxy — set
`AllowCleartextPasswordOverPlaintext`, which is named at that length on purpose.

**Cancellation is real.** A cancelled context sends `KILL QUERY` on a second
connection, so the statement stops on the server rather than only in your
process, and the connection survives to be reused. It costs one extra
connection for the duration of the kill, which is why `MaxConns` should not be
the same as the server's `max_connections`.

**What is different from the PostgreSQL path**, and worth knowing before you
size anything:

- **Result sets stream**, and hold their connection until closed. Generated
  code closes with a `defer`; hand-written code must too, or the connection
  leaks. A second statement on the same connection before then is
  `ErrRowsOpen`, not a garbled packet — and a `Pool` is what lets you nest,
  which is why `MaxConns` should exceed the result sets one request has open
  at once.
- **`CopyFrom` is emulated** with a multi-row `INSERT`: MySQL has no `COPY`. It
  is still one round trip, but it pays statement parsing that a real copy skips.
- **`Batch` is N round trips.** MySQL's protocol has no equivalent of
  PostgreSQL's extended-query pipeline.
- **No `migrate.Auto`.** MySQL's DDL is not transactional, so the
  one-transaction guarantee automigrate is built on does not exist there. Use
  `storm ddl -dialect ...` with your own migration tool. (PostgreSQL has
  `migrate.Auto`; SQL Server has `migrate.AutoMSSQL`, whose DDL *is*
  transactional.)
- **No partial indexes**, so a soft-delete table's uniqueness spans deleted rows
  or nothing. `myddl.Check` refuses the live-scoped form rather than quietly
  widening it.

Constraint violations arrive as the same `runtime.ConstraintError` and the same
sentinels PostgreSQL produces, so a handler written for one engine works on the
other.

## PgBouncer

State the combinations plainly:

| pooling mode | works with storm | how |
|---|---|---|
| **session** | yes | nothing to do; prepared statements are per-session and stay valid |
| **transaction** | yes | set `pgdrv.Options{StatementCacheCapacity: -1}`. caduceus then keeps no statement on the server: each one is parsed into the unnamed statement in the same Sync as its Bind, so nothing outlives a Sync and PgBouncer is free to hand the next one a different server connection. Results stay binary |
| **statement** | no | it forbids multi-statement transactions, which every unit of work and `InTx` needs. Use transaction pooling |

The advice you will find elsewhere — "prefer the simple protocol behind
PgBouncer" — would make every value text, which is what storm refuses. The
unnamed-statement mode above is the answer instead.

## Pools

`pgdrv.NewPool(ctx, dsn)` opens a small pool of caduceus connections, and
`pgdrv.NewPoolConfig` takes a `pgdrv.Config`: `MaxConns` (default 16),
`AcquireTimeout` (default 30s), a `Database` and `RuntimeParams` (such as
`search_path`) that override the DSN's, and the caduceus `Options` above. A
caduceus connection belongs to one goroutine at a time, so the pool lends each
statement its own connection and takes it back when its rows are closed. A
connection goes back only if it is open, idle and outside a transaction, and
one that has sat unused for a second is checked before it is lent again.

TLS and authentication come from the DSN, as caduceus reads it; storm holds no
connection state of its own, and a transaction is just an `Executor` you were
handed.

`*pgdrv.Pool` satisfies `storm.DB` — the four port methods plus `StartTx` — so
a repository method can take `storm.DB` and run against any adapter. `StartTx`
hands back a `storm.Tx`; `pool.BeginTx(ctx, caduceus.TxOptions{…})` is the
same with an isolation level, read-only or deferrable. For state that lives on
a session, such as an advisory lock, `pool.Acquire(ctx)` lends one connection
and a function that gives it back; wrap the connection in `pgdrv.Conn`.

## Running a shard set

**Migrate every shard before any of them serves a query, and do it in a loop
you wrote.** There is no `Set.Migrate`: `migrate.AutoPool` takes a
`*pgdrv.Pool` while a `shard.Set` holds `runtime.DB`, so migration happens
while you still hold the pools you are about to hand over.

```go
for i, p := range pools {
    if _, err := migrate.AutoPool(ctx, p, model, migrate.AutoOptions{}); err != nil {
        return fmt.Errorf("migrate shard %d: %w", i, err)
    }
}
shards, err := shard.New(shard.Jump(len(pools)), dbs...)
```

**What is safe:** several instances starting at once and racing to migrate the
same shard. `Auto` takes an advisory lock per *database*, computes the plan
after taking it, and applies it in one transaction. A second pass applies
nothing.

**What is not, and is yours:** nothing makes N shards atomic with each other.
A rollout that dies after shard 1 leaves shards 2 and 3 on the old schema —
and they will keep serving reads that *succeed*, because each shard's own
constraints are all satisfied. Treat a partial migration as an outage even
though nothing is erroring: the alert has to come from your rollout, because
storm has nothing to report.

**Verifying drift is per shard.** `storm verify` takes one DSN, so run it once
per shard DSN in CI and at deploy. A shard that drifted alone is the failure
this catches, and it is invisible to a check that only looks at shard 0.

**Pool sizing is per shard, and the total is what your server sees.** Four
shards at `pgdrv`'s default of 16 connections is four times that many
connections from one process. If the shards are databases on one server —
which is how most deployments start — size them against that server's
`max_connections`, not against each other.

**Adding a shard moves rows.** `shard.Jump` is a consistent hash, so growing
from four shards to five moves about 1/5 of the keys rather than 4/5; it does
not move them *for* you. That is a data migration with a cutover, and the
window where a key's rows are on the old shard and its routing says the new
one is the window that loses writes. `shard.NewTable` exists partly for this:
moving one tenant is a map edit plus a copy, with no other tenant touched.

## Observability

Every round trip goes through the `Executor` and therefore through caduceus,
so caduceus's tracers see all of it and storm needs no tracing API of its own.

**Implement all three interfaces on one type.** caduceus splits tracing up,
and a `QueryTracer` alone is blind to batches — which is where a named plan's
relation loads travel. Wire only `QueryTracer` and you will watch the one
query you wrote while never seeing the four the plan issued, which reads
exactly like an ORM hiding work.

| storm calls | caduceus interface | method carrying the SQL |
|---|---|---|
| `Query`, `Exec` | `caduceus.QueryTracer` | `TraceQueryStart` → `data.SQL` |
| `Batch` (plans, units) | `caduceus.BatchTracer` | `TraceBatchStart` → `data.Queries[i].SQL`, every statement at once |
| `CopyFrom` (bulk load) | `caduceus.CopyFromTracer` | `TraceCopyFromStart` → `data.TableName` |

```go
type tracer struct{ /* your span factory */ }

func (t *tracer) TraceQueryStart(ctx context.Context, _ *caduceus.Conn, d caduceus.TraceQueryStartData) context.Context {
    return startSpan(ctx, d.SQL)          // one span per statement
}
func (t *tracer) TraceQueryEnd(ctx context.Context, _ *caduceus.Conn, d caduceus.TraceQueryEndData) {
    endSpan(ctx, d.Err)
}
// ... TraceBatchStart/End and TraceCopyFromStart/End likewise

pool, err := pgdrv.NewPoolConfig(ctx, pgdrv.Config{DSN: dsn, Options: pgdrv.Options{Tracer: &tracer{}}})
```

The executable version, asserting the tracer sees a query, an exec and both
statements inside a batch, is `runtime/pgdrv/tracing_test.go`.

Two storm-side signals are worth exporting as gauges next to it:

- `<table>.ShapeFlushes()` — nonzero means a call site is minting query
  structures from request data rather than from code (see `docs/PRODUCTION-READINESS.md`
  P1.1). Zero is the expected value forever.
- `runtime.CountingExecutor` wraps any executor and counts round trips. It is
  how storm asserts its own N+1 guarantees, and it works the same way in your
  tests: load a plan, assert the count is what the plan promised.

## Indexes on a live table

`storm diff -concurrently` builds and drops indexes on tables that already
exist with `CREATE INDEX CONCURRENTLY` and `DROP INDEX CONCURRENTLY`, which do
not block the table's writers while they run. On a table with traffic that is
the only build a deployment can afford: a plain `CREATE INDEX` holds a lock
that blocks every INSERT, UPDATE and DELETE for the duration.

The price is that the concurrent form **cannot run inside a transaction
block**, and a migration runner wraps each file in one — or runs a
multi-statement file as one implicit transaction, which PostgreSQL refuses the
same way. So each such statement is written to a migration file of its own,
holding exactly that one statement, marked:

```
-- storm:no-transaction
-- this statement cannot run inside a transaction block: apply this file on its own, outside one
CREATE INDEX CONCURRENTLY "ix_orders_customer_id" ON "orders" ("customer_id");
```

Tell your runner. goose reads `-- +goose NO TRANSACTION`; golang-migrate runs a
single-statement file correctly without being told; Atlas and Flyway have a
per-file setting. `storm verify -pending` replays the directory the same way —
each file as one statement, the search path set on the session rather than
prepended — so a directory that verifies is one a runner can apply.

An index on a table the same plan creates is left plain: nothing writes to a
table that does not exist yet, and it stays inside the transaction that creates
the table.

A concurrent build that fails — a duplicate under a unique index, a cancelled
statement — leaves an **invalid** index behind that PostgreSQL will not use.
Introspection reads `pg_index.indisvalid`, so `storm verify` reports it as
drift, and the next `storm diff` (or `migrate.Auto`) drops and rebuilds it —
concurrently, under `-concurrently`. Fix the cause first: a unique build that
failed on a duplicate fails again on the same duplicate.

## Constraints on a live table

`-concurrently` covers constraints too. A foreign key or a check added to a
table that already exists is written in two steps:

```
ALTER TABLE "notes" ADD CONSTRAINT "fk_notes_user_id" FOREIGN KEY ("user_id") REFERENCES "users" ("id") NOT VALID;
-- ...and in a file of its own, NNNN_<name>_validate.up.sql:
ALTER TABLE "notes" VALIDATE CONSTRAINT "fk_notes_user_id";
```

The `ADD ... NOT VALID` holds its lock for as long as it takes to write the
catalog, and is enforced for every new row from then on. The `VALIDATE` scans
the existing rows under `SHARE UPDATE EXCLUSIVE`, which blocks no reader and
no writer. In one step, the scan runs under the ADD's own lock — for a foreign
key, one that blocks every write to the table for as long as the scan takes.
The validation is in a file of its own because a transaction holds every lock
it took until it ends, so sharing the ADD's file would give the split up.

A validation that fails — an old row breaks the constraint — leaves it `NOT
VALID`: enforced for new rows, promising nothing about old ones. Introspection
reads `pg_constraint.convalidated`, so `storm verify` reports it, and the next
plan is the `VALIDATE` alone, once the rows are fixed.

## Rolling back

`storm diff -down` writes a `.down.sql` beside every `.up.sql`, under the same
version — golang-migrate's convention, and what `migrate down 1` runs. Each
down undoes exactly its own version, so rolling back one version at a time,
newest first, walks the schema back through every state it passed. A
concurrent index's down is concurrent too, alone in its file.

The down is the differ run backwards, so its marks mean what an up's do: a
down that drops a table the up created says `storm:destructive`, because the
rows written since go with it. Some steps have no reverse — PostgreSQL cannot
remove an enum label — and the down says so in a comment. A down migration is
a tool for the minutes after a bad deploy, not a substitute for a backup.

## Migration directory integrity

`storm diff` writes `storm.sum` beside the migrations: a hash of each file and,
on the first line, one over all of them. Commit it with the migrations.

- `storm verify -sum` needs no database and no model. It fails when a summed
  migration was edited, added or removed, and when two up migrations share a
  version. Run it first in CI.
- `storm diff` and `storm verify -pending` refuse a directory that disagrees
  with its sum: writing the next migration would rewrite the sum and bless the
  edit.
- `storm sum` records the directory as it stands, for an edit made on purpose
  to a migration nothing has applied yet.

The first line is what catches two branches that each ran `storm diff`. Their
files have different names — `0005_add_tags.up.sql`, `0005_add_status.up.sql`
— and merge cleanly into two version 5s, which a runner applies in whichever
order it likes or refuses outright. Both branches rewrote the sum's first line,
so the merge conflicts there, before the deploy. Resolve it by regenerating the
newer migration on top of the other branch's, then `storm sum`.

## A reproducible base

`storm diff` compares the model with the live schema at `-dsn`. On a shared
project that schema is whatever your dev database happens to hold, which is
the wrong base the moment a teammate's migration lands that you have not
applied: the next diff repeats it. `storm diff -replay` builds on the
directory instead — every `*.up.sql` replayed into a scratch namespace, the
same replay `storm verify -pending` does — so the file it writes is exactly
what the directory is missing, whatever your database looks like.
PostgreSQL and SQL Server.

## Extensions

Two features need an extension: an exclusion constraint needs `btree_gist`, and
a trigram operator class (`gin_trgm_ops`, `gist_trgm_ops`) needs `pg_trgm`. The
DDL installs each one `WITH SCHEMA public`, wrapped so that two appliers
running the same migration at once do not race each other on `IF NOT EXISTS`,
and qualifies the operator classes it installed as `public.…` so they resolve
under any search path — including the scratch namespaces `verify -pending` and
the model normalisation use.

A host that keeps extensions in a schema of its own — Supabase's `extensions`,
say — declares the class qualified, `storm.OpClass(&d.Body,
"extensions.gin_trgm_ops")`; the install storm emits is then a no-op against
the copy that already exists. Introspection compares operator classes by their
bare name, so where the class lives never shows up as drift.
