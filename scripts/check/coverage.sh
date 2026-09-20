#!/usr/bin/env bash
# Coverage floors for the packages whose correctness is not observable from
# outside — a wrong fragment table or a wrong splice produces valid SQL that
# means something else.
#
# Floors, not targets. They stop coverage falling silently; they say nothing
# about whether the tests are good, and a package at its floor with bad tests is
# worse than one below it with good ones.
set -uo pipefail
cd "$(dirname "$0")/../.."

declare -a FLOORS=(
  "github.com/gsoultan/storm/runtime 95"
  # The only package that knows pgx exists, and now the home of the wire-format
  # guard: a wrong entry in its deny-list is a value decoded as the wrong type,
  # which is precisely the "not observable from outside" failure floors are for.
  # Lower than the others because a driver adapter's remaining statements are
  # error plumbing that needs a broken server to reach.
  "github.com/gsoultan/storm/runtime/pgxdrv 85"
  # The MySQL/MariaDB adapter, which is a wire protocol written by hand: an
  # off-by-one in a packet offset produces a plausible value, not an error.
  # Needs STORM_MYSQL_ADDR to reach anything, which is the same bargain pgxdrv
  # makes with STORM_DSN.
  "github.com/gsoultan/storm/runtime/mydrv 80"
  "github.com/gsoultan/storm/compile/mysql 80"
  "github.com/gsoultan/storm/compile/mariadb 80"
  "github.com/gsoultan/storm/runtime/mydec 90"
  "github.com/gsoultan/storm/compile/myddl 85"
  # M10's two, at the level they landed. The SQL Server lowering has a live
  # gate too (scripts/check/mssql.sh), but that one runs in a separate module
  # and needs a server, so it contributes nothing here — which is exactly why
  # these floors matter: they are what holds when the server is not there.
  "github.com/gsoultan/storm/compile/mssql 85"
  "github.com/gsoultan/storm/compile/msddl 85"
  # runtime/msdrv and runtime/msdec are NOT here. Their live half needs a SQL
  # Server, which this run does not have — it would measure a suite that
  # skipped, and a floor met by a package whose tests did not run is a floor
  # that means nothing. They are enforced in scripts/check/mssql.sh, which does
  # have one.
  "github.com/gsoultan/storm/compile/pgsql 80"
  "github.com/gsoultan/storm/compile/pgddl 90"
  "github.com/gsoultan/storm/codegen 85"
  "github.com/gsoultan/storm/migrate 75"
  "github.com/gsoultan/storm/schema/pg 80"
  "github.com/gsoultan/storm 65"
  # 68, not 70, and the two points are a statement about this ENVIRONMENT
  # rather than about the tests. tool gained the SQL Server half of the
  # storm.SQL escape hatch, whose tests need a server this job has not got, so
  # what is measured here is the package MINUS that file. The whole package is
  # floored at 75 in scripts/check/mssql.sh, which has one.
  "github.com/gsoultan/storm/tool 68"
)

prof=$(mktemp)
trap 'rm -f "$prof"' EXIT

echo "== measuring =="
# The output is KEPT. It used to go to /dev/null, so a suite that failed here
# and nowhere else — this is the only run built with -coverpkg, which compiles
# every package differently from the plain one above it — reported one line
# with no test name in it, and finding out cost a CI round trip.
out=$(mktemp)
if ! go test -count=1 -coverpkg=./... -coverprofile="$prof" ./... >"$out" 2>&1; then
  echo "FAILED: the test suite must pass before coverage means anything"
  grep -E "^(--- )?FAIL|^# |panic:" "$out" | head -20 | sed 's/^/    /'
  rm -f "$out"
  exit 1
fi
rm -f "$out"

fail=0
for entry in "${FLOORS[@]}"; do
  pkg=${entry% *}
  floor=${entry##* }
  # Average of per-function coverage for files in exactly this package.
  pct=$(go tool cover -func="$prof" \
        | awk -v p="$pkg/" '{ f=$1; sub(/:.*/,"",f); d=f; sub(/\/[^\/]*$/,"/",d);
            if (d == p) { c=$NF; sub(/%/,"",c); n++; s+=c } }
            END { if (n) printf "%.1f", s/n; else print "0" }')
  ok=$(awk -v a="$pct" -v b="$floor" 'BEGIN{print (a+0 >= b+0) ? "ok" : "low"}')
  printf '  %-42s %6s%%  floor %s%%  %s\n' "${pkg#github.com/gsoultan/}" "$pct" "$floor" "$ok"
  [ "$ok" = ok ] || fail=1
done

if [ "$fail" -eq 0 ]; then echo "OK: every package is above its floor"; else echo "FAILED"; fi
exit "$fail"
