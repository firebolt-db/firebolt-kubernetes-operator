#!/usr/bin/env bash
# Download a third-party binary pinned in scripts/ci/pinned-tools.tsv and
# verify its SHA-256 before the caller is allowed to touch it.
#
# Usage:
#   scripts/ci/fetch-verified.sh <name> <dest>
#
# <name> is a row key in pinned-tools.tsv; the row is selected by name plus the
# running architecture. <dest> is written only after the digest matches, so a
# caller that follows this with `chmod +x` / `tar -xz` can never execute or
# unpack unverified bytes. On mismatch the download is discarded and the script
# exits non-zero.
#
# An existing <dest> is verified rather than trusted: a matching digest short-
# circuits the download, a mismatching one is replaced. That makes the script
# safe to run unconditionally over a restored CI cache or a stale local bin/.
#
# The manifest owns the version -- callers pass a name, never a URL.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFEST="${SCRIPT_DIR}/pinned-tools.tsv"

die() {
  echo "fetch-verified: $*" 1>&2
  exit 1
}

[ "$#" -eq 2 ] || die "usage: $0 <name> <dest>"
name="$1"
dest="$2"

[ -f "$MANIFEST" ] || die "manifest not found: ${MANIFEST}"

case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac

# Rows are (name, arch, sha256, url); "any" matches every architecture.
matches="$(awk -v name="$name" -v arch="$arch" \
  '$0 !~ /^[[:space:]]*(#|$)/ && $1 == name && ($2 == arch || $2 == "any") { print $3, $4 }' \
  "$MANIFEST")"

[ -n "$matches" ] || die "no pinned entry for '${name}' on ${arch} in ${MANIFEST}"
[ "$(printf '%s\n' "$matches" | wc -l)" -eq 1 ] \
  || die "ambiguous pinned entries for '${name}' on ${arch} in ${MANIFEST}"

read -r expected_sha url <<<"$matches"

[ "${#expected_sha}" -eq 64 ] || die "malformed digest for '${name}': ${expected_sha}"

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | cut -d' ' -f1
  else
    die "neither sha256sum nor shasum available to verify ${name}"
  fi
}

if [ -f "$dest" ] && [ "$(sha256_of "$dest")" = "$expected_sha" ]; then
  # Advance mtime so Make rules that depend on the pin manifest (e.g.
  # $(TLA2TOOLS): pinned-tools.tsv) treat the target as up to date after a
  # digest hit. Leaving the file untouched would re-run this recipe forever
  # whenever the manifest is newer than an already-correct jar.
  touch "$dest"
  echo "fetch-verified: ${dest} already matches the pinned ${name} digest"
  exit 0
fi

# Stage next to the destination so the final move is a rename on the same
# filesystem: no window where <dest> exists holding unverified content.
dest_dir="$(dirname "$dest")"
mkdir -p "$dest_dir"
tmp="$(mktemp "${dest}.download.XXXXXX")"
cleanup() { rm -f "$tmp"; }
trap cleanup EXIT

echo "fetch-verified: downloading ${name} (${arch}) from ${url}"
curl --fail --location --silent --show-error \
  --retry 3 --retry-delay 2 --retry-connrefused \
  --output "$tmp" "$url" \
  || die "download failed: ${url}"

actual_sha="$(sha256_of "$tmp")"

if [ "$actual_sha" != "$expected_sha" ]; then
  die "SHA-256 mismatch for ${name} from ${url}
  expected: ${expected_sha}
  actual:   ${actual_sha}
Refusing to install. Either the pin in ${MANIFEST} is stale or the upstream
artifact was replaced -- confirm which before updating the digest."
fi

chmod 0644 "$tmp"
mv "$tmp" "$dest"
trap - EXIT
echo "fetch-verified: ${name} verified (sha256=${expected_sha}) -> ${dest}"
