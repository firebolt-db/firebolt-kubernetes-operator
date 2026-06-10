#!/usr/bin/env bash
# Verify the all-in-one website quickstart bundle
# (examples/quickstart-website/quickstart.yaml) against the operator deployed by
# `make local-deploy`. The bundle is what the docs tell users to
# `kubectl apply -f <raw URL>`, so this guards against it drifting out of sync
# with the CRDs or the operator's default engine image.
#
# The bundle is fully self-contained: it carries its own `firebolt` namespace,
# a floci S3 emulator plus a bucket-creation Job, a FireboltInstance, and a
# FireboltEngine that runs the operator's default image (no FireboltEngineClass).
# We apply it verbatim except for one CI concession: the engine's resource
# requests are scaled down so it schedules on a 2-CPU GitHub runner. The
# manifest as shipped targets a developer laptop.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
# shellcheck source=lib/verify-quickstart.sh
source "${SCRIPT_DIR}/lib/verify-quickstart.sh"

BUNDLE="${REPO_ROOT}/examples/quickstart-website/quickstart.yaml"

# These are fixed by the bundle; keep them in sync if the manifest renames them.
NAMESPACE="${NAMESPACE:-firebolt}"
INSTANCE_NAME="${INSTANCE_NAME:-quickstart}"
ENGINE_NAME="${ENGINE_NAME:-my-engine}"

echo "=== verify-quickstart website (bundle=${BUNDLE}) ==="

# Apply the bundle as a user would, with the engine sized down for CI. The
# select() assignments only touch the FireboltEngine document; every other
# document (namespace, floci, instance) passes through untouched. No CPU limit
# is set on purpose (multi-tenant convention).
yq eval '
  (select(.kind == "FireboltEngine").spec.template.spec.containers[0].resources.requests.cpu) = "250m" |
  (select(.kind == "FireboltEngine").spec.template.spec.containers[0].resources.requests.memory) = "2Gi" |
  (select(.kind == "FireboltEngine").spec.template.spec.containers[0].resources.limits.memory) = "2Gi"
' "$BUNDLE" | kubectl apply -f -

# All resources are applied at once, so allow a generous budget: the instance
# brings up Postgres, the metadata service, and the gateway, and the engine
# retries until floci and its bucket exist.
wait_instance_ready "$NAMESPACE" "$INSTANCE_NAME" 90 10
wait_engine_ready "$NAMESPACE" "$ENGINE_NAME" 90 10

# Prove the engine is actually serving queries through the gateway, not just
# reporting Ready. This is the same curl example the docs tell users to run.
run_query "$NAMESPACE" "$INSTANCE_NAME" "$ENGINE_NAME"

echo "✅ verify-quickstart website passed"

# Best-effort cleanup. Drop the bundle's own resources but keep the shared
# `firebolt` namespace, which other helm-test steps also use.
echo "Cleaning up website quickstart resources in namespace ${NAMESPACE}..."
yq eval 'select(.kind != "Namespace")' "$BUNDLE" \
  | kubectl delete --ignore-not-found --wait=false -f - || true
