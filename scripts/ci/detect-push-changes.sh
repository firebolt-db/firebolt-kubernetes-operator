#!/usr/bin/env bash
# Emit GitHub Actions outputs describing which areas changed in a push.
set -euo pipefail

# before_sha may legitimately be empty (branch creation, workflow_dispatch), so
# it is accepted unset/empty and handled below — do not use ${1:?}.
before_sha="${1-}"
after_sha="${2-}"
output_file="${3:-${GITHUB_OUTPUT:?GITHUB_OUTPUT required}}"

app_paths=false
helm_paths=false

if [ -z "$before_sha" ] || [ "$before_sha" = "0000000000000000000000000000000000000000" ]; then
  app_paths=true
  helm_paths=true
else
  # app_paths drives the app pre-release build: any change that is not purely a
  # chart or top-level README edit counts, including .github/ (workflow/CI
  # changes still build a pre-release image).
  if git diff --name-only "$before_sha" "$after_sha" -- ':!helm/' ':!README.md' | grep -q .; then
    app_paths=true
  fi
  if git diff --name-only "$before_sha" "$after_sha" -- 'helm/' | grep -q .; then
    helm_paths=true
  fi
fi

{
  echo "app_paths=$app_paths"
  echo "helm_paths=$helm_paths"
} >>"$output_file"

echo "app_paths=$app_paths helm_paths=$helm_paths"
