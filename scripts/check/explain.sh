#!/usr/bin/env bash
# Does every statement storm's generated code can issue actually plan?
#
# Three things already look at storm's SQL and none of them answer this. The
# codegen unit tests prove the lowering is consistent with itself. The orders
# tests execute the statements they happen to call. The fuzzer covers the
# spliced predicates. What none of them reach is a declared aggregation or join
# that no test calls: its SQL is fixed at generate time, so it is either valid
# or it is a runtime error the first time a caller asks for it — and "no test
# calls it" is the normal state of a feature somebody declared last week.
#
# `storm explain` sends each of them through EXPLAIN (GENERIC_PLAN), which
# plans a parameterised statement without inventing bind values. An empty
# database is enough: this asserts VALIDITY. The seq-scan threshold needs
# statistics to mean anything and is left at its default, where the planner's
# row estimates cannot reach it.
#
# Run against examples/orders, and only there. It is the model carrying window
# functions, FILTER, HAVING, grouping sets and a CTE — the constructs where a
# lowering bug produces SQL that is well-formed to storm and rejected by
# PostgreSQL — and it is the only example that is its own module. examples/blog
# is part of the storm module, so discovery from it scans all of storm and
# hands Build three unrelated model packages at once (blog, benchmodel and
# testmodel, two of which declare a `User`). That is not a discovery bug and
# not a blog bug: storm's own repository is not one schema, and adopters have
# one model package.
#
# `storm lint` is NOT run here. The orders model deliberately declares no named
# plans — every relation already gets one, and the model says so — so lint
# would report success without checking anything, which is the failure mode
# this repository is careful about elsewhere. The round-trip budget is gated by
# TestPlanCosts, which asserts the exact count for each of the fixture's three
# plans and fails CI through `go test ./...` if a planner change moves one.
set -euo pipefail
cd "$(dirname "$0")/../.."

DSN="${STORM_DSN:-}"
if [ -z "$DSN" ]; then
  echo "STORM_DSN is not set; skipping the explain check"
  exit 0
fi

# FAIL rather than skip, for the same reason the MySQL check does: the caller
# asked for this by setting STORM_DSN, so being unable to run it is an error.
if ! command -v psql >/dev/null 2>&1; then
  echo "STORM_DSN is set but the psql client is not installed" >&2
  exit 1
fi

NS=storm_explain
SQL="$(mktemp)"
trap 'rm -f "$SQL"; psql "$DSN" -q -c "DROP SCHEMA IF EXISTS $NS CASCADE" >/dev/null 2>&1 || true' EXIT

# NOTICEs from a re-created schema are not findings, and burying a real error
# under seven "drop cascades to" lines is how a failing gate reads as passing.
export PGOPTIONS='-c client_min_messages=warning'

ROOT="$PWD"
cd examples/orders

# The model's own DDL, applied to a scratch namespace. storm never applies DDL
# itself; this stands in for the migration runner a real project would use, and
# the namespace is dropped on the way out either way.
go run "$ROOT/cmd/storm" ddl > "$SQL"
psql "$DSN" -q -v ON_ERROR_STOP=1 \
  -c "DROP SCHEMA IF EXISTS $NS CASCADE; CREATE SCHEMA $NS"
psql "$DSN" -q -v ON_ERROR_STOP=1 -c "SET search_path TO $NS" -f "$SQL"

go run "$ROOT/cmd/storm" explain -schema "$NS"
