#!/usr/bin/env bash
# Removes all Firebolt custom resources and CRDs from the current cluster.
#
# When no operator is running, finalizers prevent deletion. This script
# patches them away before deleting the resources and CRDs.
#
# Usage:
#   ./scripts/cleanup-resources.sh                  # clean up default namespace
#   ./scripts/cleanup-resources.sh -n firebolt      # clean up a specific namespace
#   ./scripts/cleanup-resources.sh --all-namespaces  # clean up every namespace
set -euo pipefail

NAMESPACE=""
ALL_NS=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    -n|--namespace) NAMESPACE="$2"; shift 2 ;;
    --all-namespaces) ALL_NS=true; shift ;;
    -h|--help)
      echo "Usage: $0 [-n <namespace> | --all-namespaces]"
      exit 0
      ;;
    *) echo "Unknown flag: $1"; exit 1 ;;
  esac
done

if $ALL_NS; then
  NS_FLAG="--all-namespaces"
  NS_DISPLAY="all namespaces"
elif [[ -n "$NAMESPACE" ]]; then
  NS_FLAG="-n $NAMESPACE"
  NS_DISPLAY="namespace $NAMESPACE"
else
  NS_FLAG="-n default"
  NS_DISPLAY="namespace default"
fi

strip_finalizers() {
  local resource="$1"
  # shellcheck disable=SC2086
  local items
  items=$(kubectl get "$resource" $NS_FLAG -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
  for item in $items; do
    ns="${item%%/*}"
    name="${item##*/}"
    echo "  Stripping finalizers from $resource/$name in $ns"
    kubectl patch "$resource" "$name" -n "$ns" -p '{"metadata":{"finalizers":[]}}' --type=merge 2>/dev/null || true
  done
}

echo "Cleaning up Firebolt resources in $NS_DISPLAY..."

for crd in fireboltengines.compute.firebolt.io fireboltinstances.compute.firebolt.io; do
  if ! kubectl get crd "$crd" &>/dev/null; then
    continue
  fi

  resource="${crd%%.*}"
  echo "Processing $resource..."
  strip_finalizers "$resource"
  # shellcheck disable=SC2086
  kubectl delete "$resource" --all $NS_FLAG --wait=false 2>/dev/null || true
done

echo ""
echo "Removing CRDs..."
for crd in fireboltengines.compute.firebolt.io fireboltinstances.compute.firebolt.io; do
  if kubectl get crd "$crd" &>/dev/null; then
    kubectl patch crd "$crd" -p '{"metadata":{"finalizers":[]}}' --type=merge 2>/dev/null || true
    kubectl delete crd "$crd" --wait=false 2>/dev/null || true
    echo "  Deleted CRD $crd"
  fi
done

echo "Done."
