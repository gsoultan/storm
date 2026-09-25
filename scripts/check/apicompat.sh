#!/usr/bin/env bash
# The stability promise, checked rather than asserted.
#
# docs/STABILITY.md names the packages an adopter's own code calls and binds
# them to semver from v1.0.0. This compares their exported API against the
# latest v1 tag with apidiff and fails on any INCOMPATIBLE change: additions
# pass, removals and re-typings do not.
#
# It exists because the promise was broken on main and nothing noticed. A fifth
# method on runtime.Rows is a method every adopter-written Rows stops compiling
# without, and it sat there, unreleased, until apidiff was run by hand. A
# promise with no check is a claim.
set -euo pipefail
cd "$(dirname "$0")/../.."

# Pinned, like every tool a gate depends on: an apidiff that classified a change
# differently would change what this gate means without a diff here.
APIDIFF=golang.org/x/exp/cmd/apidiff@v0.0.0-20260908205506-85c1c2202aba

# The covered packages, relative to the module root; "." is storm itself. Adding
# one is a promise and removing one withdraws a promise, which is a major. Keep
# this list and docs/STABILITY.md's in step.
COVERED=(. runtime runtime/pgxdrv runtime/mydrv runtime/msdrv migrate)

# Reviewed exceptions: apidiff lines, exactly as printed, that may be
# incompatible anyway. Empty is the normal state. An entry here is a decision
# somebody defends in review, with its reason beside it — which is the point.
EXCEPTIONS=()

# The baseline is a tag, and CI's shallow checkout has none. Fetch only when
# there are none here, and pass --depth only to a clone that is ALREADY
# shallow: in a full clone, --depth writes .git/shallow and grafts history off
# at the tags, so `git log` stops at v1.1.0. The first version of this script
# did exactly that to the clone it was written in.
if [ -z "$(git tag --list 'v1.*')" ]; then
  if [ "$(git rev-parse --is-shallow-repository)" = true ]; then
    git fetch -q --depth=1 origin 'refs/tags/v1.*:refs/tags/v1.*'
  else
    git fetch -q origin 'refs/tags/v1.*:refs/tags/v1.*'
  fi
fi
base=$(git tag --list 'v1.*' --sort=-v:refname | head -1)
if [ -z "$base" ]; then
  echo "no v1.* tag to compare against" >&2
  exit 1
fi

tmp=$(mktemp -d)
cleanup() {
  git worktree remove --force "$tmp/base" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT

GOBIN="$tmp/bin" go install "$APIDIFF"
git worktree add -q --detach "$tmp/base" "$base"
(cd "$tmp/base" && "$tmp/bin/apidiff" -m -w "$tmp/base.api" github.com/gsoultan/storm)
"$tmp/bin/apidiff" -m -w "$tmp/head.api" github.com/gsoultan/storm
"$tmp/bin/apidiff" -m -incompatible "$tmp/base.api" "$tmp/head.api" >"$tmp/all.txt" 2>&1

echo "== the covered API is compatible with $base =="
fail=0
while IFS= read -r line; do
  case "$line" in "- "*) ;; *) continue ;; esac
  # "- ./runtime/mydrv.(*Pool).Begin: changed …" names a package by its path;
  # a change in the root package has no path at all: "- SQLExec: removed".
  rest=${line#- }
  pkg=.
  if [[ $rest == ./* ]]; then
    pkg=${rest#./}
    pkg=${pkg%%[.:]*}
  fi
  covered=0
  for c in "${COVERED[@]}"; do
    [ "$c" = "$pkg" ] && covered=1
  done
  [ "$covered" = 1 ] || continue
  for e in ${EXCEPTIONS[@]+"${EXCEPTIONS[@]}"}; do
    if [ "$e" = "$line" ]; then
      echo "  allowed: $line"
      continue 2
    fi
  done
  echo "  $line"
  fail=1
done <"$tmp/all.txt"

if [ "$fail" = 1 ]; then
  echo "  incompatible with $base in a package docs/STABILITY.md covers: a minor may not" >&2
  echo "  do this. Add instead of changing, deprecate for a minor, or make the case" >&2
  echo "  for an exception in this script, in review" >&2
  exit 1
fi
echo "OK"
