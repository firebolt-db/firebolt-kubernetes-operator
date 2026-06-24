#!/usr/bin/env bash
set -euo pipefail

# Publish required workload images to the local Docker registry that kind
# nodes mirror through (started by scripts/setup-local-registry.sh, wired
# into containerd by scripts/setup-kind-cluster.sh).
#
# All image values come from config/images/defaults.<variant>.env (single source
# of truth, where <variant> is IMAGE_VARIANT, defaulting to "latest").
#
# IMAGE_VARIANT MUST match the build tag of the operator binary and the test
# binary that will run after this script — otherwise the suite asks Kind for
# images this script never published. The Makefile threads IMAGE_VARIANT
# through `build`, `prepare-test-e2e`, and `test-e2e` to keep the two in sync.
#
# Why a registry, not `kind load docker-image`:
# `kind load` copies each image into every kind node's containerd snapshotter,
# which on a multi-node cluster (we run 1 control-plane + 1+ workers) means
# the engine image (~5 GB) is duplicated per node and Docker Desktop's
# default ~64 GB VM disk fills up. The registry stores each image once;
# kind nodes pull only the layers they actually run.
# https://kind.sigs.k8s.io/docs/user/local-registry/
#
# Upgrade-target images: the E2E image-switch tests need a DIFFERENT tag
# string than the loaded one, but not different image content. Rather than
# publishing a separately-built upgrade image, this script pushes the same
# engine / metadata content under both ${TAG} and ${TAG}-uptest. A second
# tag in an OCI registry is just a manifest pointing at existing blobs, so
# there is no extra layer transfer or disk usage. Keep
# `upgradeTagSuffix = "-uptest"` in test/e2e/e2e_suite_test.go in sync with
# UPGRADE_TAG_SUFFIX below.

CLUSTER_NAME="${1:-operator-test-e2e}"
LOAD_PARALLELISM="${E2E_LOAD_PARALLELISM:-4}"
IMAGE_VARIANT="${IMAGE_VARIANT:-latest}"

# Local registry endpoints. The host-side endpoint is what `docker push`
# talks to; the in-cluster endpoint is what containerd resolves through the
# /etc/containerd/certs.d hosts.toml files written by setup-kind-cluster.sh.
REGISTRY_NAME="${REGISTRY_NAME:-kind-registry}"
REGISTRY_PORT="${REGISTRY_PORT:-5001}"
REGISTRY_HOST_ENDPOINT="localhost:${REGISTRY_PORT}"

# Suffix for the synthetic upgrade-target tag. Must match
# test/e2e/e2e_suite_test.go's `upgradeTagSuffix`.
UPGRADE_TAG_SUFFIX="${UPGRADE_TAG_SUFFIX:--uptest}"

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

echo "=== Publishing images to local registry ${REGISTRY_HOST_ENDPOINT} (parallelism=${LOAD_PARALLELISM}) ==="

if ! kind get nodes --name "${CLUSTER_NAME}" &>/dev/null; then
    echo "Error: Kind cluster '${CLUSTER_NAME}' does not exist."
    echo "Run 'make setup-kind' first."
    exit 1
fi

# Pre-flight: registry must be up and reachable on the host. Provides a
# clearer error than `docker push` failing with "connection refused".
"${SCRIPT_DIR}/setup-local-registry.sh" >/dev/null

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

