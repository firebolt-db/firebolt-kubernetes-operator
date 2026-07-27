#!/usr/bin/env bash
# Ask that a change to a model-bound function moves its spec and fixture too.
#
# This is a review prompt, not a proof. The state-cover suite already fails when
# the reconciler and the model disagree *within the modelled state space*; what
# it cannot notice is a change that leaves the model behind entirely — a new
# phase, a new gate, a new transition the spec never represented. Nothing can
# verify that automatically, so this makes it visible instead of silent.
#
# Usage:
#   scripts/ci/check-model-scope.sh [<base-ref> [<head-ref>]]
#
# Defaults to origin/main...HEAD, so it runs locally the same way it runs in CI.
#
# Escape hatch: a change that genuinely does not affect the model says so with
# `Model-scope-ok: <reason>` in a commit message or the PR body (the workflow
# passes the PR body in via MODEL_SCOPE_TEXT).
set -euo pipefail

MANIFEST="formal/model-scope.tsv"
BASE_REF="${1:-origin/main}"
HEAD_REF="${2:-HEAD}"

if [ ! -f "$MANIFEST" ]; then
  echo "ERROR: $MANIFEST not found (run from the repository root)" >&2
  exit 1
fi

# Three dots: compare against the merge base, so commits that landed on main
# after this branch started are not mistaken for part of the change.
RANGE="${BASE_REF}...${HEAD_REF}"

changed="$(git diff --name-only "$RANGE")"
commit_msgs="$(git log --format='%B' "${BASE_REF}..${HEAD_REF}" || true)"
allow_text="${commit_msgs}
${MODEL_SCOPE_TEXT:-}"

# No `grep -q` anywhere below. Under `set -o pipefail`, grep -q exits at the
# first match, the producer dies with SIGPIPE, and the pipeline reports 141 —
# a "match" would read as a failure and vice versa. Plain bash matching on a
# captured string has no such edge, so every check here is pipe-free.
changed_contains() {
  [[ $'\n'${changed}$'\n' == *$'\n'"$1"$'\n'* ]]
}

# Does any hunk header for this file name the given function? Git puts the
# enclosing declaration in the hunk header, which is what makes this
# function-level rather than file-level. Matches both plain functions
# ("func computeCreating(") and methods ("func (r *X) computePhase(").
hunk_headers_name_func() {
  local headers="$1" fn="$2"
  [[ $headers == *"func ${fn}("* || $headers == *") ${fn}("* ]]
}

violations=0
reported_specs=""

while IFS="$(printf '\t')" read -r func file spec fixture; do
  case "$func" in '' | \#*) continue ;; esac

  # grep here reads its input to completion (no -q), so there is no SIGPIPE.
  headers="$(git diff -U0 "$RANGE" -- "$file" | grep '^@@' || true)"
  if ! hunk_headers_name_func "$headers" "$func"; then
    continue
  fi

  echo "model-bound function touched: ${func} (${file}) -> ${spec}"

  if changed_contains "$spec" && changed_contains "$fixture"; then
    echo "  OK: ${spec} and its fixture moved with it"
    continue
  fi

  # Report each spec once even when several of its functions were touched.
  case " $reported_specs " in *" $spec "*) continue ;; esac
  reported_specs="$reported_specs $spec"

  changed_contains "$spec" && spec_state="changed" || spec_state="UNCHANGED"
  changed_contains "$fixture" && fixture_state="changed" || fixture_state="UNCHANGED"
  echo "  ${spec}: ${spec_state}"
  echo "  ${fixture}: ${fixture_state}"
  violations=$((violations + 1))
done <"$MANIFEST"

if [ "$violations" -eq 0 ]; then
  echo "OK: no model-bound function changed without its spec"
  exit 0
fi

if [[ ${allow_text,,} =~ (^|$'\n')[[:space:]]*model-scope-ok: ]]; then
  echo
  echo "Model-scope-ok found; treating the unchanged model as deliberate:"
  grep -i 'model-scope-ok:' <<<"$allow_text" || true
  exit 0
fi

cat >&2 <<'MSG'

ERROR: a model-bound function changed but its TLA+ spec did not.

The state-cover suite only proves the reconciler agrees with the model inside
the state space the model represents. A change that adds a phase, a gate or a
transition the spec never had passes every formal target while the model
quietly stops describing what ships.

Either:
  * update the spec and run `make formal-gen` so the fixture moves with it, or
  * say why the model is unaffected: put a line

        Model-scope-ok: <reason>

    in a commit message or the PR body. A pure refactor, a comment, a log line
    or an error-message change is a perfectly good reason.

If the change belongs to a machine that has no spec at all, see the list of
unmodelled machines at the top of formal/model-scope.tsv.
MSG
exit 1
