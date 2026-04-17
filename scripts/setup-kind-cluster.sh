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

echo "=== Setting up Kind cluster: ${CLUSTER_NAME} ==="

# Check if Kind is installed
if ! command -v kind &> /dev/null; then
    echo "Error: Kind is not installed. Please install Kind manually."
    exit 1
fi

# Check if cluster already exists
if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    echo "Kind cluster '${CLUSTER_NAME}' already exists. Skipping creation."
    exit 0
fi

# Build kind create command
KIND_CREATE_CMD="kind create cluster --name ${CLUSTER_NAME} --retain"

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


# Wait for node to be Ready
echo "Waiting for node to be Ready..."
docker exec "${CONTROL_PLANE}" kubectl --kubeconfig=/etc/kubernetes/admin.conf \
    wait --for=condition=Ready node --all --timeout=120s

echo "=== Kind cluster '${CLUSTER_NAME}' is ready ==="
kubectl get nodes