# Translate a Docker image reference (possibly without explicit registry
# host, possibly without organisation) to the path the kind-registry
# mirror expects. The transformation matches Docker's implicit image
# normalisation:
#   ghcr.io/firebolt-db/engine:dev   -> firebolt-db/engine:dev   (strip explicit host)
#   envoyproxy/envoy:v1.37.2         -> envoyproxy/envoy:v1.37.2 (org/name; keep)
#   postgres:16-alpine               -> library/postgres:16-alpine (official Docker Hub)
# The kind nodes' containerd hosts.toml maps both ghcr.io and docker.io to
# the local registry, so a single push under each upstream's path makes the
# image resolvable without changing the operator-baked image references.
to_registry_path() {
    local image="$1"
    local first_seg="${image%%/*}"
    if [[ "${image}" == */* ]] && \
       [[ "${first_seg}" == *"."* || "${first_seg}" == *":"* || "${first_seg}" == "localhost" ]]; then
        # Has explicit host: strip it.
        printf '%s\n' "${image#*/}"
    elif [[ "${image}" == */* ]]; then
        # Has org but no host (Docker Hub user/org image): keep as is.
        printf '%s\n' "${image}"
    else
        # Bare name (Docker Hub official image): prepend library/.
        printf '%s\n' "library/${image}"
    fi
}

publish_one() {
    local entry="$1"
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

    local repo_path
    repo_path=$(to_registry_path "${image}")
    local registry_ref="${REGISTRY_HOST_ENDPOINT}/${repo_path}"

    echo ">>> [${image}] tagging -> ${registry_ref}"
    docker tag "${image}" "${registry_ref}"

    echo ">>> [${image}] pushing -> ${registry_ref}"
    docker push "${registry_ref}"

    # Engine and metadata are referenced by the E2E image-switch specs
    # under a synthetic upgrade tag. Push the same content under that tag
    # so kind nodes can resolve it via the same mirror without storing a
    # second copy of the layers (OCI registries dedupe by digest).
    case "${image}" in
        "${ENGINE_IMAGE}:${ENGINE_TAG}"|"${METADATA_IMAGE}:${METADATA_TAG}")
            local tagless="${image%:*}"
            local original_tag="${image##*:}"
            local upgrade_tag="${original_tag}${UPGRADE_TAG_SUFFIX}"
            local upgrade_repo_path
            upgrade_repo_path=$(to_registry_path "${tagless}:${upgrade_tag}")
            local upgrade_ref="${REGISTRY_HOST_ENDPOINT}/${upgrade_repo_path}"
            echo ">>> [${image}] tagging -> ${upgrade_ref} (upgrade-target alias)"
            docker tag "${image}" "${upgrade_ref}"
            echo ">>> [${image}] pushing -> ${upgrade_ref}"
            docker push "${upgrade_ref}"
            # Drop the local Docker tag of the upgrade alias once pushed;
            # registry retains it.
            docker rmi "${upgrade_ref}" >/dev/null 2>&1 || true
            ;;
    esac

    # Free Docker's local content store on the host. Pulled image bytes
    # live in the registry now; keeping a second copy on the host's
    # overlay graph driver is what blew past 64 GB on Docker Desktop.
    # The source tag is removed first; the registry-side tag is removed
    # alongside it. Both `rmi`s tolerate "image is being used by stopped
    # container" / "no such image" because of `|| true`.
    docker rmi "${registry_ref}" >/dev/null 2>&1 || true
    docker rmi "${image}" >/dev/null 2>&1 || true

    echo ">>> [${image}] done"
}
export -f publish_one to_registry_path
export REGISTRY_HOST_ENDPOINT UPGRADE_TAG_SUFFIX
export ENGINE_IMAGE ENGINE_TAG METADATA_IMAGE METADATA_TAG

# Pre-flight: ensure the engine image's architecture matches the kind node's
# architecture. Otherwise the engine binary will run via the kind node's
# user-mode emulator which (notably on Apple Silicon, where Rosetta is NOT
# propagated into nested kind containers) is qemu-x86_64. qemu-x86_64 lacks
# AVX2/BMI2/FMA, so x86-64-v3 binaries SIGILL during startup -- and the
# failure surfaces ~6 minutes later as an opaque startup-probe timeout.
#
# We do this BEFORE the parallel push/rmi pass because publish_one removes
# the source image from the host once pushed; `docker image inspect` would
# then 404. Pulling here (single-shot, foreground) doubles as a sanity
# check that GHCR auth is configured before we kick off the parallel work.
ENGINE_REF="${ENGINE_IMAGE}:${ENGINE_TAG}"
echo ""
echo "=== Pre-flight: pull ${ENGINE_REF} and verify arch matches kind node ==="
docker pull "${ENGINE_REF}"
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

# xargs -P runs up to N children concurrently; if any child exits non-zero,
# xargs continues the rest but returns non-zero itself, which (combined with
# `set -e` and `pipefail`) fails the script. Each child runs under
# `bash -eo pipefail` so intra-child failures propagate correctly.
printf '%s\n' "${IMAGES[@]}" \
    | xargs -n1 -P "${LOAD_PARALLELISM}" -I{} \
        bash -eo pipefail -c 'publish_one "$1"' _ {}

echo ""
echo "=== Image publishing complete ==="
echo ""
echo "Catalog of repositories in local registry ${REGISTRY_HOST_ENDPOINT}:"
curl -fsS "http://${REGISTRY_HOST_ENDPOINT}/v2/_catalog" | head -c 4096 || true
echo ""
