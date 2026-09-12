# storm — M9's driver question, measured 2026-09-11

`docs/PLAN.md` said: "a fork that exposes the binary result rows, or an
implementation of the protocol subset storm needs. That is the estimate to make
before starting, not after." This is the estimate.

**Answer: the protocol subset. The wrapper cannot satisfy the port at all.**

## The numbers

`internal/mysqlspike` — a SEPARATE module, so storm's `go.mod` gains no MySQL
dependency for a measurement. MySQL 8.4.11, 200 rows × 8 columns:

| path | allocs per row |
|---|---|
| `database/sql` + `Scan` | 10.1 |
| `driver.Rows.Next` directly | **8.07** |

One allocation per column per row at the floor, exactly what ADR-0007 refuses.
Bypassing `database/sql` removes only the two per row `sql.Rows` adds.

**Seed ids above 255.** The first run used 1..200 and reported 2.07 allocs/row —
it was measuring Go's preallocated small integers, not the driver. Same trap as
the three fixture bugs in the MySQL gates: a measurement that passes for the
wrong reason. **When benchmarking boxing, the values must leave the small-int
cache.**

## The finding that matters more than the count

`driver.Value` carries DECODED values, not wire bytes:

    col 0: int64   = 1000000
    col 1: []uint8 = [117 49 ...]

So `runtime.Rows.RawValues() [][]byte` cannot be satisfied without RE-ENCODING
the int64 — worse than the boxing. And the driver has already done the decoding
`runtime/mydec` exists to do, so ADR-0007's second decoder family would be dead
weight: storm would decode twice or not at all.

**The fork is not an optimisation over the wrapper. The wrapper is the wrong
SHAPE for the port.** That is a stronger argument than the allocation count and
it is the one to lead with.

## "Why not a different driver for MariaDB?" — researched 2026-09-11

**Because the WIRE is the same.** MariaDB speaks the MySQL wire protocol; any
MySQL client connects to either. A second driver would reimplement an identical
thing. What diverges is SQL — JSON functions, `RETURNING`, auth defaults, GTID —
and that is the DIALECT layer storm already has.

So the split is **one driver, two dialects**, not two drivers:

- wire: one protocol implementation, shared.
- SQL: `compile/mysql` and `compile/mariadb`.

**MariaDB is NOT "MySQL plus RETURNING" — that was my guess and it was wrong.**
Measured on 11.4.13 (built 2026-09-12): it gains `INSERT … RETURNING` and loses
FOUR things — `LATERAL`, `GROUPING()`, `WITH ROLLUP` combined with `ORDER BY`
(Error 1221), and `FOR SHARE` (it wants `LOCK IN SHARE MODE`). So it is
DIFFERENT, not cheaper. The `RETURNING` win is real though: `storm.Model`
generates for MariaDB and the insert returns the row, which MySQL 8 cannot.

Consequence worth remembering: **no LATERAL means the batch loader uses the
WINDOW form on MariaDB**, whose cost tracks the total child count rather than
the rows returned. A relation load costs more there, structurally.

One wire caveat to plan for: MySQL 8.4 turns `mysql_native_password` off by
default while most MariaDB installs still use it, so the handshake must carry
both `caching_sha2_password` and `mysql_native_password`.

## Can an existing library supply the row shape? No — see VITESS.md

`vitess.io/vitess/go/mysql` gives `sqltypes.Value.Raw() []byte` for EVERY column
including integers, which proves the shape is reachable. It is still not usable:
**9.07 allocs/row** (worse than go-sql-driver, because `ExecuteFetch`
materialises the whole result) and **text protocol only** on the client side, so
`mydec`'s binary decoders would not apply.

No Go library streams raw BINARY-protocol bytes row-at-a-time into a caller's
buffer. Vitess (Apache 2.0) is a working reference for packet framing and the
auth handshake — the tedious half — but not a dependency that solves it.

## A wire client DOES reach the target — 1.07 allocs/row (2026-09-12)

`internal/mysqlspike/wire`, MariaDB 11.4.13, 200 rows × 8 columns:

| path | allocs/row | B/row |
|---|---|---|
| `go-sql-driver` via `driver.Rows` | 8.07 | 99 |
| vitess `ExecuteFetch` | 9.07 | 420 |
| **minimal wire client** | **1.07** | **5** |

Reuse the packet buffer, point the column slices into it. Same validity contract
as pgx's `RawValues` (good until the next row), which is why storm has Slabs.

### The binary protocol works, and mydec decodes real bytes (2026-09-12)

`COM_STMT_PREPARE`/`EXECUTE` and binary rows are implemented in the spike.
**ADR-0007's second decoder family had never been handed bytes off a wire.** It
has now, and it is correct: max int64, min int32/int16, bool, UTF-8,
`DECIMAL(18,4)`, `DATETIME(6)` with microseconds, `DATE`.

| 200 rows | allocs/row |
|---|---|
| binary rows, raw | 1.07 |
| binary rows + mydec decoding | **1.04** |

**Decoding is FREE.** The scanners read out of the raw bytes without allocating
— the seam's whole design, measured end to end for the first time.

**The length prefix is ASYMMETRIC and a driver must encode that.** A string or
decimal wants the PAYLOAD; a temporal wants the LENGTH PREFIX KEPT, because
`mydec.DateTime` reads `b[0]` as the component count and switches on 0/4/7/11.
Strip it for both and every other column looks fine while temporals fail with
"MySQL binary value has the wrong length". Cost the first run of the test.

