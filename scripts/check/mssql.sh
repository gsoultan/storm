#!/usr/bin/env bash
# Does the SQL storm emits for SQL Server actually run on SQL Server?
#
# The M9 rule, applied to M10 before M10 has a driver: docs/PRODUCTION-READINESS
# §P6.7 says "a test that does not EXECUTE against the target proves the
# generator is consistent with itself and nothing more". Twelve defects came out
# of running the MySQL lowering for the first time. The SQL is the half that can
# be proven with a borrowed client, so it is proven FIRST — before runtime/msdrv
# exists, rather than after.
#
# Runs in internal/mssqlspike, which is a separate module: storm's own go.mod
# gains no SQL Server dependency for a gate, exactly as it gains none for the
# MySQL one. The borrowed client is microsoft/go-mssqldb, used here only to
# carry text to a server — what is under test is compile/mssql and
# compile/msddl.
#
# Skipped unless STORM_MSSQL_DSN names a server. On Apple silicon that is Azure
# SQL Edge, the only SQL Server engine with an arm64 image; CI runs the real
# mcr.microsoft.com/mssql/server, which is where the authoritative result is.
set -uo pipefail
cd "$(dirname "$0")/../.."

if [ -z "${STORM_MSSQL_DSN:-}" ]; then
  echo "STORM_MSSQL_DSN unset; skipping the SQL Server check"
  exit 0
fi

fail=0
note() { printf '  %s\n' "$1"; fail=1; }

echo "== the DDL storm emits for SQL Server APPLIES =="
echo "== every statement compile/mssql lowers PREPAREs and EXECUTEs =="
echo "== the constructs M10 needs all exist on this engine =="
if ! (cd internal/mssqlspike && go test ./... 2>&1 | sed 's/^/    /'); then
  note "SQL Server refused something storm emitted — see the statement above"
fi

# A refusal has to NAME what it refuses. The three JSON containment operators
# have no SQL Server form at all, and the failure mode a gate exists to catch is
# not "refused" but "silently dropped": a predicate that disappears returns
# every row.
echo "== a refusal names the construct, not just the dialect =="
if ! (cd internal/mssqlspike && go test -run 'TestDeclaredReads/refusals' ./... >/dev/null 2>&1); then
  note "a construct with no SQL Server lowering did not say so by name"
fi

if [ "$fail" -eq 0 ]; then
  echo "OK: storm's SQL Server DDL applies, and every statement it lowers runs"
else
  echo "FAILED"
fi
exit "$fail"
