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

if [ "$fail" -eq 0 ]; then
  echo "OK: storm's MySQL DDL applies, and its portability report names what does not cross"
else
  echo "FAILED"
fi
exit "$fail"
