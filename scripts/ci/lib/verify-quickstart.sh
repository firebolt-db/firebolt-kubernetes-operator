#!/usr/bin/env bash

set -euo pipefail

dump_namespace_debug() {
  local namespace="$1"

  echo "----- DEBUG: namespace ${namespace} -----"
  echo "[firebolt resources]"
  kubectl get fireboltinstances,fireboltengines -n "${namespace}" -o wide || true

  echo "[pods]"
  kubectl get pods -n "${namespace}" -o wide || true

  pending_pods=$(kubectl get pods -n "${namespace}" --field-selector=status.phase=Pending -o name 2>/dev/null || true)
  if [[ -n "${pending_pods}" ]]; then
    echo "[pending pod descriptions]"
    while IFS= read -r pod; do
      [[ -z "${pod}" ]] && continue
      echo "### kubectl describe ${pod} -n ${namespace}"
      kubectl describe "${pod}" -n "${namespace}" || true
    done <<< "${pending_pods}"
  fi

  echo "[events]"
  kubectl get events -n "${namespace}" --sort-by=.metadata.creationTimestamp || true
  echo "----- END DEBUG: namespace ${namespace} -----"
}

wait_instance_ready() {
  local namespace="$1"
  local name="$2"
  local attempts="${3:-16}"
  local sleep_seconds="${4:-5}"

  echo "Waiting for FireboltInstance/${name} in namespace ${namespace} to become Ready..."
  for i in $(seq 1 "${attempts}"); do
    phase=$(kubectl get fireboltinstance "${name}" -n "${namespace}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    if [[ "${phase}" == "Ready" ]]; then
      echo "FireboltInstance/${name} Ready in namespace ${namespace} after ${i} attempts"
      return 0
    fi
    echo "  attempt ${i}/${attempts}: phase='${phase:-<none>}' (sleep ${sleep_seconds}s)"
    sleep "${sleep_seconds}"
  done

  echo "Timed out waiting for FireboltInstance/${name} in namespace ${namespace}"
  kubectl describe fireboltinstance "${name}" -n "${namespace}" || true
  dump_namespace_debug "${namespace}"
  return 1
}

wait_engine_ready() {
  local namespace="$1"
  local name="$2"
  local attempts="${3:-16}"
  local sleep_seconds="${4:-5}"

  echo "Waiting for FireboltEngine/${name} in namespace ${namespace} to report Ready=True..."
  for i in $(seq 1 "${attempts}"); do
    ready=$(kubectl get fireboltengine "${name}" -n "${namespace}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "")
    if [[ "${ready}" == "True" ]]; then
      echo "FireboltEngine/${name} Ready in namespace ${namespace} after ${i} attempts"
      return 0
    fi
    echo "  attempt ${i}/${attempts}: ready='${ready:-<none>}' (sleep ${sleep_seconds}s)"
    sleep "${sleep_seconds}"
  done

  echo "Timed out waiting for FireboltEngine/${name} in namespace ${namespace}"
  kubectl describe fireboltengine "${name}" -n "${namespace}" || true
  dump_namespace_debug "${namespace}"
  return 1
}
