# M10 (SQL Server) — the estimate, made before starting

Measured 2026-09-20, `internal/mssqlspike` (a SEPARATE module, so storm's own
`go.mod` gains no SQL Server dependency for a measurement). PLAN.md's rule from
M9 applies again: the driver decision is "the estimate to make before starting,
not after." Both halves were run against a live server, not read from docs.

## The engine has every construct the M10 gate names

`TestEveryConstructM10NeedsExists` — fifteen probes, all pass on Azure SQL Edge:
`OUTPUT` on INSERT/UPDATE/DELETE, `MERGE`, `CREATE TYPE … AS TABLE` (TVPs, the
bulk path), `OFFSET … FETCH NEXT`, CTEs, recursive CTEs (no keyword; depth is
`OPTION (MAXRECURSION n)`), window functions, `CROSS APPLY` (= LATERAL),
`JSON_VALUE`, `OPENJSON` (one bound value for a whole list — MySQL's
`JSON_TABLE` trick), `WITH (UPDLOCK, ROWLOCK)` for row locks, and **filtered
indexes**.

The filtered index is the one that matters beyond a checklist: MySQL's lack of
partial indexes is why `myddl.Check` refuses a soft-delete table's live-scoped
unique ([[m9_mysql]]). SQL Server does not share that limitation, so soft
delete's uniqueness ports here.

## The library does NOT reach the row shape — by more than MySQL's did

`microsoft/go-mssqldb` through `driver.Stmt.Query` (bypassing `database/sql`,
its most favourable path), 200 rows × 8 columns:

| path | allocs/row |
|---|---|
| `go-sql-driver/mysql` via `driver.Rows` | 8.07 |
| `microsoft/go-mssqldb` via `driver.Stmt` | **11.3** |
| storm's own `runtime/mydrv` | **1.07** |

Every column is boxed into a `driver.Value` before storm sees a byte and there
is no `RawValues` to ask for. ADR-0007's argument therefore applies with MORE
force here, not less: **M10 is a lowering AND a TDS client**, the same shape M9
turned out to be ([[m9_driver]]).

Note `*mssql.Conn` does not implement `driver.QueryerContext` — the spike goes
through `conn.Prepare` → `st.Query(nil)`.

## Estimate: 3 weeks → 5

Cheaper than M9 in two ways: the dialect seam is **proven** rather than
discovered mid-flight (`codegen` takes the dialect as a build-time parameter,
`compile/mysql` is a worked second family, so `compile/mssql` is additive), and
TDS row tokens carry fixed-width binary for integers and temporals, so the
raw-bytes-per-column contract is a property of the protocol.

Dearer in one: **no full SQL Server on arm64** — `mcr.microsoft.com/mssql/server`
refuses with `platform linux/arm64`. Azure SQL Edge runs (reports as
`15.0.2000.1574 (ARM64)`, the 2019 level) and covers every construct above, but
it is a subset engine, so the authoritative gate must run in CI against the real
image. M9's twelve defects were every one of them found by executing against a
real server on each change ([[production_readiness]] §P6.7); that loop is slower
here, and the estimate has to carry it.

## The lowering landed 2026-09-20, and it EXECUTES

`compile/mssql` and `compile/msddl`, gated by `scripts/check/mssql.sh` — the DDL
applies and every statement the lowering can produce PREPAREs and EXECUTEs. Run
BEFORE the driver exists, on purpose: the SQL is the half a borrowed client can
prove, and proving it after writing a driver against untested SQL is the order
M9 showed is expensive ([[production_readiness]] §P6.7).

### Four things the seam could not say, and a fifth it got wrong

- `runtime.Placeholder.Prefix` — `@1` is a syntax error because a T-SQL
  parameter name is an identifier. `@p1` is the name.
- `runtime.Lowering.RowCmpExpand` — no row constructor, so a keyset comparison
  expands to the OR-chain. Legal only because names are reusable.
- `runtime.Lowering.OrderFallback` — `OFFSET/FETCH` is a clause OF `ORDER BY`,
  so a capped read with no ordering is a syntax error.
- `runtime.SpliceSectionsOutput` — `OUTPUT` is POSITIONAL, between the
  assignments and the predicate. `out` goes before the LAST section, which is
  correct for UPDATE, DELETE and the empty-predicate cases alike.
- **`takesArg` tested the last byte against the set `{$, ?}`** instead of asking
  the carrier. Every SQL Server predicate came out as a bare `@` binding
  nothing. The third dialect is what found the PostgreSQL assumption two
  dialects had shared.

### What the live gate caught that a golden test could not

The fixed-text statements — top-N, recursion, a union's cap — never reach the
splicer, so nothing numbers them. compile/pgsql writes `$1` and `$2` there for
exactly this reason and the first draft left the bare sigil. Every one came back
`Must declare the scalar variable "@"`. `mssql.Param(n)` is the fix.

### Capabilities that UNDO M9 refusals

