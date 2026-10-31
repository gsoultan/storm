#!/usr/bin/env bash
# Allocation budgets, checked rather than quoted.
#
# "6 allocations for one row or 1,000" is storm's headline claim, and until
# this gate CI only checked that the benchmarks still compiled and ran. A
# change that added one allocation to the warm path would have shipped green.
#
# allocs/op is deterministic where ns/op is not: the same code on the same Go
# release allocates the same number of times on any machine, so this compares
# exactly against bench/ALLOCS and needs no tolerance and no benchstat.
#
# Only storm's own paths are gated. The rival implementations and the
# deliberately naive contrasts (pgx's generic codecs, a string per row) are
# left out: their counts move with their own releases, not with storm.
#
#   scripts/check/allocs.sh           compare against bench/ALLOCS
#   scripts/check/allocs.sh -update   rewrite bench/ALLOCS from this run
#
# Needs STORM_DSN (PostgreSQL) and STORM_MYSQL_ADDR, like the suites it runs.
# A benchmark in the baseline that did not run FAILS the gate: one that skips
# for want of a server would otherwise pass by measuring nothing.
set -euo pipefail
cd "$(dirname "$0")/../.."

BASELINE=bench/ALLOCS
PG_BENCH='^Benchmark(Gen|Iso_Gen|Decode|Aggregate|AnyParam|Encode|TopN)'
MY_BENCH='^BenchmarkQuery200x8$'
# Contrasts inside the gated benchmarks, measured beside storm on purpose.
CONTRAST='(generic|slice_of_array|flat_array|PerRowString)'
# Enough iterations that a cold first call (a shape compiled, a connection
# opened) is amortised to zero in the integer allocs/op go test prints.
N=100x

run() { # <pattern> <package>
  go test -run XXX -bench "$1" -benchmem -benchtime="$N" "$2" |
    awk '/^Benchmark/ && / allocs\/op/ { name = $1; sub(/-[0-9]+$/, "", name); print name, $(NF-1) }'
}

got=$( { run "$PG_BENCH" ./bench/; run "$MY_BENCH" ./runtime/mydrv/; } | grep -Ev "$CONTRAST" | sort)

if [ "${1:-}" = "-update" ]; then
  {
    echo "# Allocations per op for storm's own paths, compared exactly by"
    echo "# scripts/check/allocs.sh. Regenerate with: scripts/check/allocs.sh -update"
    echo "$got"
  } > "$BASELINE"
  echo "wrote $BASELINE ($(echo "$got" | wc -l | tr -d ' ') benchmarks)"
  exit 0
fi

fail=0
while read -r name want; do
  case "$name" in ''|'#'*) continue ;; esac
  have=$(echo "$got" | awk -v n="$name" '$1 == n { print $2 }')
  if [ -z "$have" ]; then
    echo "FAIL: $name did not run (no server, or renamed?)"
    fail=1
  elif [ "$have" -gt "$want" ]; then
    echo "FAIL: $name allocates $have/op, budget $want"
    fail=1
  elif [ "$have" -lt "$want" ]; then
    echo "note: $name allocates $have/op, below its budget of $want: lower it with -update"
  fi
done < "$BASELINE"

# A new benchmark without a budget is not gated yet; say so rather than pass it
# silently.
echo "$got" | while read -r name _; do
  grep -q "^$name " "$BASELINE" || echo "note: $name has no budget in $BASELINE: add it with -update"
done

if [ "$fail" -ne 0 ]; then
  echo "allocation budgets exceeded (see above). If the increase is intended, run"
  echo "scripts/check/allocs.sh -update and say why in the commit."
  exit 1
fi
echo "OK: every allocation budget holds"
