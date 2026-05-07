#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
source "${SCRIPT_DIR}/lib/verify-quickstart.sh"

NAMESPACE="${1:-helm-verify-basic}"
INSTANCE_NAME="${INSTANCE_NAME:-firebolt}"
ENGINE_NAME="${ENGINE_NAME:-engine}"

echo "=== verify-quickstart basic (namespace=${NAMESPACE}) ==="
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -n "$NAMESPACE" -f "${REPO_ROOT}/examples/instance-basic.yaml"
kubectl apply -n "$NAMESPACE" -f "${REPO_ROOT}/examples/engine-basic.yaml"

wait_instance_ready "$NAMESPACE" "$INSTANCE_NAME"
wait_engine_ready "$NAMESPACE" "$ENGINE_NAME"

echo "✅ verify-quickstart basic passed (namespace=${NAMESPACE})"
echo "Cleaning up namespace ${NAMESPACE}..."
kubectl delete namespace "$NAMESPACE" --wait=false
