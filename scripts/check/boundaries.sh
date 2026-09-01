#!/usr/bin/env bash
# AGENTS.md calls these rules CI-enforced. This is the enforcement.
set -uo pipefail
cd "$(dirname "$0")/../.."
fail=0
note() { printf '  %s\n' "$1"; fail=1; }

echo "== no reflect under runtime/ =="
# One reflection path becomes THE path, and then the SLOs are fiction.
if grep -rn '"reflect"' runtime/ 2>/dev/null; then
  note "reflect is imported under runtime/"
fi

echo "== driver confined to its adapter =="
# Only runtime/pgxdrv may name pgx, so driver churn cannot reach the tree.
#
# The exemptions are all BUILD-TIME code — the tool, introspection, the
# migration runner — which talks to a database directly and never hands a pgx
# type to an application. tool/ is on the list because the commands moved
# there from cmd/ when they became importable; that changed who can call them,
# not when they run. Nothing an application links at runtime is exempt.
#
# examples/ is exempt for a different reason: it is ADOPTER code, and an
# adopter importing pgx to build a pool is the documented way to get one — not
# a boundary violation. examples/orders is its own module and does exactly
# that. The rule is about what STORM links, and a separate module is not that.
for f in $(git ls-files '*.go' 2>/dev/null | grep -v '_test.go' | grep -v '^bench/' | grep -v '^cmd/'); do
  case "$f" in
    runtime/pgxdrv/*|schema/pg/*|migrate/*|internal/spike/*|tool/*|examples/*) continue ;;
  esac
  if grep -q 'jackc/pgx' "$f"; then note "pgx imported outside its adapter: $f"; fi
done

echo "== core packages are stdlib-only =="
for p in ./schema ./compile/pgddl ./compile/pgsql ./codegen; do
  if go list -deps "$p" 2>/dev/null | grep -v '^github.com/gsoultan/storm' | grep -q '\.'; then
    note "$p has a third-party dependency:"
    go list -deps "$p" | grep -v '^github.com/gsoultan/storm' | grep '\.' | sed 's/^/    /'
  fi
done

echo "== no fmt.Sprintf building SQL at runtime =="
# Formatting on a hot path, and an injection surface.
if grep -rn 'fmt.Sprintf' runtime/ 2>/dev/null | grep -v '_test.go'; then
  note "fmt.Sprintf in the runtime path"
fi

echo "== no SQL text in codegen/ (R9: the dialect seam) =="
# The precise check is an AST walk over string literals; bash cannot tell a
# keyword in a comment from one in a literal.
if ! go test ./codegen/ -run TestNoSQLTextInCodegen -count=1 >/dev/null 2>&1; then
  note "SQL text is being written in codegen/ — it belongs in compile/pgsql:"
  go test ./codegen/ -run TestNoSQLTextInCodegen -count=1 2>&1 | grep 'SQL text' | sed 's/^/    /'
fi

echo "== generated code is not stale =="
# storm verify fails CI on stale output; these are the in-tree instances of it.
stale() { # <sha-before> <generator> <label>
  if [ "$1" != "$2" ]; then note "generated code is stale — run '$3' and commit"; fi
}
before=$(shasum -a 256 bench/genuser/user.gen.go 2>/dev/null | cut -d' ' -f1)
go run ./cmd/genbench >/dev/null 2>&1
stale "$before" "$(shasum -a 256 bench/genuser/user.gen.go 2>/dev/null | cut -d' ' -f1)" "go run ./cmd/genbench"

before=$(cat internal/planspike/store/*.gen.go internal/planspike/store/*/*.gen.go 2>/dev/null | shasum -a 256 | cut -d' ' -f1)
go run ./cmd/genspike >/dev/null 2>&1
stale "$before" "$(cat internal/planspike/store/*.gen.go internal/planspike/store/*/*.gen.go 2>/dev/null | shasum -a 256 | cut -d' ' -f1)" "go run ./cmd/genspike"

before=$(cat examples/blog/store/*.gen.go examples/blog/store/*/*.gen.go 2>/dev/null | shasum -a 256 | cut -d' ' -f1)
go run ./examples/blog/gen >/dev/null 2>&1
stale "$before" "$(cat examples/blog/store/*.gen.go examples/blog/store/*/*.gen.go 2>/dev/null | shasum -a 256 | cut -d' ' -f1)" "go run ./examples/blog/gen"

echo "== gofmt =="
if [ -n "$(gofmt -l . 2>/dev/null)" ]; then
  note "not gofmt'd:"; gofmt -l . | sed 's/^/    /'
fi

if [ "$fail" -eq 0 ]; then echo "OK: all boundary checks pass"; else echo "FAILED"; fi
exit "$fail"
