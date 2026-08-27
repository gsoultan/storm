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
  "github.com/gsoultan/storm/compile/pgsql 80"
  "github.com/gsoultan/storm/compile/pgddl 90"
  "github.com/gsoultan/storm/codegen 85"
  "github.com/gsoultan/storm/migrate 75"
  "github.com/gsoultan/storm/schema/pg 80"
  "github.com/gsoultan/storm 65"
  "github.com/gsoultan/storm/tool 70"
)

prof=$(mktemp)
trap 'rm -f "$prof"' EXIT

echo "== measuring =="
if ! go test -count=1 -coverpkg=./... -coverprofile="$prof" ./... >/dev/null 2>&1; then
  echo "FAILED: the test suite must pass before coverage means anything"
  exit 1
fi

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
