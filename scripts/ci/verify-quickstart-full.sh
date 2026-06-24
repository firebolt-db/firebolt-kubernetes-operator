#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
source "${SCRIPT_DIR}/lib/verify-quickstart.sh"
source "${SCRIPT_DIR}/lib/setup-floci.sh"
# Honour the same IMAGE_VARIANT switch as the Makefile (default: latest) so
# this script picks the matching defaults.<variant>.env that replaced the
# now-gone defaults.env in commit 554d320.
IMAGE_VARIANT="${IMAGE_VARIANT:-latest}"
# shellcheck source=../../config/images/defaults.latest.env
set -a
source "${REPO_ROOT}/config/images/defaults.${IMAGE_VARIANT}.env"
set +a

NAMESPACE="${1:-helm-verify-full}"
INSTANCE_NAME="${INSTANCE_NAME:-firebolt}"
ENGINE_NAME="${ENGINE_NAME:-engine}"
FLOCI_BUCKET="${FLOCI_BUCKET:-${ENGINE_NAME}-bucket}"
FLOCI_ENDPOINT="http://floci.${NAMESPACE}.svc.cluster.local:4566"

echo "=== verify-quickstart full (namespace=${NAMESPACE}) ==="
echo "Bootstrapping external Postgres endpoint for instance-full..."
kubectl apply -f "${REPO_ROOT}/scripts/ci/bootstrap-postgres-firebolt-namespace.yaml"
kubectl rollout status deployment/postgres -n firebolt --timeout=300s

echo "Labeling Kind nodes for instance-full scheduling..."
kubectl label nodes --all firebolt.dev/pool=system --overwrite
kubectl label nodes --all topology.kubernetes.io/zone=us-east-1a --overwrite

kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

# Deploy floci and create the bucket before the engine starts: the engine
# refuses to come up without bucket_name_override, and once configured it
# will start hitting floci on its first reconcile pass.
setup_floci "$NAMESPACE" "$FLOCI_BUCKET"

echo "Applying instance-full with metadata image pinned from config/images/defaults.${IMAGE_VARIANT}.env..."
# The metadata container's image lives on the embedded PodTemplateSpec at
# spec.metadata.template.spec.containers[name=="metadata"].image; there is
# no flat spec.metadata.image field. The
# example file already declares that container — we just overwrite
# image and imagePullPolicy.
METADATA_REF="${METADATA_IMAGE}:${METADATA_TAG}" yq eval '
  (select(.kind == "FireboltInstance").spec.metadata.template.spec.containers[] | select(.name == "metadata").image) = env(METADATA_REF) |
  (select(.kind == "FireboltInstance").spec.metadata.template.spec.containers[] | select(.name == "metadata").imagePullPolicy) = "IfNotPresent"
' "${REPO_ROOT}/examples/instance-full.yaml" | kubectl apply -n "$NAMESPACE" -f -
wait_instance_ready "$NAMESPACE" "$INSTANCE_NAME"

echo "Relabeling Kind nodes for engine-full scheduling..."
kubectl label nodes --all firebolt.dev/pool=engine --overwrite
kubectl label nodes --all topology.kubernetes.io/zone=us-east-1a --overwrite

echo "Creating FireboltEngineClass that pins the CI engine image (the engine image is defined on the class; there is no per-engine spec.image field)..."
ENGINE_CLASS_NAME="${ENGINE_NAME}-ci-class"
# FireboltEngineClass is namespaced; apply it to the engine's
# namespace so spec.engineClassRef resolves — the class must live
# with its consumer.
cat <<EOF | kubectl apply -n "$NAMESPACE" -f -
apiVersion: compute.firebolt.io/v1alpha1
kind: FireboltEngineClass
metadata:
  name: ${ENGINE_CLASS_NAME}
spec:
  template:
    spec:
      containers:
        - name: engine
          image: ${ENGINE_IMAGE}:${ENGINE_TAG}
          imagePullPolicy: IfNotPresent
EOF

echo "Applying engine-full with CI overrides (storage.size=1Gi, memory=2Gi) and floci managed_storage..."
BUCKET="$FLOCI_BUCKET" ENDPOINT="$FLOCI_ENDPOINT" CLASS="$ENGINE_CLASS_NAME" yq eval '
  (select(.kind == "FireboltEngine").spec.engineClassRef) = env(CLASS) |
  (select(.kind == "FireboltEngine").spec.storage.persistentVolumeClaim.size) = "500Mi" |
  (select(.kind == "FireboltEngine").spec.template.spec.containers[0].resources.requests.cpu) = "250m" |
  (select(.kind == "FireboltEngine").spec.template.spec.containers[0].resources.limits.cpu) = "250m" |
  (select(.kind == "FireboltEngine").spec.template.spec.containers[0].resources.requests.memory) = "2Gi" |
  (select(.kind == "FireboltEngine").spec.template.spec.containers[0].resources.limits.memory) = "2Gi" |
  (select(.kind == "FireboltEngine").spec.customEngineConfig) = {
    "storage": {
      "type": "minio",
      "api_scheme": "s3://",
      "bucket_name": env(BUCKET),
      "minio": {
        "endpoint": env(ENDPOINT)
      }
    }
  }
' "${REPO_ROOT}/examples/engine-full.yaml" | kubectl apply -n "$NAMESPACE" -f -
wait_engine_ready "$NAMESPACE" "$ENGINE_NAME"

# Prove the engine is actually serving queries through the gateway, not just
# reporting Ready. Mirrors the curl example in docs/quickstart.mdx.
run_query "$NAMESPACE" "$INSTANCE_NAME" "$ENGINE_NAME"

echo "✅ verify-quickstart full passed (namespace=${NAMESPACE})"
echo "Cleaning up namespace ${NAMESPACE}..."
kubectl delete namespace "$NAMESPACE" --wait=false
