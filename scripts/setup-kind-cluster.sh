#!/usr/bin/env bash
set -euo pipefail

# Setup a Kind cluster for e2e testing
# This script handles Kind's startup timeout issues in slow environments
# (e.g., Docker-from-Docker in dev containers) by manually completing
# the cluster setup steps that Kind fails to finish.
#
# PREREQUISITE: Docker daemon must have memlock ulimit set to unlimited
# for io_uring to work properly. Add to /etc/docker/daemon.json:
#
#   {
#     "default-ulimits": {
#       "memlock": {
#         "Name": "memlock",
#         "Hard": -1,
#         "Soft": -1
#       }
#     }
#   }
#
# Then restart Docker: sudo systemctl restart docker

CLUSTER_NAME="${1:-operator-test-e2e}"
CONTROL_PLANE="${CLUSTER_NAME}-control-plane"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Local OCI registry that kind nodes use as a transparent mirror for
# ghcr.io / docker.io. Defaults match scripts/setup-local-registry.sh.
REGISTRY_NAME="${REGISTRY_NAME:-kind-registry}"
REGISTRY_PORT="${REGISTRY_PORT:-5001}"
# In-cluster endpoint that kind nodes use to reach the registry. Resolved
# through Docker's embedded DNS on the "kind" network, where the registry
# container is attached by setup-local-registry.sh.
REGISTRY_ENDPOINT="http://${REGISTRY_NAME}:5000"
# Mirrored upstreams. We push every workload image into the local registry
# under the path the upstream uses (firebolt-db/engine, library/postgres,
# envoyproxy/envoy, ...), so a single hosts.toml per upstream is enough to
# make pulls transparent.
MIRRORED_HOSTS=("ghcr.io" "docker.io")

# Write /etc/containerd/certs.d/<host>/hosts.toml on every kind node, aliasing
# each mirrored upstream to the local registry. containerd hot-reloads this
# directory when `config_path = "/etc/containerd/certs.d"` is set in
# /etc/containerd/config.toml (kind-config.yaml's containerdConfigPatches),
# so no daemon restart is needed. Writing the files is idempotent: re-running
# just overwrites the same content.
configure_mirrors() {
    local nodes
    if ! nodes="$(kind get nodes --name "${CLUSTER_NAME}" 2>/dev/null)"; then
        echo "Warning: could not list kind nodes for cluster '${CLUSTER_NAME}'; skipping mirror config." >&2
        return 0
    fi
    if [ -z "${nodes}" ]; then
        echo "Warning: kind reported no nodes for cluster '${CLUSTER_NAME}'; skipping mirror config." >&2
        return 0
    fi

    echo "Wiring containerd mirrors on each kind node -> ${REGISTRY_ENDPOINT}"
    for node in ${nodes}; do
        for host in "${MIRRORED_HOSTS[@]}"; do
            local certs_dir="/etc/containerd/certs.d/${host}"
            docker exec "${node}" mkdir -p "${certs_dir}"
            # No `server = ...` line: with `server` unset the original
            # registry is the implicit fallback. Our private engine /
            # metadata images would fail an unauthenticated fallback to
            # ghcr.io anyway, so the practical effect is that the local
            # registry is the only source — but we omit `server` so a
            # failed registry pull still tries the upstream for public
            # images (postgres, envoy, curl) that don't need auth.
            docker exec -i "${node}" sh -c "cat > ${certs_dir}/hosts.toml" <<EOF
[host."${REGISTRY_ENDPOINT}"]
  capabilities = ["pull", "resolve"]
EOF
        done
    done
}

echo "=== Setting up Kind cluster: ${CLUSTER_NAME} ==="

# Check if Kind is installed
if ! command -v kind &> /dev/null; then
    echo "Error: Kind is not installed. Please install Kind manually."
    exit 1
fi

# Pre-flight: the local registry must exist and be attached to the "kind"
# network BEFORE we wire containerd to mirror through it. Otherwise
# containerd starts with hosts.toml pointing at an unreachable host and the
# very first pod-template pull stalls. setup-local-registry.sh is
# idempotent; running it here is cheap when the registry is already up.
"${SCRIPT_DIR}/setup-local-registry.sh"

