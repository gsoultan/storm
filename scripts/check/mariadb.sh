#!/usr/bin/env bash
# Does the SQL storm emits for MariaDB actually run on MariaDB?
#
# A separate gate from mysql.sh, not a flag on it, because the point is that the
# two are DIFFERENT dialects. They share a wire protocol and four fifths of
# their SQL; the fifth is what this checks, and a gate that ran the shared part
# twice would prove nothing about the part that diverges.
#
# Measured differences, 11.4.13 vs 8.4.11: MariaDB HAS INSERT ... RETURNING and
# has NOT got LATERAL, GROUPING(), ordered WITH ROLLUP, or FOR SHARE.
#
# Skipped unless STORM_MARIADB names a running container, or STORM_MARIADB_DSN
# names a host:port — the same two ways in that mysql.sh has, because the check
# has to run locally against a container and in CI against a service.
set -uo pipefail
cd "$(dirname "$0")/../.."

if [ -n "${STORM_MARIADB_DSN:-}" ]; then
  # FAIL rather than skip: the caller asked for this check by setting the
  # variable, so not being able to run it is an error, not a pass.
  if command -v mariadb >/dev/null 2>&1; then
    CLIENT=mariadb
  elif command -v mysql >/dev/null 2>&1; then
    CLIENT=mysql
  else
    echo "STORM_MARIADB_DSN is set but no mariadb/mysql client is installed" >&2
    exit 1
  fi
  MARIA_HOST="${STORM_MARIADB_DSN%%:*}"
  MARIA_PORT="${STORM_MARIADB_DSN##*:}"
  maria_run() {
    "$CLIENT" --protocol=TCP -h"$MARIA_HOST" -P"$MARIA_PORT" -uroot -pstorm storm
  }
elif [ -n "${STORM_MARIADB:-}" ]; then
  maria_run() { container exec -i "$STORM_MARIADB" sh -c 'mariadb -uroot -pstorm storm'; }
else
  echo "neither STORM_MARIADB nor STORM_MARIADB_DSN set; skipping the MariaDB check"
  exit 0
fi

fail=0
note() { echo "  $*" >&2; fail=1; }

echo "== waiting for MariaDB to be usable =="
ready=0
for _ in $(seq 1 30); do
  if echo 'SELECT 1' | maria_run >/dev/null 2>&1; then ready=1; break; fi
  sleep 2
done
if [ "$ready" -ne 1 ]; then
  echo "MariaDB never became usable" >&2; exit 1
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "== MariaDB PREPAREs and EXECUTEs what storm emits for it =="
if ! go run ./internal/mariadbcheck > "$TMP/q.sql" 2>"$TMP/err"; then
  note "emitting the script failed:"; sed 's/^/    /' "$TMP/err" >&2
else
  if ! maria_run < "$TMP/q.sql" > "$TMP/out" 2>"$TMP/apply"; then
    note "MariaDB refused a statement:"
    grep -v Warning "$TMP/apply" | head -5 | sed 's/^/    /' >&2
  fi
  # The difference that pays for the dialect: the insert must hand the row back.
  if ! grep -q 'returned_id' "$TMP/out"; then
    note "the insert did not return the row it wrote — the whole reason MariaDB is a"
    note "separate target rather than an alias for MySQL"
  fi
  # The recursive traversal's ANSWERS. A cycle guard that returns the depth
  # bound's worth of rows instead of stopping at the revisited key is accepted
  # by the server and wrong — the difference between a guard and a bound.
  lv="$(awk '$1=="mc_recursive_levels"{print $2;exit}' "$TMP/out")"
  if [ "${lv:-0}" != "4" ]; then
    note "the recursive traversal reached ${lv:-no} levels of a four-level chain"
  fi
  cy="$(awk '$1=="mc_recursive_cycle"{print $2;exit}' "$TMP/out")"
  if [ "${cy:-0}" != "2" ]; then
    note "a two-node cycle produced ${cy:-no} rows under a depth bound of 200 — the guard stopped at the bound, not at the revisited key"
  fi
fi

if [ "$fail" -eq 0 ]; then
  echo "OK: storm's MariaDB SQL applies, and its insert returns the row it wrote"
else
  echo "FAILED"
fi
exit "$fail"
