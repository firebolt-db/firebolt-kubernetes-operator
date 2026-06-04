#!/usr/bin/env bash
set -euo pipefail

# Install the dedicated CRD Helm chart against the current (kind) cluster and
# fail if `helm install` is rejected. This is the canonical guard against the
# CRDs growing back past Kubernetes' 1 MiB release-Secret cap (FB-1475): Helm
# packs the chart + rendered manifest, gzip+base64, into the
# sh.helm.release.v1.* Secret, and the apiserver refuses any Secret over
# 1048576 bytes, so an over-budget CRD chart aborts here with
# "data: Too long: may not be more than 1048576 bytes". A `--dry-run` would
# NOT catch this — the Secret is only written by a real install.
#
# config/crd/bases stays full-fat; the chart ships description-slimmed copies
# via scripts/strip-crd-descriptions.py (run by `make manifests`). See
# KNOWN_ISSUES.md (release-Secret size).
#
# Hermetic: installs into its own namespace/release and uninstalls on exit, so
# the rest of the test-helm flow (operator chart via `make local-deploy`)
# re-creates the CRDs from a clean slate. MUST therefore run before the
# operator chart is installed — its crds/ would otherwise already own these
# cluster-scoped CRDs and a templates/-based install would collide.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
CHART_DIR="${REPO_ROOT}/helm/firebolt-operator-crds"

NAMESPACE="${1:-helm-verify-crds}"
RELEASE="${RELEASE:-firebolt-crds}"
SECRET_LIMIT=1048576

cleanup() {
  helm uninstall "$RELEASE" -n "$NAMESPACE" >/dev/null 2>&1 || true
  # helm issues CRD deletion asynchronously; block until they are actually
  # gone so the follow-on operator-chart install starts from a clean slate.
  kubectl delete crd \
    fireboltinstances.compute.firebolt.io \
    fireboltengines.compute.firebolt.io \
    fireboltengineclasses.compute.firebolt.io \
    --ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1 || true
  kubectl delete namespace "$NAMESPACE" --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "=== verify CRD chart install (namespace=${NAMESPACE}, release=${RELEASE}) ==="

# Start clean: a prior run exits with `kubectl delete namespace --wait=false`,
# so the namespace may still be Terminating, which would make the install fail
# for the wrong reason ("namespace is being terminated"). Block until it is
# fully gone. No-op on a fresh CI cluster.
kubectl delete namespace "$NAMESPACE" --ignore-not-found --wait=true --timeout=120s >/dev/null 2>&1 || true

# The whole point: a real install writes the release Secret, which is what
# trips the 1 MiB cap. helm exits non-zero if the apiserver rejects it.
if ! helm install "$RELEASE" "$CHART_DIR" -n "$NAMESPACE" --create-namespace; then
  echo "❌ helm install of the CRD chart failed — most likely the release Secret exceeds the 1 MiB cap." >&2
  echo "   Confirm 'make manifests' ran scripts/strip-crd-descriptions.py and the slimmed chart CRDs are committed." >&2
  exit 1
fi

# A written release Secret is not proof the CRDs landed: poll until each is
# Established by the apiserver. (`kubectl wait --for=condition=Established`
# errors out when .status.conditions is briefly nil right after creation, so
# poll instead — same approach as the verify-quickstart helpers.)
for crd in fireboltinstances fireboltengines fireboltengineclasses; do
  full="${crd}.compute.firebolt.io"
  established=""
  for _ in $(seq 1 15); do
    established=$(kubectl get crd "$full" -o jsonpath='{.status.conditions[?(@.type=="Established")].status}' 2>/dev/null || echo "")
    [[ "$established" == "True" ]] && break
    sleep 2
  done
  if [[ "$established" != "True" ]]; then
    echo "❌ CRD ${full} did not reach Established (status='${established:-<none>}')." >&2
    exit 1
  fi
done

# Report headroom so a chart creeping toward the cap is visible well before it
# starts failing installs.
secret_bytes=$(kubectl get secret "sh.helm.release.v1.${RELEASE}.v1" -n "$NAMESPACE" -o jsonpath='{.data.release}' | wc -c)
pct=$(( secret_bytes * 100 / SECRET_LIMIT ))
echo "CRD chart release Secret: ${secret_bytes} / ${SECRET_LIMIT} bytes (${pct}% of cap)"
if [[ "$pct" -ge 90 ]]; then
  echo "⚠️  release Secret is at ${pct}% of the 1 MiB cap — trim CRDs further before it starts failing installs." >&2
fi

echo "✅ CRD chart installs cleanly and all three CRDs are Established (namespace=${NAMESPACE})"
