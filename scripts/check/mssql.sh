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
  # Skipping is right on a laptop and wrong in CI. A typo in the workflow's env
  # would otherwise turn this gate into an unconditional pass, which is the
  # same failure the statement count below guards against, one level up.
  if [ -n "${CI:-}" ]; then
    echo "STORM_MSSQL_DSN unset in CI: this gate cannot pass by not running" >&2
    exit 1
  fi
  echo "STORM_MSSQL_DSN unset; skipping the SQL Server check"
  exit 0
fi

fail=0
note() { printf '  %s\n' "$1"; fail=1; }

echo "== the DDL storm emits for SQL Server APPLIES =="
echo "== every statement compile/mssql lowers PREPAREs and EXECUTEs =="
echo "== the constructs M10 needs all exist on this engine =="
out=$(mktemp)
if ! (cd internal/mssqlspike && go test -v ./... >"$out" 2>&1); then
  note "SQL Server refused something storm emitted:"
  grep -E "^(    )?(--- FAIL|.*refused|.*mssql:)" "$out" | head -40 | sed 's/^/    /'
fi

# COUNT the statements, do not trust the word ok.
#
# `go test` prints ok for a package whose tests all SKIPPED, and every test in
# here skips without a reachable server. The gate would then pass in exactly the
# situation it exists to catch — which is the shape of defect M9 shipped twice
# (docs/PRODUCTION-READINESS.md P6.7, and the label the mysql client stripped).
# So the floor is a number: fewer statements than the lowering can produce means
# something skipped, however green it looked.
ran=$(grep -c -- "--- PASS: .*/" "$out")
echo "== $ran statement(s) actually reached the server =="
if [ "$ran" -lt 35 ]; then
  note "only $ran statements ran; the gate covers 39, so something skipped rather than passed"
  grep -E "^(=== RUN|--- SKIP)" "$out" | head -10 | sed 's/^/    /'
fi
rm -f "$out"

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
