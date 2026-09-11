# Can an existing Go library give storm the row shape it needs?

Storm's port wants `Rows.RawValues() [][]byte` — the wire bytes, zero-copy,
row at a time. Two candidates, both measured against MySQL 8.4.11, 200 rows ×
8 columns.

| library | allocs/row | B/op | row values |
|---|---|---|---|
| `go-sql-driver/mysql` via `database/sql` | 10.1 | 25091 | decoded |
| `go-sql-driver/mysql` via `driver.Rows` | **8.07** | 19736 | decoded (`int64`, `[]uint8`) |
| `vitess.io/vitess/go/mysql` `ExecuteFetch` | 9.07 | 83918 | **raw `[]byte`** |

## go-sql-driver: right count, wrong shape

`driver.Value` carries DECODED values in BOTH protocols — verified, text and
binary give `int64` for an integer column either way, so it is not an artefact
of protocol selection. `RawValues()` would need the int64 RE-ENCODED, and
`runtime/mydec` would be dead weight because the driver already decoded.

## Vitess: right shape, wrong cost — and the wrong protocol

`sqltypes.Value.Raw()` really does return `[]byte` for every column including
integers, which proves the shape is reachable. But:

- **9.07 allocs/row**, worse than go-sql-driver, because `ExecuteFetch`
  MATERIALISES the whole result into `[][]sqltypes.Value` — a slice per row plus
  the Value structs. It is built for a proxy forwarding whole results, not for
  streaming rows into a caller's arena.
- **Text protocol.** The bytes are ASCII (`raw="1000000"`), and the client API
  is `ExecuteFetch` / `ExecuteStreamFetch`. `runtime/mydec` decodes the BINARY
  format — little-endian integers, component-wise temporals (ADR-0007) — so it
  would not apply to these bytes at all.

## Conclusion

No existing Go library gives storm raw BINARY-protocol bytes streamed row at a
time into a caller's buffer. That is still the protocol-subset job the plan
named — but the spec is now exact rather than assumed, and Vitess (Apache 2.0)
is a working reference for the packet framing and auth handshake, which is the
tedious half.
