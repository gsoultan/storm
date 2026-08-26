---
tags: [raorm, deployment, pgbouncer]
updated: 2026-08-26
---

# Deploying raorm

Short, because raorm is a library and most of this is pgx's story. The part
that is raorm's own is the first section, and it is the one that will bite.

## raorm requires the binary wire format

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

So raorm refuses the exact, closed set it decodes from a fixed binary layout
— `bool`, the integers and floats, `bytea`, `uuid`, the temporal types,
`numeric`, `inet`/`cidr` and the arrays — and passes everything else through.
A user-defined type you add tomorrow does not need raorm to be taught about
it. Domains get no free pass either way: PostgreSQL reports the base type's
OID in the row description, so a domain over `int8` is checked as `int8`.

raorm enforces this in two places, so neither an application's own pool nor a
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

| pooling mode | works with raorm | how |
|---|---|---|
| **session** | yes | nothing to do; prepared statements are per-session and stay valid |
| **transaction** | yes | keep pgx's default exec mode. pgx names its prepared statements per connection and re-prepares when PgBouncer hands it a different server connection, which is what `QueryExecModeCacheStatement` and `CacheDescribe` are for |
| **statement** | no | it forbids the extended protocol entirely, so every value would be text. Use transaction pooling |

The advice you will find elsewhere — "set `simple_protocol` / prefer simple
protocol behind PgBouncer" — predates pgx v5's statement-cache handling and is
what raorm refuses. If transaction pooling misbehaves, the fix is
`QueryExecModeCacheDescribe` or `DescribeExec` (both still binary), never
simple protocol.

## Pools

`pgxdrv.NewPool` is a thin wrapper that installs raorm's fast parameter
encoders (`RegisterFastArrays`) and applies the check above. An application
that builds its own `pgxpool.Config` should call `RegisterFastArrays` from
`AfterConnect` and is otherwise free to tune everything; wrapping it in
`pgxdrv.Pool{P: pool}` still gets the per-statement guard.

Sizing, timeouts, health checks and TLS are pgx's and your platform's
business: raorm holds no connection state of its own, and a transaction is
just an `Executor` you were handed (`pgxdrv.Tx{T: tx}`).

## Observability

Every round trip goes through the `Executor`, so pgx's `QueryTracer` sees
every statement raorm issues — attach it to the pool and you have spans and
slow-query logs without raorm needing an opinion. `runtime.CountingExecutor`
wraps any executor and counts round trips, which is how the N+1 guarantees are
asserted in tests and how you can assert them in yours.
