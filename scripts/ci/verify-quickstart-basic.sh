#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
source "${SCRIPT_DIR}/lib/verify-quickstart.sh"
source "${SCRIPT_DIR}/lib/setup-floci.sh"

NAMESPACE="${1:-helm-verify-basic}"
INSTANCE_NAME="${INSTANCE_NAME:-firebolt}"
ENGINE_NAME="${ENGINE_NAME:-engine}"
FLOCI_BUCKET="${FLOCI_BUCKET:-${ENGINE_NAME}-bucket}"
FLOCI_ENDPOINT="http://floci.${NAMESPACE}.svc.cluster.local:4566"

echo "=== verify-quickstart basic (namespace=${NAMESPACE}) ==="
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

# Deploy floci and create the bucket before the engine starts: the engine
# refuses to come up without bucket_name_override, and once configured it
# will start hitting floci on its first reconcile pass.
setup_floci "$NAMESPACE" "$FLOCI_BUCKET"

kubectl apply -n "$NAMESPACE" -f "${REPO_ROOT}/examples/instance-basic.yaml"

# Inject the top-level `storage:` block via spec.customEngineConfig so the
# rendered config.yaml steers tablet I/O at floci instead of the local fs.
# `type: minio` is what flips the engine's S3 client into MinIO-compat mode
# and hardcodes the access/secret to firebolt/firebolt internally — floci is
# zero-auth so those baked-in credentials sign requests just fine. There is
# no tmp_bucket field in the YAML schema; managed and tmp tablets share
# bucket_name.
BUCKET="$FLOCI_BUCKET" ENDPOINT="$FLOCI_ENDPOINT" yq eval '
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
' "${REPO_ROOT}/examples/engine-basic.yaml" | kubectl apply -n "$NAMESPACE" -f -

wait_instance_ready "$NAMESPACE" "$INSTANCE_NAME"
wait_engine_ready "$NAMESPACE" "$ENGINE_NAME"

# Prove the engine is actually serving queries through the gateway, not just
# reporting Ready. Mirrors the curl example in docs/quickstart.mdx.
run_query "$NAMESPACE" "$INSTANCE_NAME" "$ENGINE_NAME"

echo "✅ verify-quickstart basic passed (namespace=${NAMESPACE})"
echo "Cleaning up namespace ${NAMESPACE}..."
kubectl delete namespace "$NAMESPACE" --wait=false
