#!/usr/bin/env bash
# Does the MySQL DDL storm emits actually run on MySQL?
#
# A golden test proves the generator is consistent with itself; it cannot prove
# the SQL is valid. That is what shipped `DEFAULT  NOT NULL` — an empty default
# translation with the keyword still written — which every unit test accepted
# and MySQL rejected on the first line.
#
# Applied through the container's own client, so storm gains no MySQL driver
# dependency for a check that is about DDL text.
#
# Skipped unless STORM_MYSQL names a running container.
set -euo pipefail
cd "$(dirname "$0")/../.."

# Two ways in, because the check has to run in two places: a local Apple
# container (STORM_MYSQL names it) and CI, where MySQL is a service on
# localhost and there is no container to exec into (STORM_MYSQL_DSN).
# Always through STDIN, never -e: passing SQL as an argument means quoting it
# through `sh -c`, which silently mangles anything with a space in it and hands
# mysql its own --help.
if [ -n "${STORM_MYSQL_DSN:-}" ]; then
  # FAIL rather than skip. A gate that quietly disappears when a tool is
  # missing is the "documentation, not a gate" problem — the caller asked for
  # this check by setting the variable, so not being able to run it is an
  # error, not a pass.
  if ! command -v mysql >/dev/null 2>&1; then
    echo "STORM_MYSQL_DSN is set but the mysql client is not installed" >&2
    exit 1
  fi
  mysql_run() { mysql --protocol=TCP -h127.0.0.1 -ustorm -pstorm storm; }
elif [ -n "${STORM_MYSQL:-}" ]; then
  mysql_run() { container exec -i "$STORM_MYSQL" sh -c 'mysql -ustorm -pstorm storm'; }
else
  echo "neither STORM_MYSQL nor STORM_MYSQL_DSN set; skipping the MySQL DDL check"
  exit 0
fi

fail=0
note() { echo "  $*" >&2; fail=1; }

# A container's health check can pass before its init scripts have created the
# database and the user, so "the server answers" is not "the server is usable".
# Wait for the thing this check actually needs.
echo "== waiting for MySQL to be usable =="
ready=0
for _ in $(seq 1 30); do
  if echo 'SELECT 1' | mysql_run >/dev/null 2>&1; then ready=1; break; fi
  sleep 2
done
if [ "$ready" -ne 1 ]; then
  echo "MySQL never became usable as storm@storm" >&2
  exit 1
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$TMP/gen"
cat > "$TMP/gen/main.go" <<'EOF'
package main

import (
	"fmt"
	"os"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/myddl"
	"github.com/gsoultan/storm/examples/blog/model"
)

func main() {
	s, err := storm.Build(model.All()...)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ddl, err := myddl.Create(s)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Print(ddl)
}
EOF
cp -r "$TMP/gen" ./.mysqlgen
trap 'rm -rf "$TMP" ./.mysqlgen' EXIT

echo "== storm emits MySQL DDL for a portable model =="
if ! go run ./.mysqlgen > "$TMP/schema.sql" 2>"$TMP/err"; then
  note "generation failed:"; sed 's/^/    /' "$TMP/err" >&2
  echo "FAILED"; exit 1
fi

echo "== MySQL accepts it =="
echo 'DROP TABLE IF EXISTS articles; DROP TABLE IF EXISTS authors;' | mysql_run 2>/dev/null || true

if ! mysql_run < "$TMP/schema.sql" 2>"$TMP/apply"; then
  note "MySQL refused the DDL:"; sed 's/^/    /' "$TMP/apply" | grep -v Warning | head -5 >&2
fi

echo "== the tables, the index and the foreign key are all there =="
got="$(echo 'SHOW CREATE TABLE articles\G' | mysql_run 2>/dev/null || true)"
for want in 'binary(16)' 'datetime(6)' 'ix_articles_author_id' 'fk_articles_author_id' 'ON DELETE CASCADE'; do
  case "$got" in
    *"$want"*) ;;
    *) note "missing from the applied schema: $want" ;;
  esac
done

echo "== the portability report names what does NOT cross =="
mkdir -p ./.mysqlchk
cat > ./.mysqlchk/main.go <<'EOF'
package main

import (
	"fmt"
	"os"

	"github.com/gsoultan/storm"
	"github.com/gsoultan/storm/compile/myddl"
	"github.com/gsoultan/storm/internal/testmodel"
)

func main() {
	s, _ := storm.Build(testmodel.All()...)
	if err := myddl.Check(s); err != nil {
		fmt.Println(err)
		os.Exit(0)
	}
	fmt.Println("NO PROBLEMS")
}
EOF
report="$(go run ./.mysqlchk 2>&1 || true)"
rm -rf ./.mysqlchk
for want in 'array type' 'INTERVAL' 'network address' 'EXCLUDE'; do
  case "$report" in
    *"$want"*) ;;
    *) note "the portability report does not mention: $want" ;;
  esac
done

echo "== MySQL PREPAREs and EXECUTEs every statement compile/mysql lowers =="
# The gate that was missing, and the reason the dialect emitted PostgreSQL SQL
# through four releases: a package that COMPILES is not a package that RUNS.
# TestMySQLGeneratedPackageCompiles asserted the cheaper claim and read as
# though it asserted this one.
#
# PREPARE rather than a driver, so storm gains no MySQL dependency for a check
# about SQL text — the same reasoning the DDL half above already applies. It is
# also the stronger assertion: PREPARE type-checks the statement against the
# real schema, placeholders included.
if ! go run ./internal/myquerycheck > "$TMP/queries.sql" 2>"$TMP/qerr"; then
  note "emitting the query script failed:"; sed 's/^/    /' "$TMP/qerr" >&2
else
  if ! mysql_run < "$TMP/queries.sql" > "$TMP/qout" 2>"$TMP/qapply"; then
    note "MySQL refused a lowered statement:"
    grep -v Warning "$TMP/qapply" | head -5 | sed 's/^/    /' >&2
  fi
  # ADR-0010's load-bearing claim: the JSON_TABLE IN-list must still reach the
  # index. A lowering that is merely ACCEPTED but scans every row would have
  # traded a correctness problem for a performance one.
  # Two index claims, not one: the IN-list and the batch loader. Both are the
  # difference between a lowering that works and one that reads every row.
  got_idx="$(grep -ci 'index lookup\|eq_ref' "$TMP/qout" || true)"
  if [ "${got_idx:-0}" -lt 2 ]; then
    note "a JSON_TABLE lowering stopped using an index (ADR-0010) — $got_idx of 2 plans:"
    tail -6 "$TMP/qout" | sed 's/^/    /' >&2
  fi
  # The recursive traversal ANSWERS, not just its acceptance. A cycle guard
  # that returns the depth bound's worth of rows instead of stopping at the
  # revisited key is accepted by the server and wrong — which is the whole
  # difference between a guard and a bound.
  chain="$(awk '$1=="recursive_reaches_every_level"{print $2;exit}' "$TMP/qout")"
  if [ "${chain:-0}" != "4" ]; then
    note "the recursive traversal reached $chain levels of a four-level chain"
  fi
  cyc="$(awk '$1=="recursive_cycle_terminates"{print $2;exit}' "$TMP/qout")"
  if [ "${cyc:-0}" != "2" ]; then
    note "a two-node cycle produced $cyc rows under a depth bound of 200 — the guard stopped at the bound, not at the revisited key"
  fi
fi

if [ "$fail" -eq 0 ]; then
  echo "OK: storm's MySQL DDL applies, its queries PREPARE and EXECUTE, and its portability report names what does not cross"
else
  echo "FAILED"
fi
exit "$fail"
