#!/usr/bin/env bash
set -euo pipefail

# Verify the built-in engine web UI sidecar end to end on a chart-installed
# operator: deploy an instance and an engine with `uiSidecar: true`, then
# assert that the injected `engine-web` container becomes Ready, still runs
# under the hardened securityContext (readOnlyRootFilesystem), and actually
# serves both the SPA index and the runtime /config.js over HTTP.
#
# The sidecar image is tracked at the mutable `:latest` alias, so this check
# doubles as a canary for UI image regressions against the operator's
# injected pod spec (e.g. an entrypoint change that conflicts with the
# read-only root filesystem): nothing else in CI ever starts this container.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
source "${SCRIPT_DIR}/lib/verify-quickstart.sh"
source "${SCRIPT_DIR}/lib/setup-floci.sh"
# Sourced for CURL_IMAGE (the in-cluster HTTP probe pod); same variant switch
# as the Makefile so the image matches what load-e2e-images.sh published.
IMAGE_VARIANT="${IMAGE_VARIANT:-latest}"
# shellcheck source=../../config/images/defaults.latest.env
set -a
source "${REPO_ROOT}/config/images/defaults.${IMAGE_VARIANT}.env"
set +a

NAMESPACE="${1:-helm-verify-ui}"
INSTANCE_NAME="${INSTANCE_NAME:-firebolt}"
ENGINE_NAME="${ENGINE_NAME:-engine}"
FLOCI_BUCKET="${FLOCI_BUCKET:-${ENGINE_NAME}-bucket}"
FLOCI_ENDPOINT="http://floci.${NAMESPACE}.svc.cluster.local:4566"
UI_PORT=9100

echo "=== verify-ui-sidecar (namespace=${NAMESPACE}) ==="
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

# Deploy floci and create the bucket before the engine starts: the engine
# refuses to come up without bucket_name_override, and once configured it
# will start hitting floci on its first reconcile pass.
setup_floci "$NAMESPACE" "$FLOCI_BUCKET"

kubectl apply -n "$NAMESPACE" -f "${REPO_ROOT}/examples/instance-basic.yaml"

# engine-basic.yaml with two patches: floci-backed managed storage (same as
# verify-quickstart-basic.sh) and the UI sidecar enabled.
BUCKET="$FLOCI_BUCKET" ENDPOINT="$FLOCI_ENDPOINT" yq eval '
  (select(.kind == "FireboltEngine").spec.uiSidecar) = true |
  (select(.kind == "FireboltEngine").spec.customEngineConfig) = {
    "storage": {
      "managed_table_storage": "s3",
      "managed_table_bucket_name": env(BUCKET),
      "aws": {
        "endpoint": env(ENDPOINT),
        "path_style_addressing": true
      }
    }
  }
' "${REPO_ROOT}/examples/engine-basic.yaml" | kubectl apply -n "$NAMESPACE" -f -

wait_instance_ready "$NAMESPACE" "$INSTANCE_NAME"
wait_engine_ready "$NAMESPACE" "$ENGINE_NAME"

engine_pod=$(kubectl get pod -n "$NAMESPACE" -l "firebolt.io/engine=${ENGINE_NAME}" \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
if [[ -z "${engine_pod}" ]]; then
  echo "No engine pod found with label firebolt.io/engine=${ENGINE_NAME} in namespace ${NAMESPACE}"
  dump_namespace_debug "$NAMESPACE"
  exit 1
fi

# The operator's own securityContext contract: the sidecar must stay
# hardened. If this fails, a regression "fixed" the UI by loosening the
# read-only root filesystem instead of keeping the image compatible with it.
rofs=$(kubectl get pod "$engine_pod" -n "$NAMESPACE" \
  -o jsonpath='{.spec.containers[?(@.name=="engine-web")].securityContext.readOnlyRootFilesystem}')
if [[ "${rofs}" != "true" ]]; then
  echo "engine-web readOnlyRootFilesystem = '${rofs:-<unset>}', expected 'true'"
  dump_namespace_debug "$NAMESPACE"
  exit 1
fi
echo "engine-web securityContext keeps readOnlyRootFilesystem=true"

echo "Waiting for the engine-web container in ${engine_pod} to become Ready..."
attempts=12
for i in $(seq 1 "$attempts"); do
  web_ready=$(kubectl get pod "$engine_pod" -n "$NAMESPACE" \
    -o jsonpath='{.status.containerStatuses[?(@.name=="engine-web")].ready}' 2>/dev/null || echo "")
  if [[ "${web_ready}" == "true" ]]; then
    echo "engine-web Ready after ${i} attempts"
    break
  fi
  if [[ "$i" == "$attempts" ]]; then
    echo "Timed out waiting for the engine-web container to become Ready"
    dump_namespace_debug "$NAMESPACE"
    exit 1
  fi
  echo "  attempt ${i}/${attempts}: ready='${web_ready:-<none>}' (sleep 5s)"
  sleep 5
done

# Hit the UI over the pod network from a throwaway curl pod: the SPA index
# must answer 200 and /config.js must carry the runtime config the
# entrypoint renders at startup.
pod_ip=$(kubectl get pod "$engine_pod" -n "$NAMESPACE" -o jsonpath='{.status.podIP}')
echo "Probing the Engine Web UI at ${pod_ip}:${UI_PORT} (index + /config.js)..."
attempts=6
for i in $(seq 1 "$attempts"); do
  if output=$(kubectl run "ui-probe-$i" -n "$NAMESPACE" --rm -i --restart=Never \
      --image="${CURL_IMAGE}" --quiet -- sh -c \
      "curl -sf -o /dev/null http://${pod_ip}:${UI_PORT}/ && curl -sf http://${pod_ip}:${UI_PORT}/config.js" 2>/dev/null); then
    if printf '%s' "$output" | grep -q "__FIREBOLT_CORE_CONFIG__"; then
      echo "UI index answers 200 and /config.js carries the runtime config:"
      printf '%s\n' "$output"
      break
    fi
    echo "  attempt ${i}/${attempts}: /config.js answered without __FIREBOLT_CORE_CONFIG__:"
    printf '%s\n' "$output"
  fi
  if [[ "$i" == "$attempts" ]]; then
    echo "Timed out probing the Engine Web UI on ${pod_ip}:${UI_PORT}"
    dump_namespace_debug "$NAMESPACE"
    exit 1
  fi
  echo "  attempt ${i}/${attempts}: UI not answering yet (sleep 5s)"
  sleep 5
done

echo "✅ verify-ui-sidecar passed (namespace=${NAMESPACE})"
echo "Cleaning up namespace ${NAMESPACE}..."
kubectl delete namespace "$NAMESPACE" --wait=false
