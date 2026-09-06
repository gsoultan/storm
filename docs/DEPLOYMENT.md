---
tags: [storm, deployment, pgbouncer]
updated: 2026-08-26
---

# Deploying storm

Short, because storm is a library and most of this is pgx's story. The part
that is storm's own is the first section, and it is the one that will bite.

## storm requires the binary wire format

Generated scanners decode the raw bytes Postgres sends. Postgres can send each
column as **binary** or as **text**, and the two are nothing alike for most
types: `false` is one zero byte in binary and the byte `'f'` in text; an
`int8` is eight big-endian bytes or a run of ASCII digits.

pgx chooses the format, and its choice depends on the connection's
`DefaultQueryExecMode`. Measured, on 2026-08-25, for
`SELECT false, 42::int8, 'x'::text`:

| exec mode | formats | verdict |
|---|---|---|
| `QueryExecModeCacheStatement` (pgx default) | binary, binary, text | **supported** |
| `QueryExecModeCacheDescribe` | binary, binary, text | **supported** |
| `QueryExecModeDescribeExec` | binary, binary, text | **supported** |
| `QueryExecModeExec` | text, text, text | **refused** |
| `QueryExecModeSimpleProtocol` | text, text, text | **refused** |

Text for the `text` column is not a problem and never was — the bytes are the
string either way, which is why pgx does not bother asking for binary. The
same is true of `jsonb`, whose binary form is the same document behind one
version byte that `runtime.JSONB` already strips, and of **enums**, whose
label on the wire *is* the value you scan into a string.

So storm refuses the exact, closed set it decodes from a fixed binary layout
— `bool`, the integers and floats, `bytea`, `uuid`, the temporal types,
`numeric`, `inet`/`cidr` and the arrays — and passes everything else through.
A user-defined type you add tomorrow does not need storm to be taught about
it. Domains get no free pass either way: PostgreSQL reports the base type's
OID in the row description, so a domain over `int8` is checked as `int8`.

**One case the guard lets through on purpose, and where it lands instead.**
An *array* of a user-defined type — `my_enum[]` — is sent as text, because pgx
has no binary codec for one. The executor cannot tell that OID apart from a
scalar enum's, and refusing every unknown OID would refuse working schemas, so
the array reaches the decoder and fails there with `ErrArrayTextFormat`, which
names the two fixes: declare the column `text[]`, or cast it in the query with
`col::text[]`. Decoding the text format was rejected — it means a second array
parser with its own quoting rules to keep faithful to the first, for a case
with two better answers. A plain `text[]` is unaffected: pgx sends it binary.

storm enforces this in two places, so neither an application's own pool nor a
per-connection override can slip past:

- `pgxdrv.NewPool` / `NewPoolConfig` **refuse** a config in
  `QueryExecModeSimpleProtocol` or `QueryExecModeExec`, at construction.
- Every result is checked once per statement — before the first row — and a
  column that arrived in an undecodable format is an error naming the column,
  not a value. The check costs 3.8 ns for eight columns and allocates nothing
  (`bench/RESULTS.md`).

Before this existed, a `false` from such a connection decoded as **true**,
silently. If you are reading this because you hit the error, that inversion is
what it saved you from.

## PgBouncer

PgBouncer is why anyone sets those modes, so state the combinations plainly:

| pooling mode | works with storm | how |
|---|---|---|
| **session** | yes | nothing to do; prepared statements are per-session and stay valid |
| **transaction** | yes | keep pgx's default exec mode. pgx names its prepared statements per connection and re-prepares when PgBouncer hands it a different server connection, which is what `QueryExecModeCacheStatement` and `CacheDescribe` are for |
| **statement** | no | it forbids the extended protocol entirely, so every value would be text. Use transaction pooling |

The advice you will find elsewhere — "set `simple_protocol` / prefer simple
protocol behind PgBouncer" — predates pgx v5's statement-cache handling and is
what storm refuses. If transaction pooling misbehaves, the fix is
`QueryExecModeCacheDescribe` or `DescribeExec` (both still binary), never
simple protocol.

## Pools

`pgxdrv.NewPool` is a thin wrapper that installs storm's fast parameter
encoders (`RegisterFastArrays`) and applies the check above. An application
that builds its own `pgxpool.Config` should call `RegisterFastArrays` from
`AfterConnect` and is otherwise free to tune everything; wrapping it in
`pgxdrv.Pool{P: pool}` still gets the per-statement guard.

Sizing, timeouts, health checks and TLS are pgx's and your platform's
business: storm holds no connection state of its own, and a transaction is
just an `Executor` you were handed (`pgxdrv.Tx{T: tx}`).

## Observability

Every round trip goes through the `Executor` and therefore through pgx, so
pgx's tracers see all of it and storm needs no tracing API of its own.

**Implement all three interfaces on one type.** pgx splits tracing up, and a
`QueryTracer` alone is blind to batches — which is where a named plan's
relation loads travel. Wire only `QueryTracer` and you will watch the one
query you wrote while never seeing the four the plan issued, which reads
exactly like an ORM hiding work. (This is not a hypothetical: the test below
was written asserting `QueryTracer` saw everything, and failed.)

| storm calls | pgx interface | method carrying the SQL |
|---|---|---|
| `Query`, `Exec` | `pgx.QueryTracer` | `TraceQueryStart` → `data.SQL` |
| `Batch` (plans, units) | `pgx.BatchTracer` | `TraceBatchQuery` → `data.SQL`, once per statement |
| `CopyFrom` (bulk load) | `pgx.CopyFromTracer` | `TraceCopyFromStart` → `data.TableName` |

```go
type tracer struct{ /* your span factory */ }

func (t *tracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
    return startSpan(ctx, d.SQL)          // one span per statement
}
func (t *tracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
    endSpan(ctx, d.Err)
}
// ... TraceBatchStart/Query/End and TraceCopyFromStart/End likewise

cfg.ConnConfig.Tracer = &tracer{}          // then pgxdrv.NewPoolConfig(ctx, cfg)
```

The executable version, asserting the tracer sees a query, an exec and both
statements inside a batch, is `runtime/pgxdrv/tracing_test.go`.

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
`storm verify` reports it as drift, because introspection reads the definition
and not the validity flag; drop it and re-run the migration.

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
