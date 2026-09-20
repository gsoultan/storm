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

## Re-running it

    container run -d --name storm-mssql -e ACCEPT_EULA=1 \
      -e MSSQL_SA_PASSWORD='Storm!Passw0rd' mcr.microsoft.com/azure-sql-edge:latest
    STORM_MSSQL_DSN='sqlserver://sa:Storm%21Passw0rd@<ip>:1433?encrypt=disable' \
      go test -run TestEveryConstruct -v        # in internal/mssqlspike

Full write-up: `internal/mssqlspike/README.md`; the estimate is in
`docs/PLAN.md` under the milestone table.
