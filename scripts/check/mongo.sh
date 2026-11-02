#!/usr/bin/env bash
# Does storm's MongoDB back end work against MongoDB?
#
# Everything a document back end promises is observable only against a server:
# that the validators compile/mgddl writes are accepted, that the commands
# compile/mongo lowers run and mean what the SQL targets mean, and that
# runtime/mgdrv speaks the wire. So this gate runs the three suites that need
# one — the driver, its authentication, and the generated example — and then
# holds the driver to a coverage floor measured on a run that did NOT skip.
#
# Needs a REPLICA SET (transactions refuse a standalone) in STORM_MONGO_URI,
# and a server with authentication on in STORM_MONGO_AUTH_URI:
#
#   docker run -d -p 27017:27017 mongo:7 --replSet rs0
#   docker exec <id> mongosh --eval 'rs.initiate({_id:"rs0",members:[{_id:0,host:"localhost:27017"}]})'
#   docker run -d -p 27018:27017 -e MONGO_INITDB_ROOT_USERNAME=storm \
#     -e MONGO_INITDB_ROOT_PASSWORD=secret mongo:7
#   STORM_MONGO_URI=mongodb://localhost:27017/storm_test \
#   STORM_MONGO_AUTH_URI='mongodb://storm:secret@localhost:27018/storm_test?authSource=admin' \
#     scripts/check/mongo.sh
set -uo pipefail
cd "$(dirname "$0")/../.."

if [ -z "${STORM_MONGO_URI:-}" ] || [ -z "${STORM_MONGO_AUTH_URI:-}" ]; then
  if [ -n "${CI:-}" ]; then
    echo "STORM_MONGO_URI or STORM_MONGO_AUTH_URI unset in CI: this gate cannot pass by not running" >&2
    exit 1
  fi
  echo "STORM_MONGO_URI / STORM_MONGO_AUTH_URI unset; skipping the MongoDB check"
  exit 0
fi

fail=0
note() { printf '  %s\n' "$1"; fail=1; }

prof=$(mktemp)
out=$(mktemp)
trap 'rm -f "$prof" "$out"' EXIT

echo "== runtime/mgdrv against a replica set, and SCRAM against an authenticating server =="
echo "== the generated example against MongoDB =="
if ! go test -count=1 -race -v -coverpkg=./runtime/mgdrv -coverprofile="$prof" \
  ./runtime/mgdrv ./examples/library >"$out" 2>&1; then
  note "a MongoDB suite failed:"
  grep -E "^(\s*--- FAIL|\s+\S+_test.go:|panic:)" "$out" | head -40 | sed 's/^/    /'
fi

# `go test` says ok for a package whose tests all skipped. Count the ones that
# had to talk to a server instead.
for want in TestLiveRoundTrip TestLiveTransactions TestLiveSCRAM TestTour; do
  if ! grep -q -- "--- PASS: $want " "$out"; then
    note "$want did not PASS — it skipped or never ran, so this gate measured nothing"
  fi
done

floor=85
got=$(go tool cover -func="$prof" 2>/dev/null | awk '/^total:/ {sub("%","",$3); print $3}')
if [ -z "$got" ] || awk -v g="$got" -v f="$floor" 'BEGIN{exit !(g < f)}'; then
  note "runtime/mgdrv coverage ${got:-?}% is below its floor of $floor%"
else
  echo "  runtime/mgdrv: $got% (floor $floor%)"
fi

if [ "$fail" -ne 0 ]; then
  echo "FAILED"
  exit 1
fi
echo "ok"
