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
# The exemptions were all BUILD-TIME code — the tool, introspection, the
# migration runner — which talks to a database directly and never hands a pgx
# type to an application. tool/ is on the list because the commands moved
# there from cmd/ when they became importable; that changed who can call them,
# not when they run.
#
# migrate/ broke the "build-time only" half of that sentence when it gained
# Auto (automigrate), which an application calls at startup. The rule that
# replaces it is narrower and is checked below rather than asserted here: the
# ROOT package must stay driver-free, so importing storm never pulls in pgx and
# only an application that reaches for migrate/ on purpose pays for it.
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

echo "== importing storm does not import a driver =="
# The property that survived migrate/ becoming runtime-linked: `import
# "github.com/gsoultan/storm"` must not drag pgx into an application's binary.
# migrate.Auto is opt-in precisely because it is a separate import; if pgx ever
# reaches the root package's closure, that choice has been taken away from
# every adopter, silently.
if go list -deps . 2>/dev/null | grep -q 'jackc/pgx'; then
  note "the root package now depends on pgx:"
  go list -deps . | grep 'jackc/pgx' | sed 's/^/    /'
fi

echo "== core packages are stdlib-only =="
# A third-party import is one whose FIRST path element carries a dot — a
# domain. Matching a dot anywhere flags the standard library's own internals:
# crypto/rand pulls in crypto/internal/entropy/v1.0.0, whose dot is a version
# rather than a host, and the gate reported the standard library as a
# third-party dependency.
outsiders() { go list -deps "$1" 2>/dev/null | grep -v '^github.com/gsoultan/storm' | grep -E '^[^/]+\.[^/]+/'; }
# The list is DERIVED from the tree, not written down. It used to be a list,
# with a comment warning that leaving a new back end off it is how the rule
# quietly stops applying to the half of compile/ that grew after it — and
# compile/oracle and compile/oraddl were then left off it. The two exemptions
# are the pgx adapter and the introspection that reads pg_catalog through it,
# the same two the driver check above allows.
for p in $(go list ./schema/... ./compile/... ./codegen/... ./runtime/... 2>/dev/null); do
  case "$p" in
    */runtime/pgxdrv|*/schema/pg) continue ;;
  esac
  if outsiders "$p" | grep -q .; then
    note "$p has a third-party dependency:"
    outsiders "$p" | sed 's/^/    /'
  fi
done

echo "== no dialect branch at run time =="
# The dialect is chosen once, where a build starts — codegen's decodersFor and
# loweringFor, migrate's ddlFor, the CLI's flag — and everything downstream is
# handed the result. What makes that a structural fact rather than a habit is
# that runtime/ cannot reach the code that chooses: it imports nothing of
# storm's outside itself, so no generated read can ask which dialect it is.
reach=$(go list -deps ./runtime/... 2>/dev/null | grep '^github.com/gsoultan/storm' \
  | grep -vE '^github.com/gsoultan/storm/runtime(/|$)' || true)
if [ -n "$reach" ]; then
  note "runtime/ reaches build-time packages:"
  printf '%s\n' "$reach" | sed 's/^/    /'
fi

echo "== the core does not reach the tool =="
# tool/discover parses adopters' source and tool/bootstrap writes a main; both
# are the CLI's business. Nothing a generated package links may depend on
# either, or on cmd/.
reach=$(go list -deps ./schema/... ./compile/... ./codegen/... ./runtime/... 2>/dev/null \
  | grep -E '^github.com/gsoultan/storm/(tool|cmd)(/|$)' || true)
if [ -n "$reach" ]; then
  note "the core reaches the tool:"
  printf '%s\n' "$reach" | sed 's/^/    /'
fi

echo "== one type per file, ten files per folder, unique clauses, no aliases =="
# The rules about DECLARATIONS, which need the AST rather than grep. Where the
# tree had drifted from one, internal/archcheck holds it as a ratchet: no worse
# than the numbers recorded there, and lowered whenever it gets better.
if ! out=$(go run ./internal/archcheck 2>&1); then
  note "the structural rules are broken:"
  printf '%s\n' "$out" | grep -v '^exit status' | sed 's/^/  /'
fi

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

# examples/orders is NOT in the list above, because regenerating it needs a
# server and this script needs none. What it does not need a server for is the
# question that matters: does the code checked in still COMPILE against the
# runtime it is checked in beside? CI regenerates before it builds, so CI has
# never once compiled the committed snapshot — and at v1.0.0 the snapshot had
# been dead for two releases (`runtime.MaskKey` became a named type; the join
# loaders moved a field behind `.Row`). Anyone cloning the repository and
# building the worked example — the on-ramp the README points at — got six
# type errors. The same shape as the vet gap above, one directory over.
echo "== the committed example compiles, without regenerating it =="
if ! (cd examples/orders && go build ./... 2>&1 | sed 's/^/    /'); then
  note "examples/orders does not build as committed — regenerate it (make example) and commit the result"
fi

# Minimal version selection resolves an adopter's build to the HIGHEST
# requirement in the graph, so a module here that asks for an OLDER version of
# something the root also requires is a module whose tests run a dependency the
# adopter's build does not. storm shipped exactly that: go.mod said pgx 5.10
# while the adopter's build resolved to 5.11, and pgxdrv is the one package
# where a driver change is invisible at compile time and wrong at run time.
#
# Fixing the root left examples/orders behind, where the SAME drift broke
# scripts/check/explain.sh — silently in CI, which had a warm enough module
# cache to resolve it anyway. Hence a check rather than a habit.
echo "== every module agrees with the root on shared dependencies =="
root_reqs="$(mktemp)"
go mod edit -json | python3 -c '
import json,sys
d=json.load(sys.stdin)
for r in d.get("Require") or []:
    if not r.get("Indirect"):
        print(r["Path"], r["Version"])
' > "$root_reqs"
while IFS= read -r mod; do
  [ "$mod" = "./go.mod" ] && continue
  case "$mod" in */testdata/*) continue ;; esac
  (cd "$(dirname "$mod")" && go mod edit -json) | python3 -c '
import json,sys
d=json.load(sys.stdin)
for r in d.get("Require") or []:
    if not r.get("Indirect"):
        print(r["Path"], r["Version"])
' | while read -r path ver; do
    want="$(awk -v p="$path" '$1==p{print $2}' "$root_reqs")"
    if [ -n "$want" ] && [ "$want" != "$ver" ]; then
      note "$mod requires $path $ver; the root requires $want — minimal version selection gives an adopter $want, so this module tests something nobody builds"
    fi
  done
done < <(find . -name go.mod -not -path "*/testdata/*")
rm -f "$root_reqs"

echo "== gofmt =="
if [ -n "$(gofmt -l . 2>/dev/null)" ]; then
  note "not gofmt'd:"; gofmt -l . | sed 's/^/    /'
fi

if [ "$fail" -eq 0 ]; then echo "OK: all boundary checks pass"; else echo "FAILED"; fi
exit "$fail"
