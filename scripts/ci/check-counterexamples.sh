#!/usr/bin/env bash
# Assert every naive config still produces the violation it was written to produce.
#
# A green TLC run is only evidence if the run *could* have gone red. An invariant
# that no reachable state can violate passes forever, including after the guard it
# was written to protect is deleted — the check keeps reporting success while it
# has stopped checking anything. Each row below removes one shipped guard and
# names the invariant that must then fail; if it stops failing, either the guard
# was weakened somewhere else or the model no longer expresses the hazard.
#
# Usage:
#   scripts/ci/check-counterexamples.sh [<tlc-jar>]
#
# Defaults to bin/tla2tools.jar, so it runs locally the same way it runs in CI.
set -euo pipefail

MANIFEST="formal/counterexamples.tsv"
TLC_JAR="${1:-bin/tla2tools.jar}"

if [ ! -f "$MANIFEST" ]; then
  echo "ERROR: $MANIFEST not found (run from the repository root)" >&2
  exit 1
fi
if [ ! -f "$TLC_JAR" ]; then
  echo "ERROR: $TLC_JAR not found; run \`make tla2tools\` first" >&2
  exit 1
fi

# No `grep -q` on a pipeline anywhere below. Under `set -o pipefail`, grep -q
# exits at the first match, the producer dies with SIGPIPE, and the pipeline
# reports 141 — a "match" would read as a failure. Every grep here reads a FILE
# to completion, which has no such edge. Same reasoning as check-model-scope.sh.

fail=0
listed=""

# The current row's TLC log; the trap covers an interrupt mid-row.
log=""
trap '[ -n "$log" ] && rm -f "$log"' EXIT

while IFS="$(printf '\t')" read -r cfg spec expect; do
  case "$cfg" in '' | \#*) continue ;; esac
  listed="$listed $cfg"

  # A row with a missing column must fail loudly, not vacuously: grep -F ""
  # matches every log, so an empty expectation would turn the row into a check
  # that can never go red — the exact failure mode this script exists to catch.
  if [ -z "$spec" ] || [ -z "$expect" ]; then
    echo "ERROR: $MANIFEST row \"$cfg\" is missing its spec or expectation column" >&2
    echo "       (want <cfg><TAB><spec><TAB><violation line>)." >&2
    fail=1
    continue
  fi

  for f in "formal/$cfg" "formal/$spec"; do
    if [ ! -f "$f" ]; then
      echo "ERROR: $MANIFEST names $f, which does not exist." >&2
      fail=1
      continue 2
    fi
  done

  echo "counterexample: $cfg"

  # TLC's own violation line is the assertion, not merely a non-zero exit: TLC
  # exits non-zero on a parse error, an unassigned constant and OOM too, any of
  # which would otherwise make a broken spec look like a passing counterexample.
  log="$(mktemp "${TMPDIR:-/tmp}/counterexample.XXXXXX")"
  java -cp "$TLC_JAR" tlc2.TLC -workers auto -nowarning \
    -config "formal/$cfg" "formal/$spec" >"$log" 2>&1 || true

  if grep -qF "$expect" "$log"; then
    echo "  OK: still reports \"$expect\""
    rm -f "$log"
    continue
  fi

  echo "ERROR: $cfg no longer produces \"$expect\"." >&2
  echo "       Either the shipped guard was weakened, or the model stopped expressing" >&2
  echo "       the hazard. Do not relax the expectation without establishing which." >&2
  tail -40 "$log" >&2
  rm -f "$log"
  fail=1
done <"$MANIFEST"

if [ -z "$listed" ]; then
  echo "ERROR: $MANIFEST lists no counterexamples at all." >&2
  exit 1
fi

# A naive config nothing runs is worse than no config: it looks like coverage in
# the directory listing and proves nothing. Every one of them must be in the table.
for path in formal/*Naive*.cfg; do
  base="$(basename "$path")"
  case " $listed " in
  *" $base "*) continue ;;
  esac
  echo "ERROR: $path is not listed in $MANIFEST, so nothing ever runs it." >&2
  fail=1
done

if [ "$fail" -ne 0 ]; then
  exit 1
fi
echo "OK: every naive config still produces its pinned violation"