**So the four weeks is BREADTH, not risk.** What remains: `caching_sha2_password`
(MySQL 8.4 disables native password; the spike speaks only native, hence testing
on MariaDB), TLS, the full parameter type table, pooling, cancellation, error
mapping, and the `runtime.Executor` adapter itself.

Gotcha that cost the first run: without `CLIENT_DEPRECATE_EOF` the column
definitions are followed by an EOF packet. Not consuming it means the first
"row" read IS the EOF, and the result looks empty rather than wrong.

## Still undecided

- The `Insert` API difference between dialects is unreleased and is a public
  surface question — though targeting MariaDB first would make it moot.

Related: [[m9_mysql]], [[decisions]] (ADR-0007), [[core]].

---

# The adapter is shippable — 2026-09-12

The four gaps `runtime/mydrv`'s package doc named are closed. Each is proved by
a test that fails when the feature is removed (probed, not assumed).

## What landed

- **TLS.** `Config.TLS`: `TLSPreferred` (upgrade where offered, do NOT verify),
  `TLSRequired` (refuse a server without it, and verify — a nil `TLSConfig`
  there is not `InsecureSkipVerify`), `TLSDisabled`. The default does not verify
  ON PURPOSE: stock MySQL and MariaDB both ship a self-signed certificate, so a
  verifying default cannot connect to either and pushes callers to
  `TLSDisabled`, which is strictly less. Same model as go-sql-driver's
  `tls=preferred`.
- **`caching_sha2_password`** with full auth, `mysql_native_password`, and the
  auth-switch request. Full auth on a plaintext socket is `ErrCleartextRefused`
  unless `AllowCleartextPasswordOverPlaintext`.
- **`mydrv.Pool`** (bounded, an `Executor`) and **`mydrv.Tx`** (pinned to one
  connection; `BEGIN` is session state).
- **`KILL QUERY` from a second connection** on cancel. Asserted against
  `information_schema.processlist`.

## The three findings worth remembering

1. **The RSA branch of full auth is OAEP with SHA-1, not SHA-256.** The digest
   is OAEP's mask function; it has nothing to do with the plugin's name. Getting
   it wrong fails as "Access denied", indistinguishable from a wrong password.
   Only a test that CREATEs a fresh account (so the server has no cached hash)
   reaches this branch at all — every ordinary test takes the fast path.

2. **The shipped adapter cost 10.1 allocs/row while the docs claimed 1.07.**
   The 1.07 was the spike's; the shipped `Query` MATERIALISED every result set,
   which is nine allocations a row on top. Materialising had a real reason —
   a result set holds the connection, and a plan loading a relation mid-iteration
   would deadlock — that **the pool made obsolete and nobody revisited**. Now it
   streams: 1.07 allocs/row, 6 B/row, and
   `TestQueryCostsAboutOneAllocationPerRow` is the gate. The lesson generalises:
   when a constraint is removed, go back and find what was built to work around
   it.

3. **A `FLOAT` column panicked the row decoder.** `fixedWidth` had no entry, so
   four bytes were read as length-encoded and every subsequent column in the row
   decoded from the wrong offset. Neither side's unit tests could see it —
   `mydec` hand-writes the bytes it expects and the binder's tests check what it
   produced, so a contract mismatch passes both. **Only a real server, sitting
   in the middle, can tell two halves of a contract apart.** The fix ships with
   a round trip of every supported type against both engines.

## Streaming's contract

A result set holds its connection until `Close`. A second statement on the same
connection before then is `ErrRowsOpen` — a NAMED error, because the first
version let it corrupt the protocol and panic several statements later, with
nothing to connect the crash to the missing `Close`. `Close` drains first. A
`Pool` is what lets you nest, which is what a fetch plan does.

## Errors

`mydrv.classify` maps the server's codes onto `runtime.ConstraintError` and the
same sentinels `pgxdrv` uses. Nothing is invented: MySQL has no exclusion
constraint and reports a serialization conflict AS a deadlock, so those two
sentinels stay unmapped. The constraint's NAME is dug out of the message
(best-effort, documented) because the server does not send it as a field — what
is matched is an identifier quoted back from the DDL, not translated prose.
The two servers use different codes for a failed CHECK (3819 / 4025) and
different quoting, so the test runs against both.

## Reachability

`storm generate -dialect mysql|mariadb`, `storm ddl -dialect ...`. Before this,
every engine-specific piece existed and none could be asked for from the CLI.
Commands that read a live PostgreSQL catalogue (`diff`, `verify`, `explain`,
`import`, `watch`) refuse a non-PostgreSQL dialect and say what to do instead.
Raw `storm.SQL` declarations refuse it too: they are validated by PREPAREing
against PostgreSQL, and there is no MySQL equivalent.

## Local servers

`storm-my` (MySQL 8, 192.168.64.3:3306) and `storm-maria` (MariaDB 11.4,
192.168.64.188:3306), both root/storm, database `storm`. Tests read
`STORM_MYSQL_ADDR` and `STORM_MARIADB_ADDR`; CI has both services and the
MariaDB dialect gate gained a `STORM_MARIADB_DSN` path.
