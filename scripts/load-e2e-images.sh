#!/usr/bin/env bash
set -euo pipefail

# Load required Docker images into Kind cluster for e2e testing.
# All image values come from config/images/defaults.<variant>.env (single source
# of truth, where <variant> is IMAGE_VARIANT, defaulting to "dev").
#
# IMAGE_VARIANT MUST match the build tag of the operator binary and the test
# binary that will run after this script — otherwise the suite asks Kind for
# images this script never loaded. The Makefile threads IMAGE_VARIANT through
# `build`, `prepare-test-e2e`, and `test-e2e` to keep the two in sync.
#
# Images are pulled (if missing locally) and loaded into Kind in parallel.
# Parallelism can be tuned via E2E_LOAD_PARALLELISM (default: 4). Each
# `kind load docker-image` invocation streams `docker save` into containerd
# on the control-plane node; these imports are independent and safe to run
# concurrently.
#
# Upgrade-target images: the E2E image-switch tests need a DIFFERENT tag
# string than the loaded one, but not different image content. Rather than
# publishing and loading a second image (which would double the engine /
# metadata disk footprint per kind node), the suite re-tags the already-
# loaded image inside containerd at startup. See
# test/e2e/e2e_suite_test.go for the re-tag step.

CLUSTER_NAME="${1:-operator-test-e2e}"
LOAD_PARALLELISM="${E2E_LOAD_PARALLELISM:-4}"
IMAGE_VARIANT="${IMAGE_VARIANT:-dev}"

case "${IMAGE_VARIANT}" in
    latest|dev) ;;
    *)
        echo "Error: unsupported IMAGE_VARIANT='${IMAGE_VARIANT}' (expected 'latest' or 'dev')." >&2
        exit 1
        ;;
esac

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEFAULTS_ENV="${SCRIPT_DIR}/../config/images/defaults.${IMAGE_VARIANT}.env"
if [ ! -f "${DEFAULTS_ENV}" ]; then
    echo "Error: defaults file not found at ${DEFAULTS_ENV}" >&2
    exit 1
fi
echo "Sourcing defaults from ${DEFAULTS_ENV} (IMAGE_VARIANT=${IMAGE_VARIANT})"
# shellcheck disable=SC1090
source "${DEFAULTS_ENV}"

# `kind load docker-image` creates multi-GB tarballs via `docker save` under
# $TMPDIR. The default /tmp is tmpfs on many Linux distros (notably Ubuntu
# 24.04+) and fills up quickly with concurrent loads. Redirect to a
# workspace-local disk-backed directory so we don't compete for tmpfs.
KIND_LOAD_TMPDIR="${KIND_LOAD_TMPDIR:-/var/tmp}"
mkdir -p "${KIND_LOAD_TMPDIR}"
KIND_LOAD_TMPDIR="$(cd "${KIND_LOAD_TMPDIR}" && pwd)"
export TMPDIR="${KIND_LOAD_TMPDIR}"
trap 'rm -rf "${KIND_LOAD_TMPDIR:?}"/images-tar* 2>/dev/null || true' EXIT

echo "=== Loading images into Kind cluster: ${CLUSTER_NAME} (parallelism=${LOAD_PARALLELISM}) ==="

if ! kind get nodes --name "${CLUSTER_NAME}" &>/dev/null; then
    echo "Error: Kind cluster '${CLUSTER_NAME}' does not exist."
    echo "Run 'make setup-kind' first."
    exit 1
fi

# NOTE: GHCR authentication should be done before running this script.
# In CI, use docker/login-action with ghcr.io.
# Locally, run: echo $GITHUB_TOKEN | docker login ghcr.io -u USERNAME --password-stdin

