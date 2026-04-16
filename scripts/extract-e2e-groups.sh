#!/usr/bin/env bash
# Extracts second-level Describe block names from E2E test files and distributes
# them round-robin into N parallel CI groups. Outputs a JSON object suitable for
# GitHub Actions matrix strategy: {"include":[{"name":"group-1","focus":"..."},...]}.
set -euo pipefail

NUM_GROUPS="${1:-4}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
TEST_DIR="$SCRIPT_DIR/../test/e2e"

tests=()
while IFS= read -r name; do
  tests+=("$name")
done < <(grep -h $'^\tDescribe("' "$TEST_DIR"/*_test.go \
  | sed -E 's/.*Describe\("([^"]+)".*/\1/' \
  | sort)

if [[ ${#tests[@]} -eq 0 ]]; then
  echo "Error: no test names found in $TEST_DIR" >&2
  exit 1
fi

declare -a groups
for ((i = 0; i < NUM_GROUPS; i++)); do
  groups[$i]=""
done

for ((i = 0; i < ${#tests[@]}; i++)); do
  g=$((i % NUM_GROUPS))
  if [[ -n "${groups[$g]}" ]]; then
    groups[$g]="${groups[$g]}|${tests[$i]}"
  else
    groups[$g]="${tests[$i]}"
  fi
done

json='{"include":['
first=true
for ((i = 0; i < NUM_GROUPS; i++)); do
  [[ -z "${groups[$i]:-}" ]] && continue
  $first && first=false || json+=','
  json+="{\"name\":\"group-$((i + 1))\",\"focus\":\"${groups[$i]}\"}"
done
json+=']}'

echo "$json"
