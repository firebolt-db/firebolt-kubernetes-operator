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

  echo "[pod logs]"
  # Capture current and previous container output for every pod in the
  # namespace. The CI kind cluster is destroyed when the GitHub Actions job
  # ends, so anything we don't print here is lost — engine/metadata/gateway
  # logs in particular are essential for diagnosing startup failures.
  pods=$(kubectl get pods -n "${namespace}" -o name 2>/dev/null || true)
  while IFS= read -r pod; do
    [[ -z "${pod}" ]] && continue
    echo "### kubectl logs ${pod} -n ${namespace} --all-containers --tail=200"
    kubectl logs "${pod}" -n "${namespace}" --all-containers --tail=200 2>&1 || true
    prev=$(kubectl logs "${pod}" -n "${namespace}" --all-containers --previous --tail=200 2>/dev/null || true)
    if [[ -n "${prev}" ]]; then
      echo "### kubectl logs ${pod} -n ${namespace} --all-containers --previous --tail=200"
      printf '%s\n' "${prev}"
    fi
  done <<< "${pods}"

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

# Run a query through the instance gateway and assert the result contains an
# expected substring. This mirrors the firebolt client example in
# docs/quickstart.mdx: exec into an engine pod and use the bundled CLI with
# --endpoint <instance>-gateway and --set engine=<engine>.
#
# Usage:
#   run_query <namespace> <instance> <engine> [query] [expected] [attempts] [sleep]
#
# A FireboltEngine reporting Ready=True can still need a few seconds before the
# gateway routes queries to it (engine HTTP listener warming up), so this polls.
run_query() {
  local namespace="$1"
  local instance="$2"
  local engine="$3"
  local query="${4:-SELECT 42}"
  local expected="${5:-42}"
  local attempts="${6:-12}"
  local sleep_seconds="${7:-5}"

  local gateway="${instance}-gateway"
  echo "Running query against ${gateway} (engine=${engine}) in namespace ${namespace}: ${query}"

  local engine_pod
  engine_pod=$(kubectl get pod -n "${namespace}" -l "firebolt.io/engine=${engine}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  if [[ -z "${engine_pod}" ]]; then
    echo "No engine pod found with label firebolt.io/engine=${engine} in namespace ${namespace}"
    dump_namespace_debug "${namespace}"
    return 1
  fi

  local output=""
  for i in $(seq 1 "${attempts}"); do
    if output=$(kubectl exec "${engine_pod}" -n "${namespace}" -c engine -- \
        firebolt client \
          --endpoint "http://${gateway}" \
          --insecure \
          --set "engine=${engine}" \
          -c "${query}" \
          --no-interactive 2>/dev/null); then
      if printf '%s' "${output}" | grep -q "${expected}"; then
        echo "Query succeeded after ${i} attempt(s); result contains '${expected}':"
        printf '%s\n' "${output}"
        return 0
      fi
    fi
    echo "  attempt ${i}/${attempts}: no '${expected}' in result yet (sleep ${sleep_seconds}s)"
    sleep "${sleep_seconds}"
  done

  echo "Timed out running query against ${gateway} in namespace ${namespace}"
  echo "last output: ${output:-<none>}"
  dump_namespace_debug "${namespace}"
  return 1
}
