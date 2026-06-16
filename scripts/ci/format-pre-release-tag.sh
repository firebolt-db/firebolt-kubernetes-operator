#!/usr/bin/env bash
# Format a pre-release tag: {next}-pre.0.{utc-datetime}.{12-char-sha}
set -euo pipefail

version="${1:?version required}"
sha="${2:-${GITHUB_SHA:?GITHUB_SHA required}}"

datetime="$(date -u +%Y%m%d%H%M%S)"
short_sha="$(printf '%s' "$sha" | cut -c1-12)"

printf '%s-pre.0.%s.%s\n' "$version" "$datetime" "$short_sha"
