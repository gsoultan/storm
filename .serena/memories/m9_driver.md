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

## Still undecided, and worth deciding before the driver starts

- **MySQL vs MariaDB.** MariaDB has `RETURNING`, which removes the insert-shape
  divergence entirely ([[m9_mysql]]). M9's exit gate names both engines, so the
  choice changes what the protocol subset must target.
- The `Insert` API difference between dialects is unreleased and is a public
  surface question.

Related: [[m9_mysql]], [[decisions]] (ADR-0007), [[core]].