# Check if cluster already exists
if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    echo "Kind cluster '${CLUSTER_NAME}' already exists. Skipping creation."
    # If the cluster was created with the legacy kind-config (no
    # containerdConfigPatches for config_path), writing hosts.toml has no
    # effect because containerd never reads /etc/containerd/certs.d. Detect
    # that and tell the user how to migrate, otherwise their next image
    # pull silently goes to ghcr.io and fails for private images.
    if ! docker exec "${CONTROL_PLANE}" \
            grep -q 'config_path = "/etc/containerd/certs.d"' \
            /etc/containerd/config.toml 2>/dev/null; then
        echo "WARNING: existing cluster '${CLUSTER_NAME}' was created without the local-registry"
        echo "         mirror config. Pulls will not be redirected through the kind-registry."
        echo "         Run 'make cleanup-test-e2e' followed by 'make prepare-test-e2e' to recreate"
        echo "         the cluster with the new containerd config." >&2
    fi
    # Still ensure mirror config is in place (idempotent) in case the
    # cluster has the patch but a node is missing the hosts.toml files
    # (e.g. a prior partial run).
    configure_mirrors
    exit 0
fi

# Build kind create command
KIND_CREATE_CMD="kind create cluster --name ${CLUSTER_NAME} --retain --config ${SCRIPT_DIR}/kind-config.yaml"

# Create cluster (will likely fail due to timeout, but --retain keeps the container)
echo "Creating Kind cluster (this may report failure due to timeout - that's expected)..."
${KIND_CREATE_CMD} || true

# Wait for API server to be ready
echo "Waiting for API server to be ready..."
for i in {1..30}; do
    if docker exec "${CONTROL_PLANE}" kubectl --kubeconfig=/etc/kubernetes/admin.conf get nodes &>/dev/null; then
        echo "API server is ready"
        break
    fi
    echo "Waiting... (${i}/30)"
    sleep 5
done

# Verify API is accessible
if ! docker exec "${CONTROL_PLANE}" kubectl --kubeconfig=/etc/kubernetes/admin.conf get nodes &>/dev/null; then
    echo "Error: API server did not become ready in time"
    exit 1
fi

function extra_setup() {
# Export kubeconfig
echo "Exporting kubeconfig..."
kind export kubeconfig --name "${CLUSTER_NAME}"

# Fix kubeconfig for dev container access (use container IP instead of localhost)
echo "Updating kubeconfig for dev container access..."
KIND_IP=$(docker inspect "${CONTROL_PLANE}" --format='{{.NetworkSettings.Networks.kind.IPAddress}}')
kubectl config set-cluster "kind-${CLUSTER_NAME}" --server="https://${KIND_IP}:6443"

# Install kindnet CNI
echo "Installing kindnet CNI..."
POD_SUBNET=$(docker exec "${CONTROL_PLANE}" kubectl --kubeconfig=/etc/kubernetes/admin.conf cluster-info dump 2>/dev/null | grep -oP 'cluster-cidr=\K[0-9./]+' | head -1)
docker exec "${CONTROL_PLANE}" cat /kind/manifests/default-cni.yaml | \
    sed "s|{{ .PodSubnet }}|${POD_SUBNET}|g" | \
    docker exec -i "${CONTROL_PLANE}" kubectl --kubeconfig=/etc/kubernetes/admin.conf apply -f -

# Install local-path storage provisioner
echo "Installing local-path storage provisioner..."
docker exec "${CONTROL_PLANE}" cat /kind/manifests/default-storage.yaml | \
    docker exec -i "${CONTROL_PLANE}" kubectl --kubeconfig=/etc/kubernetes/admin.conf apply -f -

# Remove control-plane taint to allow scheduling on single-node cluster
echo "Removing control-plane taint..."
docker exec "${CONTROL_PLANE}" kubectl --kubeconfig=/etc/kubernetes/admin.conf \
    taint nodes --all node-role.kubernetes.io/control-plane- 2>/dev/null || true
}

## TODO: do we need this? what is the purpose?
#extra_setup

# Wait for node to be Ready
echo "Waiting for node to be Ready..."
docker exec "${CONTROL_PLANE}" kubectl --kubeconfig=/etc/kubernetes/admin.conf \
    wait --for=condition=Ready node --all --timeout=120s

# Wire containerd to use the local registry as a mirror for ghcr.io / docker.io.
# Done after nodes are Ready so we know the node containers are running and
# `kind get nodes` returns the full set.
configure_mirrors

echo "=== Kind cluster '${CLUSTER_NAME}' is ready ==="
kubectl get nodes
