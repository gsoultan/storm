#!/usr/bin/env bash
# Be a stranger.
#
# Every other gate in this directory runs INSIDE this repository and therefore
# shares its assumptions. That is how `storm generate` shipped in v0.1.0
# emitting storm's own module path into other people's modules: the wrong
# answer is the right answer here, so nothing could see it — not the tests,
# not the coverage floors, not even the first adopter, because the adopter was
# also us.
#
# So this builds a module that shares nothing: a different module path, a
# directory outside the tree, a model storm has never seen, and the five-line
# main a real user writes. It needs no database — `ddl` prints and `generate`
# only needs a server when there are raw queries to PREPARE — so it belongs in
# the fast CI job, not the one with a Postgres service.
set -euo pipefail
cd "$(dirname "$0")/../.."
REPO="$PWD"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
cd "$TMP"

mkdir -p model cmd/storm
cat > go.mod <<EOF
module example.com/outsider

go 1.26
EOF

# A model with a relation and a nullable column: enough shape that the
# generated context package must import the per-table packages, which is where
# the import path bug lived.
cat > model/model.go <<'EOF'
package model

import "github.com/gsoultan/storm"

type Team struct {
	storm.Model
	Name    string
	Members []Member
}

func (t *Team) Schema(s *storm.Table) { s.Unique(&t.Name) }

// A named plan, so `lint` has a load pattern to cost and `explain` has more
// than one statement to plan.
func (t *Team) Plans(p *storm.Plans) { p.Named("Roster").With(&t.Members) }

type Member struct {
	storm.Model
	Team     Team
	Email    string
	Nickname *string
}

func (m *Member) Schema(s *storm.Table) { s.Unique(&m.Email) }

func All() []any { return []any{&Team{}, &Member{}} }
EOF

cat > cmd/storm/main.go <<'EOF'
package main

import (
	"example.com/outsider/model"
	"github.com/gsoultan/storm/tool"
)

func main() { tool.Main(model.All(), nil) }
EOF

go mod edit -replace "github.com/gsoultan/storm=$REPO"
GOFLAGS=-mod=mod go mod tidy >/dev/null 2>&1

fail=0
note() { echo "  $*" >&2; fail=1; }

echo "== a stranger's module builds the tool =="
if ! go build ./... >/dev/null 2>&1; then
  note "the five-line tool main does not compile:"
  go build ./... 2>&1 | sed 's/^/    /' >&2
fi

echo "== ddl needs no database =="
if ! go run ./cmd/storm ddl > ddl.sql 2>ddl.err; then
  note "ddl failed:"; sed 's/^/    /' ddl.err >&2
elif ! grep -q 'CREATE TABLE "teams"' ddl.sql; then
  note "ddl did not emit the model's tables"
fi

echo "== generate emits the HOST module's import path =="
if ! go run ./cmd/storm generate internal/store >gen.out 2>gen.err; then
  note "generate failed:"; sed 's/^/    /' gen.err >&2
else
  ctx="internal/store/store.gen.go"
  if ! grep -q '"example.com/outsider/internal/store/' "$ctx"; then
    note "generated code does not import the host module — this is the v0.1.0 bug"
    grep -n 'internal/store/' "$ctx" | head -3 | sed 's/^/    /' >&2
  fi
  if grep -q '"github.com/gsoultan/storm/internal/store/' "$ctx"; then
    note "generated code imports STORM's module path for the user's packages"
  fi
  # The runtime import must still point at storm: the two halves are
  # different questions and the fix for one must not break the other.
  if ! grep -q '"github.com/gsoultan/storm/runtime"' "$ctx"; then
    note "the runtime import no longer points at storm"
  fi
fi

echo "== the generated code compiles in a module that is not storm =="
if ! go build ./... >build.err 2>&1; then
  note "generated code does not compile:"; sed 's/^/    /' build.err | head -5 >&2
fi

# The migration path, when a server is available. It is the riskiest thing an
# ORM does — it changes schemas that hold production data — and until now it
# had only ever been exercised inside storm's own module, the same blind spot
# that let `generate` ship broken. Skipped without a DSN so the fast CI job
# stays database-free.
if [ -n "${STORM_DSN:-}" ]; then
  echo "== a stranger can diff, apply and verify a migration =="
  ns="storm_outsider_$$"
  case "$STORM_DSN" in *\?*) sep="&" ;; *) sep="?" ;; esac
  scoped="${STORM_DSN}${sep}search_path=${ns}"

  # An adopter applies migrations with their own runner — storm never does.
  # This is the smallest honest stand-in for one.
  mkdir -p cmd/apply
  cat > cmd/apply/main.go <<'GOEOF'
package main

