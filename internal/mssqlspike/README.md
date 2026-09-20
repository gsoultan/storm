# M10 driver spike — what go-mssqldb costs, and what SQL Server has

`docs/PLAN.md` set the rule at M9 and it applies again: the driver question is
"a fork that exposes the result rows, or an implementation of the protocol
subset storm needs — that is the estimate to make before starting, not after."

M9's answer was the protocol subset, and it was right: the hand-written client
reached 1.07 allocations per row where the best library path cost 8.07. This is
the same measurement for SQL Server, plus the half M9 did not need — whether
the engine has the constructs the milestone's exit gate names at all.

A SEPARATE module, so storm's own `go.mod` gains no SQL Server dependency for a
measurement.

## Running it

Full SQL Server is amd64 only. On Apple silicon the only engine with an arm64
image is Azure SQL Edge, which reports as 15.0 (the SQL Server 2019 level):

    container run -d --name storm-mssql -e ACCEPT_EULA=1 \
      -e MSSQL_SA_PASSWORD='Storm!Passw0rd' mcr.microsoft.com/azure-sql-edge:latest

    STORM_MSSQL_DSN='sqlserver://sa:Storm%21Passw0rd@<ip>:1433?encrypt=disable' \
      go test -run TestEveryConstruct -v
    STORM_MSSQL_DSN=... go test -run XXX -bench . -benchtime 30x

## Result 1: the engine has everything M10's gate names

Every construct, on Edge, on arm64 — `TestEveryConstructM10NeedsExists`:

| M10 needs | Have | Notes |
|---|---|---|
| `INSERT/UPDATE/DELETE … OUTPUT` | yes | the returning clause, on all three verbs |
| `MERGE` | yes | |
| Table-valued parameters | yes | `CREATE TYPE … AS TABLE` — the bulk path |
| `OFFSET … FETCH NEXT` | yes | the paging gate |
| CTEs, recursive CTEs | yes | recursive needs no keyword; depth is `OPTION (MAXRECURSION n)` |
| Window functions | yes | |
| `CROSS APPLY` | yes | this is LATERAL |
| `OPENJSON` | yes | one bound value for a whole list — MySQL's `JSON_TABLE` trick |
| **Filtered indexes** | yes | `CREATE INDEX … WHERE` — so a partial UNIQUE **does** port here, which it does not to MySQL, and soft delete's live-scoped uniqueness comes back |
| Row locks | yes | `WITH (UPDLOCK, ROWLOCK)`, a table hint rather than a suffix |

That was fifteen guesses from documentation before it ran. The one that matters
most is the filtered index: MySQL's lack of it is why `myddl.Check` refuses a
soft-delete table's live-scoped unique, and SQL Server does not share the
limitation.

## Result 2: the library costs MORE than MySQL's did

`microsoft/go-mssqldb` through `driver.Stmt.Query` — bypassing `database/sql`,
the most favourable path the library offers. 200 rows × 8 columns:

| path | allocs/row | B/row |
|---|---|---|
| `go-sql-driver/mysql` via `driver.Rows` | 8.07 | 99 |
| `vitess.io/vitess/go/mysql` | 9.07 | 420 |
| **`microsoft/go-mssqldb` via `driver.Stmt`** | **11.3** | **350** |
| storm's own `runtime/mydrv` | **1.07** | **6** |

Eleven allocations per row against storm's one. ADR-0007's argument applies
with more force here than it did to MySQL, not less: the library boxes every
column into a `driver.Value` before storm can see a byte, and there is no
`RawValues` to ask for.

## What that means for M10's estimate

**M10 is a lowering AND a TDS client**, the same shape M9 turned out to be —
and the protocol is the harder half. TDS is a token-stream protocol with a
login sequence, collation negotiation and optional encryption, where MySQL's is
length-prefixed packets with a two-phase auth. M9's six days are a floor, not a
comparison.

Two things make it cheaper than M9 was:

- **The seam is proven.** M9 discovered mid-flight that the query side had ONE
  implementation serving both dialects. It has three now, `codegen` takes the
  dialect as a build-time parameter, and `compile/mysql` is a worked example of
  a second family. `compile/mssql` is additive.
- **The row shape is reachable.** TDS row tokens carry fixed-width binary for
  the integer and temporal types, exactly as MySQL's binary protocol does, so
  the raw-bytes-per-column contract `runtime.Rows` wants is a property of the
  protocol rather than something a client has to reconstruct.

And one that makes it dearer: **no local full SQL Server on arm64.** Edge covers
every construct above, but it is a subset engine at the 2019 level, so the
authoritative gate has to run in CI against `mcr.microsoft.com/mssql/server`.
M9's twelve defects were all found by running against a real server on every
change; that loop is slower here.
