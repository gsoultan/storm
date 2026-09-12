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

**So the four weeks is BREADTH, not risk.** The design is proven. What remains:
`caching_sha2_password` (MySQL 8.4 disables native password; the spike only
speaks native, hence testing on MariaDB), TLS, **the binary protocol —
`COM_STMT_PREPARE`/`COM_STMT_EXECUTE`, without which `runtime/mydec` does not
apply at all, and the largest single piece** — pooling, cancellation, error
mapping.

Gotcha that cost the first run: without `CLIENT_DEPRECATE_EOF` the column
definitions are followed by an EOF packet. Not consuming it means the first
"row" read IS the EOF, and the result looks empty rather than wrong.

## Still undecided

- The `Insert` API difference between dialects is unreleased and is a public
  surface question — though targeting MariaDB first would make it moot.

Related: [[m9_mysql]], [[decisions]] (ADR-0007), [[core]].
