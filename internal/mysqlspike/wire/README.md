# Is storm's port reachable over the wire? Yes — 1.07 allocs/row.

**This is not a driver.** No TLS, no `caching_sha2_password`, no prepared
statements, no pooling, no cancellation, no error mapping. Those are the four
weeks `docs/PLAN.md` estimates. This is the RISK inside them, measured, so the
estimate is breadth rather than uncertainty.

## The question

storm's `runtime.Rows` wants `RawValues() [][]byte` — wire bytes, zero-copy, one
row at a time. `internal/mysqlspike/VITESS.md` shows no existing Go library
supplies it. Could a wire client, and at what cost?

## The answer

MariaDB 11.4.13, 200 rows × 8 columns:

| path | allocs/row | B/row |
|---|---|---|
| `go-sql-driver` via `driver.Rows` | 8.07 | 99 |
| `vitess.io/vitess/go/mysql` | 9.07 | 420 |
| **this** | **1.07** | **5** |

Eight times fewer allocations, nineteen times fewer bytes. The residual ~1 per
row is one allocation somewhere in the read path and is findable; it does not
change the conclusion, which is that **the port is satisfiable over the wire and
is not satisfiable on top of a decoding driver.**

## How

`readPacket` reads into a REUSED buffer and the column slices point into it, so
a row costs no allocation to expose. The slices are valid until the next row —
the same contract pgx's `RawValues` has, and the reason storm has Slabs.

## The binary protocol works, and so does mydec

`COM_STMT_PREPARE` / `COM_STMT_EXECUTE` and binary result rows are implemented,
which makes `runtime/mydec` mean something: ADR-0007's second decoder family had
never been handed bytes off a wire. It is now, and it decodes them correctly —
max int64, min int32, min int16, bool, UTF-8 text, `DECIMAL(18,4)`, `DATETIME(6)`
with microseconds, and `DATE`.

| path (200 rows) | allocs/row | B/row |
|---|---|---|
| binary rows, raw | 1.07 | 4.3 |
| binary rows **+ mydec decoding** | **1.04** | **4.2** |

**Decoding is free.** The scanners read out of the raw bytes without allocating,
which is the design the whole seam exists for, measured end to end for the first
time.

### The contract detail a driver must get right

The length prefix is **asymmetric**, and the first run of this got it wrong:

- a **string or decimal** wants the PAYLOAD — `mydec.Text` and `mydec.Decimal`
  read the bytes themselves;
- a **temporal** wants the LENGTH PREFIX KEPT — `mydec.DateTime` reads `b[0]` as
  the component count and switches on 0/4/7/11, because MySQL packs a datetime
  component-wise and the length is how you know whether the microseconds are
  there.

Strip it for both and every other column looks fine while the temporals fail
with "MySQL binary value has the wrong length".

## What is missing, and why each matters

- **`caching_sha2_password`.** MySQL 8.4 turns `mysql_native_password` off by
  default; this speaks only the latter, which is why it is tested against
  MariaDB. A driver needs both, and the sha2 exchange needs TLS or the server's
  public key.
- **TLS.** Not optional for anything real.
- **The full parameter type table.** `COM_STMT_EXECUTE` here binds `int64` and
  `string` only; a driver needs every type storm can pass.
- **`CLIENT_DEPRECATE_EOF`.** Not set here, which is why the column definitions
  are followed by an EOF packet the reader has to consume — the bug that made
  the first run return zero rows.
- **Pooling, context cancellation, error mapping, multi-result sets, LOAD DATA.**

## Running it

    go test -run TestWireClientReadsRows -v
    go test -bench BenchmarkWireRawRows -benchtime 200x -run XXX

Points at `192.168.64.188:3306` (the `storm-maria` container).
