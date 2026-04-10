#!/usr/bin/env bash
set -euo pipefail

# Load required Docker images into Kind cluster for e2e testing
# This script pulls images from ECR and loads them into Kind

CLUSTER_NAME="${1:-operator-test-e2e}"

# Engine image configuration (passed from Makefile)
ENGINE_IMAGE="${ENGINE_IMAGE:-000000000000.dkr.ecr.us-east-1.amazonaws.com/firebolt-core}"
ENGINE_TAG="${ENGINE_TAG:-release-4.32.0-pre.0.20260331033249.e67bde0be1cd-amd64}"
ENGINE_NEW_TAG="${ENGINE_NEW_TAG:-${ENGINE_TAG}}"

# Other images
PENSIEVE_TAG="${PENSIEVE_TAG:-4.32.0-pre.0.20260331033249.e67bde0be1cd}"
PENSIEVE_IMAGE="${PENSIEVE_IMAGE:-000000000000.dkr.ecr.us-east-1.amazonaws.com/dedicated-pensieve}"
OPERATOR_IMAGE="${OPERATOR_IMAGE:-controller:latest}"

echo "=== Loading images into Kind cluster: ${CLUSTER_NAME} ==="

# Check if Kind cluster exists
if ! kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    echo "Error: Kind cluster '${CLUSTER_NAME}' does not exist."
    echo "Run 'make setup-test-e2e' first."
    exit 1
fi

# NOTE: ECR authentication should be done before running this script.
# In CI, use firebolt-analytics/gha-workflows/.github/actions/ecr-login.
# Locally, run: aws ecr get-login-password --region us-east-1 | docker login --username AWS --password-stdin <registry>

# Build list of images to load
declare -a IMAGES=(
    "${ENGINE_IMAGE}:${ENGINE_TAG}"
    "${PENSIEVE_IMAGE}:${PENSIEVE_TAG}"
    "postgres:16-alpine"
)

# Add new engine tag if different from current tag (for upgrade tests)
if [[ "${ENGINE_NEW_TAG}" != "${ENGINE_TAG}" ]]; then
    IMAGES+=("${ENGINE_IMAGE}:${ENGINE_NEW_TAG}")
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
