# M9 driver spike — what go-sql-driver/mysql costs

`docs/PLAN.md` said M9's driver question was "a fork that exposes the binary
result rows, or an implementation of the protocol subset storm needs. That is
the estimate to make before starting, not after."

This is the measurement. Run it against a MySQL with a seeded table:

    container exec -i storm-my sh -c 'mysql -uroot -pstorm storm_m9' < seed.sql
    go test -bench . -benchtime 200x -run XXX

## Result, 8.4.11, 200 rows x 8 columns per op

| path | allocs/op | per row | B/op |
|---|---|---|---|
| `database/sql` + `Scan` | 2022 | **10.1** | 25091 |
| `driver.Rows.Next` directly | 1613 | **8.07** | 19736 |

Eight columns, 8.07 allocations per row at the floor. That is one per column per
row — exactly what ADR-0007 refuses — and bypassing `database/sql` removes only
the two per row that `sql.Rows` adds on top.

**Measure with values above 255.** The first run of this used ids 1..200 and
reported 2.07 allocs/row, because Go preallocates small integers and boxing them
is free. The table seeds ids from 1,000,000 for that reason.

## The finding that matters more than the count

`driver.Value` does not carry wire bytes. It carries DECODED values:

    col 0: int64  = 1000000
    col 1: []uint8 = [117 49 ...]
    col 2: int64  = 1090

So `runtime.Rows.RawValues() [][]byte` cannot be satisfied on top of this driver
without RE-ENCODING the int64 back to bytes, which is worse than the boxing. And
the driver has already done the decoding `runtime/mydec` exists to do, so
ADR-0007's second decoder family would be dead weight on this path — storm would
either decode twice or not at all.

That is why M9 needs the wire, not a wrapper: not because the boxing is
expensive, but because the driver's output is the wrong SHAPE for the port.

See VITESS.md for whether an existing Go library can supply the row shape
instead (short answer: no, but for a more interesting reason than cost).
