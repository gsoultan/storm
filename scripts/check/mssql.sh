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
mig=$(mktemp)
if ! go test -count=1 -v -run 'TestMSSQLMigrationRoundTrip|TestMSSQLAltersApply|TestAutoMSSQL' ./migrate/ >"$mig" 2>&1; then
  note "the server refused a statement migrate emitted, or a property did not hold:"
  grep -E -- "^ *--- FAIL" "$mig" | head -10 | sed 's/^/    /'
  # Anchored on --- FAIL and nothing else. An earlier version of this grep
  # listed the PHRASES a failure might use, which worked until the next test
  # used a different one — and then reported two failures and not one word about
  # why, for the second time in this file's life. What a test says when it fails
  # is the test's business.
  grep -E -B2 -A16 -- "^ *--- FAIL" "$mig" | head -90 | sed 's/^/    /'
fi
# COUNT the alters, do not trust the word ok: these skip without a server, and a
# gate that passes by not running is the defect this whole file is named after.
ran=$(grep -c -- "--- PASS: TestMSSQLAltersApply/" "$mig")
echo "== $ran alter(s) reached the server =="
if [ "$ran" -lt 9 ]; then
  note "only $ran of 9 alters ran; the rest skipped rather than passed"
  grep -E "^(=== RUN|--- SKIP)" "$mig" | head -10 | sed 's/^/    /'
fi

# Automigrate, which is the one path in storm that writes DDL nobody reviewed.
# Its four claims — it converges, it refuses to lose data, a failure leaves the
# schema where it began, and concurrent callers apply once — are all about what
# the SERVER holds afterwards, so its tests are counted too.
ran=$(grep -c -- "--- PASS: TestAutoMSSQL" "$mig")
echo "== $ran automigrate propert(ies) reached the server =="
if [ "$ran" -lt 5 ]; then
  note "only $ran of 5 automigrate tests ran; the rest skipped rather than passed"
fi
rm -f "$mig"

# The SQL Server half of the CLI, RUN. Three of these are storm.SQL's whole
# safety story — a server of the TARGET's own kind types every declared
# statement — and the fourth is `storm verify -pending`: write the migration the
# tool would write, replay it, and demand the model have nothing left to ask
# for.
#
# This used to be a CI step of its own pointing at ./tool/, and it kept passing
# after the code moved to ./tool/mstool/ because `go test -run` with no matches
# is a success. It is counted here now, for exactly that reason.
echo "== the escape hatch and the migration replay run against a server =="
out=$(mktemp)
if ! go test -count=1 -v ./tool/mstool/ >"$out" 2>&1; then
  note "the SQL Server half of the CLI failed:"
  grep -E -B2 -A16 -- "^ *--- FAIL" "$out" | head -90 | sed 's/^/    /'
fi
ran=$(grep -c -E -- "--- PASS: (TestRawQuer|TestVerifyPending)" "$out")
echo "== $ran of them reached the server =="
if [ "$ran" -lt 4 ]; then
  note "only $ran of 4 server-backed CLI tests ran; the rest skipped rather than passed"
fi
rm -f "$out"

# The packages whose live half only runs where there is a server. Their floors
# live here rather than in scripts/check/coverage.sh for that reason: measured
# without one, they would be measuring a suite that skipped.
echo "== the SQL Server packages are above their floors =="
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
# The SQL Server half of the CLI. 68 because the first honest measurement of it
# was 71.6, taken here. The gap is slack for a live test that varies, not
# headroom to spend.
#
# It is a package because of this line. tool used to be one package split across
# two jobs — PostgreSQL and no SQL Server in one, SQL Server and no PostgreSQL in
# the other — so no floor either job could measure meant anything, and the one in
# coverage.sh was nudged down twice chasing it. Splitting the code split the
# measurement.
cov ./tool/mstool 68

if [ "$fail" -eq 0 ]; then
  echo "OK: storm's SQL Server DDL applies, and every statement it lowers runs"
else
  echo "FAILED"
fi
exit "$fail"
