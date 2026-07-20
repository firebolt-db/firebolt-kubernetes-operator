#!/usr/bin/env bash
set -euo pipefail

# Start a local Docker registry for kind, idempotent.
#
# Why this exists: `kind load docker-image` copies each image's bytes into
# every kind node's containerd snapshotter. With a multi-node kind cluster
# (we run 1 control-plane + N workers) and the engine image at multiple GB,
# that duplication blows past Docker Desktop's default ~64 GB VM disk.
#
# A local OCI registry sidesteps the duplication: images live ONCE in the
# registry, and each kind node pulls only the layers it actually runs. The
# kind nodes are wired to use this registry as a transparent mirror for
# ghcr.io, docker.io, and oci.firebolt.io via /etc/containerd/certs.d
# hosts.toml files written
# in scripts/setup-kind-cluster.sh after the cluster comes up.
#
# Layout:
#   - Container name: "kind-registry"
#   - Image: "registry:2"
#   - Host port: ${REGISTRY_PORT:-5001} -> container port 5000
#   - Network: connected to the "kind" Docker network so nodes resolve
#     "kind-registry:5000" via Docker's embedded DNS.
#
# This script is idempotent and safe to re-run:
#   - If the registry container is already running, exit 0 with no changes.
#   - If the container exists but is stopped, start it.
#   - If the "kind" network exists, ensure the registry is attached to it.
#
# References:
#   - https://kind.sigs.k8s.io/docs/user/local-registry/
#   - https://github.com/kubernetes/enhancements/tree/master/keps/sig-cluster-lifecycle/generic/1755-communicating-local-registry

REGISTRY_NAME="${REGISTRY_NAME:-kind-registry}"
REGISTRY_PORT="${REGISTRY_PORT:-5001}"
REGISTRY_IMAGE="${REGISTRY_IMAGE:-registry:2}"
KIND_NETWORK="${KIND_NETWORK:-kind}"

echo "=== Ensuring local Docker registry '${REGISTRY_NAME}' is running on 127.0.0.1:${REGISTRY_PORT} ==="

if ! command -v docker &>/dev/null; then
    echo "Error: docker is not installed or not on PATH." >&2
    exit 1
fi

# Decide what to do based on the registry container's current state.
running="$(docker inspect -f '{{.State.Running}}' "${REGISTRY_NAME}" 2>/dev/null || true)"
case "${running}" in
    true)
        echo "Registry '${REGISTRY_NAME}' is already running."
        ;;
    false)
        echo "Registry '${REGISTRY_NAME}' exists but is stopped. Starting it."
        docker start "${REGISTRY_NAME}" >/dev/null
        ;;
    *)
        echo "Registry '${REGISTRY_NAME}' does not exist. Creating it."
        # --restart=always so the registry survives developer reboots; the
        # next prepare-test-e2e re-uses the cached registry contents.
        docker run -d \
            --restart=always \
            --name "${REGISTRY_NAME}" \
            -p "127.0.0.1:${REGISTRY_PORT}:5000" \
            "${REGISTRY_IMAGE}" >/dev/null
        ;;
esac

# Connect the registry to the "kind" Docker network so kind nodes can reach
# it via the in-cluster DNS name "kind-registry". The "kind" network is
# created on first `kind create cluster`; if it does not exist yet (registry
# being set up before the cluster), Docker will create the network when the
# cluster is created and we run again, OR we create it ourselves here so
# the registry is reachable from the moment the cluster comes up.
if ! docker network inspect "${KIND_NETWORK}" >/dev/null 2>&1; then
    echo "Docker network '${KIND_NETWORK}' does not exist; creating it."
    docker network create "${KIND_NETWORK}" >/dev/null
fi

# `docker network connect` errors if the container is already attached.
# Inspect first to make this idempotent.
attached="$(docker inspect -f "{{json .NetworkSettings.Networks.${KIND_NETWORK}}}" "${REGISTRY_NAME}" 2>/dev/null || echo "null")"
if [ "${attached}" = "null" ]; then
    echo "Connecting registry to Docker network '${KIND_NETWORK}'."
    docker network connect "${KIND_NETWORK}" "${REGISTRY_NAME}"
else
    echo "Registry already attached to Docker network '${KIND_NETWORK}'."
fi

# Wait for the registry's HTTP API to respond. The `/v2/` endpoint returns
# 200 OK on a healthy distribution registry. We poll briefly because
# `docker run -d` returns before the process is ready to accept connections.
echo "Waiting for registry HTTP API..."
for i in {1..30}; do
    if curl -fsS "http://127.0.0.1:${REGISTRY_PORT}/v2/" >/dev/null 2>&1; then
        echo "Registry is responding on http://127.0.0.1:${REGISTRY_PORT}/v2/"
        break
    fi
    if [ "$i" -eq 30 ]; then
        echo "Error: registry did not become ready within 30s." >&2
        docker logs --tail 50 "${REGISTRY_NAME}" >&2 || true
        exit 1
    fi
    sleep 1
done

echo "=== Local Docker registry ready ==="