Filtered indexes (so soft delete's live-scoped unique ports), covering indexes,
`GROUPING SETS`/`CUBE`/`GROUPING()`, a server-side uuid default (`NEWID()`, so
keys are NOT client-side here), and a declared parameter reused across union
branches. The full table is in `docs/DIALECTS.md`.

One translation rather than a refusal: `UNIQUE` treats NULLs as EQUAL here, so a
nullable unique becomes a FILTERED index — `WHERE col IS NOT NULL` indexes
exactly the rows PostgreSQL's constrained. `NULLS NOT DISTINCT` is therefore
this server's plain form, and the one MySQL refuses.

### The client landed too, and it beat the library by 125x

`runtime/msdrv` — a TDS client, stdlib only — reads 200 rows of 8 columns in
**18 allocations for the whole result**. 0.09 per row, against
microsoft/go-mssqldb's 11.3 and `runtime/mydrv`'s 1.07.

What it speaks: PRELOGIN with the TLS handshake INSIDE TDS packets; the
protocol's default of encrypting the LOGIN PACKET ONLY (a client without that
cannot log in to a stock SQL Server at all); LOGIN7; sp_executesql for named
parameters and plain SQL batches for statements with none; ATTENTION for
cancellation; the whole token stream. Batch is ONE round trip — an RPC request
may carry several calls separated by a batch flag, which the MySQL wire cannot
do. CopyFrom is emulated with batched multi-row INSERTs and says so.

Four wire defects, each worth a debugging session:

- `fill()` read the "last packet" flag without checking whether a MESSAGE was in
  progress. The flag is left set by the previous reply, so the first read of
  every new one reported the end of a message that had already finished — the
  login "succeeded" without reading the server's answer.
- LOGIN7's option flags are bit MASKS, and the low bits of the first byte
  declare byte order and character set. Setting them by ordinal announces a
  big-endian EBCDIC client and the server hangs up with no message.
- Every request after a BEGIN must echo the transaction DESCRIPTOR from an
  ENVCHANGE. Zero works until the first transaction and then fails everything
  inside it.
- A sub-slice of the packet buffer is valid only until the next PACKET, not the
  next row. A row with a wide column came back as fragments of its own tail.
  Values are copied out and the slices rebuilt AFTER the row completes.

And the batch needed a TRAILING batch flag after the last call, not just
between calls — without it the read blocks on a reply that is never sent.

### The codegen defect only a live run could find

`count(*)` returns an INT on this server — four bytes, where a Count() of type
int64 decodes eight — so **every count came back as zero with no error**.
`count_big` is the fix. Fourth time P6.7's rule has been literally true.

### The text hook

The one assignment in a generated scanner that is not a decoder CALL is
`sl.Str(rv[i])`, so a family whose strings are UTF-16 cannot be served by
renaming a function. `decoders.text` is the hook. Without it every string in the
database reads back as its own interleaved-null bytes: no error, no failure,
mojibake.

### The last two pieces

**CopyFrom is the real bulk path** — `INSERT BULK`, a packet of type 7 carrying
COLMETADATA and one ROW token per row, one reply. The column types are READ FROM
THE SERVER (`SELECT TOP 0`, cached per connection and column list), because a
bulk row carries no parameter declaration and therefore no conversion step: a
value written in the wrong width is read as the next column's bytes. Four
defects, every one reported by the server against the WRONG column:

- The COLMETADATA TOKEN BYTE was missing — the count was read as a token id.
- A fixed BIT written with a length byte → "the next unicode column has an odd
  byte size".
- A decimal written at the widest form rather than the COLUMN's declared width.
- A MAX value using the KNOWN-length PLP header, after which the terminator was
  read as the next row's token. The unknown-length header is the one to use in a
  bulk stream.

**The upsert is a MERGE**, with two non-optional parts: `WITH (HOLDLOCK)`
(without it two concurrent merges of one key both insert and one fails — works
in every test, breaks under load) and the terminating semicolon (whose absence
is reported against the NEXT statement). Every parameter is numbered once in the
source row and referred to by name after, which is what lets both branches read
the same row.

The UNTARGETED `DoNothing()` is refused by name (`ErrUpsertNeedsTarget`): a bare
`ON CONFLICT DO NOTHING` fires on any unique index, a MERGE's match condition
names columns, and watching the primary key instead would be a lie at the call
site.

### CI: SQL Server has a job of its own

Four databases on the shared runner made `runtime/mydrv`'s soak test flaky —
cancellation there is a real `KILL QUERY`, which needs a SECOND connection,
whose login can exceed its deadline on a starved runner. Not a bug; a scheduling
fact. See P8 in `docs/PRODUCTION-READINESS.md`. The floors for `runtime/msdrv`
and `runtime/msdec` live in `scripts/check/mssql.sh` for the same reason: in the
other job they would measure a suite that skipped.

### M10 is done

Lowering, DDL, TDS client, decoder family, codegen, upsert and bulk load — all
executing against a real server. Gated by:
`scripts/check/mssql.sh` (39 statements), `codegen/mssqllive_test.go` (a
generated package, five tests, refusing to pass if any SKIPPED), coverage floors
for both runtime packages, and CI against the real
`mcr.microsoft.com/mssql/server`.

## Re-running it

    container run -d --name storm-mssql -e ACCEPT_EULA=1 \
      -e MSSQL_SA_PASSWORD='Storm!Passw0rd' mcr.microsoft.com/azure-sql-edge:latest
    STORM_MSSQL_DSN='sqlserver://sa:Storm%21Passw0rd@<ip>:1433?encrypt=disable' \
      go test -run TestEveryConstruct -v        # in internal/mssqlspike

Full write-up: `internal/mssqlspike/README.md`; the estimate is in
`docs/PLAN.md` under the milestone table.