# Each entry encodes "image|policy". Policy is one of:
#   pull   — registry-backed, always re-pull. The dev variant's
#            ENGINE_TAG / METADATA_TAG are mutable `:dev` aliases, so a
#            stale local copy would silently make the suite validate an
#            old build of the alias. `docker pull` on a pinned release tag
#            is cheap (manifest check, no layer download), so applying the
#            same policy uniformly to the latest variant is fine.
#   local  — reserved for local-only images built outside any registry
#            (pulling them would 404). Currently unused: the operator runs
#            in-process during E2E, so no operator image needs loading
#            here. The Helm-based local-deploy path uses its own
#            `make kind-load-operator` target.
declare -a IMAGES=(
    "${ENGINE_IMAGE}:${ENGINE_TAG}|pull"
    "${METADATA_IMAGE}:${METADATA_TAG}|pull"
    "${POSTGRES_IMAGE}|pull"
    "${ENVOY_IMAGE}:${ENVOY_TAG}|pull"
    "${CURL_IMAGE}|pull"
)

load_one() {
    local entry="$1"
    local cluster="$2"
    local image="${entry%|*}"
    local policy="${entry##*|}"

    case "${policy}" in
        pull)
            echo ">>> [${image}] pulling (force, refresh mutable aliases)"
            docker pull "${image}"
            ;;
        local)
            echo ">>> [${image}] using local image (built outside any registry, no pull)"
            ;;
        *)
            echo "ERROR: unknown load policy '${policy}' for image '${image}'" >&2
            exit 1
            ;;
    esac

    echo ">>> [${image}] loading into Kind"
    kind load docker-image "${image}" --name "${cluster}"
    echo ">>> [${image}] done"
}
export -f load_one

# xargs -P runs up to N children concurrently; if any child exits non-zero,
# xargs continues the rest but returns non-zero itself, which (combined with
# `set -e` and `pipefail`) fails the script. Each child runs under
# `bash -eo pipefail` so intra-child failures propagate correctly.
printf '%s\n' "${IMAGES[@]}" \
    | xargs -n1 -P "${LOAD_PARALLELISM}" -I{} \
        bash -eo pipefail -c 'load_one "$1" "$2"' _ {} "${CLUSTER_NAME}"

echo ""
echo "=== Image loading complete ==="
echo ""
echo "Loaded images in Kind cluster:"
docker exec "${CLUSTER_NAME}-control-plane" crictl images 2>/dev/null | head -20 || true

# Pre-flight: ensure the engine image's architecture matches the kind node's
# architecture. Otherwise the engine binary will run via the kind node's
# user-mode emulator which (notably on Apple Silicon, where Rosetta is NOT
# propagated into nested kind containers) is qemu-x86_64. qemu-x86_64 lacks
# AVX2/BMI2/FMA, so x86-64-v3 binaries SIGILL during startup -- and the
# failure surfaces ~6 minutes later as an opaque startup-probe timeout.
echo ""
echo "=== Pre-flight: verify ${ENGINE_IMAGE}:${ENGINE_TAG} arch matches kind node ==="

ENGINE_REF="${ENGINE_IMAGE}:${ENGINE_TAG}"
image_arch=$(docker image inspect "${ENGINE_REF}" --format '{{.Architecture}}' 2>/dev/null || echo "unknown")
node_arch_kernel=$(docker exec "${CLUSTER_NAME}-control-plane" uname -m 2>/dev/null || echo "unknown")
case "${node_arch_kernel}" in
    x86_64)  node_arch=amd64 ;;
    aarch64) node_arch=arm64 ;;
    *)       node_arch="${node_arch_kernel}" ;;
esac

if [ "${image_arch}" != "${node_arch}" ]; then
    echo "ERROR: engine image arch '${image_arch}' does not match kind node arch '${node_arch}'." >&2
    echo "       Inside a kind node, foreign-arch binaries are run via user-mode emulation." >&2
    echo "       On Apple Silicon (arm64 host, amd64 image), kind falls back to qemu-x86_64," >&2
    echo "       which lacks AVX2/BMI2/FMA -- the engine binary will SIGILL during startup." >&2
    echo "       Fix: in ${DEFAULTS_ENV}, drop any '-amd64' suffix on ENGINE_TAG so Docker" >&2
    echo "       resolves a manifest list and pulls the native '${node_arch}' variant. Or run" >&2
    echo "       on a host whose arch matches the image." >&2
    exit 1
fi

echo ">>> Engine image arch '${image_arch}' matches kind node arch '${node_arch}'."
