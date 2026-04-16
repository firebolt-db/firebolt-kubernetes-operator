#!/usr/bin/env bash
set -euo pipefail

# Load required Docker images into Kind cluster for e2e testing.
# All image values come from config/images/defaults.env (single source of truth).

CLUSTER_NAME="${1:-operator-test-e2e}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../config/images/defaults.env
source "${SCRIPT_DIR}/../config/images/defaults.env"

OPERATOR_IMAGE="controller:latest"

echo "=== Loading images into Kind cluster: ${CLUSTER_NAME} ==="

# Check if Kind cluster exists
if ! kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    echo "Error: Kind cluster '${CLUSTER_NAME}' does not exist."
    echo "Run 'make setup-kind' first."
    exit 1
fi

# NOTE: ECR authentication should be done before running this script.
# In CI, use firebolt-analytics/gha-workflows/.github/actions/ecr-login.
# Locally, run: aws ecr get-login-password --region us-east-1 | docker login --username AWS --password-stdin <registry>

# Build list of images to load
declare -a IMAGES=(
    "${ENGINE_IMAGE}:${ENGINE_TAG}"
    "${PENSIEVE_IMAGE}:${PENSIEVE_TAG}"
    "${POSTGRES_IMAGE}"
    "${ENVOY_IMAGE}:${ENVOY_TAG}"
    "${CURL_IMAGE}"
)

# Add new tags if different from current tags (for upgrade/switch tests)
if [[ "${ENGINE_NEW_TAG}" != "${ENGINE_TAG}" ]]; then
    IMAGES+=("${ENGINE_IMAGE}:${ENGINE_NEW_TAG}")
fi
if [[ "${PENSIEVE_NEW_TAG}" != "${PENSIEVE_TAG}" ]]; then
    IMAGES+=("${PENSIEVE_IMAGE}:${PENSIEVE_NEW_TAG}")
fi

# Pull and load each image
for IMAGE in "${IMAGES[@]}"; do
    echo ""
    echo "--- Processing: ${IMAGE} ---"

    # Check if image exists locally
    if ! docker image inspect "${IMAGE}" &>/dev/null; then
        echo "Pulling ${IMAGE}..."
        docker pull "${IMAGE}"
    else
        echo "Image ${IMAGE} already exists locally."
    fi

    echo "Loading ${IMAGE} into Kind..."
    kind load docker-image "${IMAGE}" --name "${CLUSTER_NAME}"
    echo "Successfully loaded ${IMAGE}"
done

# Load the operator image if it exists
echo ""
echo "--- Processing operator image: ${OPERATOR_IMAGE} ---"
if docker image inspect "${OPERATOR_IMAGE}" &>/dev/null; then
    echo "Loading ${OPERATOR_IMAGE} into Kind..."
    kind load docker-image "${OPERATOR_IMAGE}" --name "${CLUSTER_NAME}"
    echo "Successfully loaded ${OPERATOR_IMAGE}"
else
    echo "Operator image '${OPERATOR_IMAGE}' not found locally."
    echo "Build it with: make docker-build"
fi

echo ""
echo "=== Image loading complete ==="
echo ""
echo "Loaded images in Kind cluster:"
docker exec "${CLUSTER_NAME}-control-plane" crictl images 2>/dev/null | head -20 || true