import (
	"context"
	"os"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx := context.Background()
	c, err := pgx.Connect(ctx, os.Args[1])
	if err != nil {
		panic(err)
	}
	defer c.Close(ctx)
	sql, err := os.ReadFile(os.Args[2])
	if err != nil {
		panic(err)
	}
	if _, err := c.Exec(ctx, string(sql)); err != nil {
		panic(err)
	}
}
GOEOF
  GOFLAGS=-mod=mod go mod tidy >/dev/null 2>&1

  drop_ns() { go run ./cmd/apply "$STORM_DSN" /dev/stdin <<< "DROP SCHEMA IF EXISTS ${ns} CASCADE" >/dev/null 2>&1 || true; }
  trap 'drop_ns; rm -rf "$TMP"' EXIT
  if ! go run ./cmd/apply "$STORM_DSN" /dev/stdin <<< "CREATE SCHEMA ${ns}" >/dev/null 2>&1; then
    note "could not create a scratch namespace to migrate into"
  else
    if ! go run ./cmd/storm diff init -dsn "$scoped" -schema "$ns" -out migrations >diff.out 2>diff.err; then
      note "diff failed:"; sed 's/^/    /' diff.err >&2
    else
      up="$(ls migrations/*init*.up.sql 2>/dev/null | head -1)"
      if [ -z "$up" ]; then
        note "diff wrote no up migration"
      elif ! grep -q 'CREATE TABLE' "$up"; then
        note "the migration contains no CREATE TABLE"
      elif go run ./cmd/storm verify -dsn "$scoped" -schema "$ns" >/dev/null 2>&1; then
        # Before applying anything, an empty namespace MUST read as drifted.
        # Without this the "clean afterwards" below would pass just as well if
        # verify were broken and always said yes.
        note "an empty namespace verified clean — verify is not looking"
      elif ! go run ./cmd/apply "$scoped" "$up" >apply.err 2>&1; then
        note "the migration storm emitted does not apply:"; sed 's/^/    /' apply.err | head -5 >&2
      elif ! go run ./cmd/storm verify -dsn "$scoped" -schema "$ns" >verify.out 2>&1; then
        note "after applying its own migration, verify still reports drift:"
        sed 's/^/    /' verify.out | head -6 >&2
      else
        # The commands nobody outside this repository had ever run. `generate`
        # was broken for every outsider for months; there is no reason to
        # assume these are not.

        echo "== verify -pending: the model against its migrations =="
        if ! go run ./cmd/storm verify -pending -dsn "$scoped" -schema "$ns" -out migrations >pending.out 2>&1; then
          note "the migration diff just wrote does not carry the model:"
          sed 's/^/    /' pending.out | head -6 >&2
        fi

        echo "== lint: the named plan is costed =="
        if ! go run ./cmd/storm lint -dsn "$scoped" -schema "$ns" >lint.out 2>&1; then
          note "lint failed:"; sed 's/^/    /' lint.out | head -6 >&2
        elif ! grep -qi 'roster' lint.out; then
          note "lint did not cost the declared plan:"; sed 's/^/    /' lint.out | head -6 >&2
        fi

        echo "== explain: every statement planned =="
        if ! go run ./cmd/storm explain -dsn "$scoped" -schema "$ns" >explain.out 2>&1; then
          note "explain failed:"; sed 's/^/    /' explain.out | head -8 >&2
        elif ! grep -qE 'statement\(s\) planned' explain.out; then
          note "explain planned nothing:"; sed 's/^/    /' explain.out | head -6 >&2
        fi

        echo "== import: the on-ramp for an existing database =="
        # The most common adoption path there is: point storm at a schema you
        # already have and get a model draft back. It must be Go that parses,
        # not prose — a draft nobody can compile is not an on-ramp.
        if ! go run ./cmd/storm import -dsn "$scoped" -schema "$ns" >imported.go 2>import.err; then
          note "import failed:"; sed 's/^/    /' import.err | head -6 >&2
        elif ! grep -q 'type Team' imported.go; then
          note "the imported model does not describe the schema it read:"
          sed 's/^/    /' imported.go | head -8 >&2
        else
          mkdir -p imported
          # gofmt parses; a draft that does not is a draft nobody can use.
          if ! gofmt -e imported.go > imported/model.go 2>fmt.err; then
            note "the imported model is not valid Go:"; sed 's/^/    /' fmt.err | head -5 >&2
          fi
        fi
      fi
    fi
  fi
fi

if [ "$fail" -eq 0 ]; then
  if [ -n "${STORM_DSN:-}" ]; then
    echo "OK: a module outside this repository can model, generate, build, migrate and verify"
  else
    echo "OK: a module outside this repository can model, generate and build (migration path skipped — no STORM_DSN)"
  fi
else
  echo "FAILED"
fi
exit "$fail"
