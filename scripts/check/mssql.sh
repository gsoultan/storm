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

# The Go tests below need the address and the password as their own variables —
# the DSN is the borrowed client's form, and runtime/msdrv takes neither a DSN
# nor a URL. Missing them is an ERROR rather than a skip: the coverage floors at
# the end would otherwise measure a suite that skipped, which is the exact thing
# the statement count above refuses to do one level up.
if [ -z "${STORM_MSSQL_ADDR:-}" ] || [ -z "${STORM_MSSQL_PASSWORD:-}" ]; then
  echo "STORM_MSSQL_DSN is set but STORM_MSSQL_ADDR and STORM_MSSQL_PASSWORD are not:" >&2
  echo "  the TDS client's own tests take host:port and a password, not a DSN" >&2
  exit 1
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

# The migration path, APPLIED. migrate/ renders five statements whose SQL Server
# spelling differs, and four of the five fail in a way no text assertion sees:
# ADD COLUMN parses as a column named COLUMN, an ALTER COLUMN that omits
# NULL/NOT NULL takes a session setting's answer, a second DEFAULT constraint is
# error 1781, and DROP INDEX without ON does not parse.
#
# The property is the SECOND plan being empty. That is the only evidence that
# normalisation and introspection agree about widths, parenthesised defaults and
# the CHECK an enum became — and a migration that reapplies itself forever is
# what disagreement looks like in production.
echo "== a SQL Server migration plan applies, and the next plan is empty =="
out=$(mktemp)
if ! go test -count=1 -v -run 'TestMSSQLMigrationRoundTrip|TestMSSQLAltersApply' ./migrate/ >"$out" 2>&1; then
  note "the server refused a statement migrate emitted, or the re-diff was not empty:"
  grep -E -- "--- FAIL|refused by the server|is not empty|tested nothing" "$out" | head -20 | sed 's/^/    /'
fi
# COUNT the alters, do not trust the word ok: these skip without a server, and a
# gate that passes by not running is the defect this whole file is named after.
ran=$(grep -c -- "--- PASS: TestMSSQLAltersApply/" "$out")
echo "== $ran alter(s) reached the server =="
if [ "$ran" -lt 9 ]; then
  note "only $ran of 9 alters ran; the rest skipped rather than passed"
  grep -E "^(=== RUN|--- SKIP)" "$out" | head -10 | sed 's/^/    /'
fi
rm -f "$out"

# The two runtime packages whose live half only runs where there is a server.
# Their floors live here rather than in scripts/check/coverage.sh for that
# reason: measured without one, they would be measuring a suite that skipped.
echo "== the TDS client and its decoders are above their floors =="
cov() { # <package> <floor>
  pct=$(go test -count=1 -cover "$1" 2>/dev/null |
    sed -n 's/.*coverage: \([0-9.]*\)%.*/\1/p')
  if [ -z "$pct" ]; then
    note "$1 reported no coverage at all, so its tests did not run"
    return
  fi
  ok=$(awk -v a="$pct" -v b="$2" 'BEGIN{print (a+0 >= b+0) ? "ok" : "low"}')
  printf '  %-36s %6s%%  floor %s%%  %s\n' "${1#github.com/gsoultan/}" "$pct" "$2" "$ok"
  [ "$ok" = "ok" ] || note "$1 is below its floor"
}
cov ./runtime/msdrv 55
cov ./runtime/msdec 85
# The introspector, whose every query reads a catalogue — which is not
# something a fake can be honest about, so it has no unit half at all.
cov ./schema/mssql 75
# tool is NOT floored here, and that is a deliberate answer to a question this
# gate asked and got wrong once. No single job can measure it whole: the other
# one has PostgreSQL and no SQL Server (69%), this one has SQL Server and no
# PostgreSQL (54%), and only a developer with both sees 83%. A floor is a
# tripwire against code nothing runs, and the SQL Server half of the escape
# hatch IS run — by the three live tests in this job, which fail the build
# directly. A number neither environment can reach is instrument noise, not a
# gate.

if [ "$fail" -eq 0 ]; then
  echo "OK: storm's SQL Server DDL applies, and every statement it lowers runs"
else
  echo "FAILED"
fi
exit "$fail"
