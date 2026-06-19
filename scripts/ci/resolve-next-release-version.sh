#!/usr/bin/env bash
# Resolve the next semver for a release-please component.
#
# Prefer the version proposed on an open release PR (the `prs` JSON emitted by
# release-please-action). Fall back to the committed manifest entry when no
# release PR is open yet.
#
# The `prs` array holds release-please PullRequest objects, which expose
# `headBranchName` and `title` but NOT a per-package `path`/`version`. Each
# component's open PR is identified by its branch suffix
# (release-please--branches--<base>--components--<component>) and its title
# (chore(release): <component> <version>); the next version is the trailing
# semver in that title.
set -euo pipefail

manifest_path="${1:?manifest path required}"
prs_json="${2:-[]}"

if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required" >&2
  exit 1
fi

# Map manifest path -> release-please component (see release-please-config.json).
case "$manifest_path" in
  .) component="firebolt-operator" ;;
  helm/firebolt-operator) component="firebolt-operator-chart" ;;
  helm/firebolt-operator-crds) component="firebolt-operator-crds-chart" ;;
  *) component="" ;;
esac

pr_version=""
if [ -n "$component" ]; then
  pr_version="$(printf '%s' "$prs_json" | jq -r --arg comp "$component" '
    [ .[]
      | select(
          ((.headBranchName // "") | endswith("--components--" + $comp))
          or ((.title // "") | test("^chore\\(release\\):\\s+" + $comp + "\\s+[0-9]"))
        )
      | ((.title // "") | capture("(?<v>[0-9]+\\.[0-9]+\\.[0-9]+(-[0-9A-Za-z.-]+)?)\\s*$")? | .v)
    ]
    | map(select(. != null))
    | first // empty
  ' 2>/dev/null || true)"
fi

if [ -n "$pr_version" ]; then
  printf '%s\n' "$pr_version"
  exit 0
fi

jq -r --arg path "$manifest_path" '.[$path] // empty' .release-please-manifest.json
