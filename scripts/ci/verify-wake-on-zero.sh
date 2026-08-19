#!/usr/bin/env bash
set -euo pipefail

# Verify wake-on-zero end to end against a chart-installed operator.
#
# This is the only check anywhere that exercises the real path: the
# operator-rendered Envoy config, the wake-agent sidecar built from the
# operator image, the agent's EndpointSlice watch, and the operator's demand
# poll. It lives here rather than in the Go e2e suite because that suite runs
# the operator in-process and never publishes an operator image, so the
# sidecar — which must occupy the gateway pod's own loopback for Envoy's Lua
# call to reach it — cannot exist there.
#
# The sequence is the one a user hits on their first query after an idle
# period: auto-stop parks the engine at zero, a query arrives, the gateway
# holds it, the operator scales the engine back up, and the held query is
# released and answered. A 503 or a connection reset at any point is a
# failure — the whole point of the feature is that the triggering query
# survives.
#
# The negative half of the same contract is pinned here too, while the engine
# is parked: a health probe on the gateway's /healthz is answered by the
# health-check filter ahead of the wake filter, so it never registers demand
# and never wakes the engine. Both halves are asserted from outside the pod,
# against the gateway Service — no filter-config inspection — so a refactor
# that reorders the filters or moves the health path fails these checks
# instead of slipping past them.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
source "${SCRIPT_DIR}/lib/verify-quickstart.sh"
source "${SCRIPT_DIR}/lib/setup-floci.sh"
# Sourced for CURL_IMAGE, matching what load-e2e-images.sh published.
IMAGE_VARIANT="${IMAGE_VARIANT:-latest}"
# shellcheck source=../../config/images/defaults.latest.env
set -a
source "${REPO_ROOT}/config/images/defaults.${IMAGE_VARIANT}.env"
set +a

NAMESPACE="${1:-helm-verify-wake}"
INSTANCE_NAME="${INSTANCE_NAME:-firebolt}"
ENGINE_NAME="${ENGINE_NAME:-engine}"
FLOCI_BUCKET="${FLOCI_BUCKET:-${ENGINE_NAME}-bucket}"
FLOCI_ENDPOINT="http://floci.${NAMESPACE}.svc.cluster.local:4566"
GATEWAY_SVC="${INSTANCE_NAME}-gateway"

# Aggressive so the idle scale-down lands inside a CI-friendly window. The
# operator's own defaults are 30m/1m.
IDLE_TIMEOUT="${IDLE_TIMEOUT:-25s}"
POLL_INTERVAL="${POLL_INTERVAL:-5s}"

# Bounds the held query. Must comfortably exceed engine cold start: the
# request is parked for exactly as long as the engine takes to come up, so a
# client deadline shorter than that kills the very request that triggers the
# wake. This is the client-side contract the feature imposes, and it is
# documented in docs/engine/auto-stop-and-wake-up.mdx.
WAKE_QUERY_TIMEOUT="${WAKE_QUERY_TIMEOUT:-240}"

# How long to wait for auto-stop to park the engine, and for the wake to
# bring it back.
STOP_WAIT_SECONDS="${STOP_WAIT_SECONDS:-180}"
WAKE_WAIT_SECONDS="${WAKE_WAIT_SECONDS:-240}"

# How many health probes to send at the parked engine, how long the probe
# pod may take to schedule and finish them, and how long the engine must
# still be observed at zero after the last probe. The settle window matters:
# stamped demand turns into a scale-up only after the operator notices it —
# its demand scrape runs every two seconds and triggers an immediate
# reconcile, with the auto-stop pollInterval as the fallback cadence — so
# the engine is watched well past both after the probes finish. An engine
# still at zero then proves no probe registered demand.
HEALTH_PROBE_COUNT="${HEALTH_PROBE_COUNT:-10}"
HEALTH_PROBE_TIMEOUT="${HEALTH_PROBE_TIMEOUT:-120}"
HEALTH_SETTLE_SECONDS="${HEALTH_SETTLE_SECONDS:-20}"

echo "=== verify-wake-on-zero (namespace=${NAMESPACE}) ==="
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

# floci first: the engine refuses to start without a reachable bucket.
setup_floci "$NAMESPACE" "$FLOCI_BUCKET"

kubectl apply -n "$NAMESPACE" -f "${REPO_ROOT}/examples/instance-basic.yaml"

# engine-basic.yaml with floci-backed storage (same shape as
# verify-ui-sidecar.sh — the engine's storage config schema is versioned with
# the engine image, so copy it from a script that currently passes rather
# than from docs) and an aggressive auto-stop policy. idleReplicas 0 is what
# makes this a wake test rather than a scale-down test.
BUCKET="$FLOCI_BUCKET" ENDPOINT="$FLOCI_ENDPOINT" \
IDLE="$IDLE_TIMEOUT" POLL="$POLL_INTERVAL" yq eval '
  (select(.kind == "FireboltEngine").spec.autoStop) = {
    "enabled": true,
    "activeReplicas": 1,
    "idleReplicas": 0,
    "idleTimeout": env(IDLE),
    "pollInterval": env(POLL)
  } |
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

# ---------------------------------------------------------------------------
# 1. The chart actually renders the sidecar, and it holds no writable token.
# ---------------------------------------------------------------------------

gateway_pod=$(kubectl get pod -n "$NAMESPACE" -l "firebolt.io/component=gateway" \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
if [[ -z "${gateway_pod}" ]]; then
  echo "No gateway pod found in namespace ${NAMESPACE}"
  dump_namespace_debug "$NAMESPACE"
  exit 1
fi
echo "Gateway pod: ${gateway_pod}"

containers=$(kubectl get pod "$gateway_pod" -n "$NAMESPACE" \
  -o jsonpath='{range .spec.containers[*]}{.name}{"\n"}{end}')
if ! grep -qx "wake-agent" <<<"$containers"; then
  echo "Gateway pod has no wake-agent container. Containers present:"
  printf '%s\n' "$containers"
  dump_namespace_debug "$NAMESPACE"
  exit 1
fi
echo "wake-agent sidecar is present"

# The security property the design rests on: Envoy terminates untrusted
# traffic and must not be able to reach a Kubernetes credential. Containers
# share a network namespace but not a mount namespace, so a token projected
# into the agent alone is invisible to Envoy.
automount=$(kubectl get pod "$gateway_pod" -n "$NAMESPACE" \
  -o jsonpath='{.spec.automountServiceAccountToken}')
if [[ "${automount}" != "false" ]]; then
  echo "automountServiceAccountToken = '${automount:-<unset>}', expected 'false'"
  exit 1
fi
envoy_mounts=$(kubectl get pod "$gateway_pod" -n "$NAMESPACE" \
  -o jsonpath='{range .spec.containers[?(@.name=="envoy")].volumeMounts[*]}{.mountPath}{"\n"}{end}')
if grep -q "serviceaccount" <<<"$envoy_mounts"; then
  echo "The envoy container mounts a ServiceAccount token; it must never hold a credential:"
  printf '%s\n' "$envoy_mounts"
  exit 1
fi
echo "envoy holds no ServiceAccount token; automount is disabled"

# With no probes on the sidecar (deliberately — see buildWakeAgentContainer),
# Ready here means "started without crashing", not "healthy". That is still
# worth asserting: it catches a bad image, a bad flag, or a crash loop. The
# wake path itself is proven by the query below, not by this.
echo "Waiting for the wake-agent container to start..."
for i in $(seq 1 60); do
  agent_ready=$(kubectl get pod "$gateway_pod" -n "$NAMESPACE" \
    -o jsonpath='{.status.containerStatuses[?(@.name=="wake-agent")].ready}' 2>/dev/null || echo "")
  if [[ "${agent_ready}" == "true" ]]; then
    echo "wake-agent Ready after ${i} attempt(s)"
    break
  fi
  if [[ "${i}" -eq 60 ]]; then
    echo "Timed out waiting for the wake-agent container to become Ready"
    kubectl logs "$gateway_pod" -n "$NAMESPACE" -c wake-agent --tail=50 || true
    dump_namespace_debug "$NAMESPACE"
    exit 1
  fi
  sleep 2
done

# ---------------------------------------------------------------------------
# 2. Let auto-stop park the engine at zero.
# ---------------------------------------------------------------------------

echo "Waiting up to ${STOP_WAIT_SECONDS}s for auto-stop to scale the engine to zero..."
deadline=$(( SECONDS + STOP_WAIT_SECONDS ))
while (( SECONDS < deadline )); do
  replicas=$(kubectl get fireboltengine "$ENGINE_NAME" -n "$NAMESPACE" \
    -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "")
  if [[ "${replicas}" == "0" ]]; then
    echo "Engine scaled to zero"
    break
  fi
  sleep 5
done
if [[ "$(kubectl get fireboltengine "$ENGINE_NAME" -n "$NAMESPACE" -o jsonpath='{.spec.replicas}')" != "0" ]]; then
  reason=$(kubectl get fireboltengine "$ENGINE_NAME" -n "$NAMESPACE" -o jsonpath='{.status.autoStopReason}' || true)
  echo "Engine never scaled to zero (autoStopReason=${reason:-<none>})"
  dump_namespace_debug "$NAMESPACE"
  exit 1
fi

# A stopped engine's headless Service has no endpoints, so its name does not
# resolve. That is precisely why Envoy cannot ride this out on its own and
# why the agent has to hold the request.
echo "Confirming the stopped engine has no endpoints..."
endpoints=$(kubectl get endpointslice -n "$NAMESPACE" \
  -l "kubernetes.io/service-name=${ENGINE_NAME}-service" \
  -o jsonpath='{range .items[*]}{.endpoints[*].addresses[*]}{" "}{end}' 2>/dev/null || echo "")
if [[ -n "${endpoints// /}" ]]; then
  echo "Stopped engine still has endpoints: ${endpoints}"
  exit 1
fi
echo "No endpoints, as expected"

# ---------------------------------------------------------------------------
# 3. Health probes are answered while the engine is parked, and never wake it.
# ---------------------------------------------------------------------------

# The gateway's client listener answers /healthz with its health-check filter,
# placed ahead of the wake filter, so a probe is served by Envoy itself and
# never reaches the wake path. Monitoring depends on that ordering: if a
# refactor moved the health-check filter behind the wake filter, every probe
# would stamp demand and no parked engine would ever stay parked. Two probe
# shapes are sent because they fail differently under a reorder — a bare
# probe would be rejected for its missing engine selector (non-200), and one
# naming the engine would wake it (scale-up during the hold).
health_url="http://${GATEWAY_SVC}.${NAMESPACE}.svc.cluster.local:80/healthz"
health_probe_pod="healthz-probe-$$"

echo "Probing ${health_url} while the engine is parked (${HEALTH_PROBE_COUNT} rounds)..."
: > /tmp/healthz-probe-output
(
  kubectl run "$health_probe_pod" -n "$NAMESPACE" --rm -i --restart=Never \
    --image="${CURL_IMAGE}" --command -- sh -c "
      for i in \$(seq 1 ${HEALTH_PROBE_COUNT}); do
        curl -sS -o /dev/null -w '%{http_code}\n' --max-time 5 '${health_url}'
        curl -sS -o /dev/null -w '%{http_code}\n' --max-time 5 \
          -H 'X-Firebolt-Engine: ${ENGINE_NAME}' '${health_url}'
        sleep 1
      done"
) > /tmp/healthz-probe-output 2>&1 &
health_probe_pid=$!

# Fails the phase when the engine leaves its parked state. Watching is not
# window-based but synchronized with the probe pod's lifetime: the watch runs
# for as long as the probes do (however long the pod takes to schedule), and
# then through a settle window after the last probe, since demand turns into
# a scale-up only after the operator's demand scrape sees it. A fixed window
# could stop watching while late probes were still being sent, and a demand
# stamped then would wake the engine unobserved — and hand the wake phase
# below an already-running engine.
require_engine_parked() {
  replicas=$(kubectl get fireboltengine "$ENGINE_NAME" -n "$NAMESPACE" \
    -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "")
  reason=$(kubectl get fireboltengine "$ENGINE_NAME" -n "$NAMESPACE" \
    -o jsonpath='{.status.autoStopReason}' 2>/dev/null || echo "")
  if [[ "${replicas}" != "0" || "${reason}" == "WakeRequested" ]]; then
    echo "A health probe woke the parked engine (replicas=${replicas:-?}, autoStopReason=${reason:-<none>})."
    echo "A probe on /healthz must be answered ahead of the wake filter and must"
    echo "never register demand."
    kill "$health_probe_pid" 2>/dev/null || true
    kubectl logs "$gateway_pod" -n "$NAMESPACE" -c wake-agent --tail=80 || true
    dump_namespace_debug "$NAMESPACE"
    exit 1
  fi
}

echo "Watching the engine while the probes run (up to ${HEALTH_PROBE_TIMEOUT}s)..."
probe_deadline=$(( SECONDS + HEALTH_PROBE_TIMEOUT ))
while kill -0 "$health_probe_pid" 2>/dev/null; do
  if (( SECONDS >= probe_deadline )); then
    echo "Health probes did not complete within ${HEALTH_PROBE_TIMEOUT}s:"
    cat /tmp/healthz-probe-output
    kill "$health_probe_pid" 2>/dev/null || true
    dump_namespace_debug "$NAMESPACE"
    exit 1
  fi
  require_engine_parked
  sleep 3
done

if ! wait "$health_probe_pid"; then
  echo "The health-probe pod failed:"
  cat /tmp/healthz-probe-output
  dump_namespace_debug "$NAMESPACE"
  exit 1
fi

echo "Probes done; the engine must stay at zero for another ${HEALTH_SETTLE_SECONDS}s..."
settle_deadline=$(( SECONDS + HEALTH_SETTLE_SECONDS ))
while (( SECONDS < settle_deadline )); do
  require_engine_parked
  sleep 3
done
# The loop's last check can land a few seconds short of the deadline, and a
# demand processed in that final gap would wake the engine unnoticed — and
# hand the wake phase below a pre-woken engine that satisfies its
# replicas/WakeRequested assertion without its query doing the waking. Check
# once more at the deadline, after the full settle has elapsed.
require_engine_parked

# Every probe must have been answered 200 by the gateway itself. A 400 means
# the probe fell through to engine routing (the health-check filter no longer
# answers ahead of it); a 404 means the health path moved off /healthz.
# Either way, every external monitor pointed at the gateway breaks with it.
health_statuses=$(grep -Eo '^[0-9]{3}$' /tmp/healthz-probe-output || true)
health_expected=$(( HEALTH_PROBE_COUNT * 2 ))
if [[ -z "${health_statuses}" ]]; then
  health_got=0
else
  health_got=$(wc -l <<<"$health_statuses" | tr -d ' ')
fi
if [[ "${health_got}" -ne "${health_expected}" ]]; then
  echo "Expected ${health_expected} health-probe responses, saw ${health_got}:"
  cat /tmp/healthz-probe-output
  dump_namespace_debug "$NAMESPACE"
  exit 1
fi
health_bad=$(grep -cv '^200$' <<<"$health_statuses" || true)
if [[ "${health_bad}" -ne 0 ]]; then
  echo "${health_bad} of ${health_got} health probes were not answered 200:"
  cat /tmp/healthz-probe-output
  dump_namespace_debug "$NAMESPACE"
  exit 1
fi
echo "${health_got} probes answered 200 and the engine stayed parked at zero"

# ---------------------------------------------------------------------------
# 4. The query that wakes it must itself succeed.
# ---------------------------------------------------------------------------

probe_pod="wake-probe-$$"
query_url="http://${GATEWAY_SVC}.${NAMESPACE}.svc.cluster.local:80/?output_format=JSON_Compact"

echo "Sending a query through the gateway at the stopped engine (timeout ${WAKE_QUERY_TIMEOUT}s)..."
echo "  The gateway should hold this request until the operator brings the engine up."

# Backgrounded so the wake can be observed while the query is still parked.
: > /tmp/wake-query-output
(
  kubectl run "$probe_pod" -n "$NAMESPACE" --rm -i --restart=Never \
    --image="${CURL_IMAGE}" --command -- \
    curl -sS -o /dev/stdout -w '\nHTTP_STATUS=%{http_code} TIME=%{time_total}\n' \
      --max-time "${WAKE_QUERY_TIMEOUT}" \
      -X POST -H "Content-Type: text/plain" -H "X-Firebolt-Engine: ${ENGINE_NAME}" \
      -d "SELECT 42" "${query_url}"
) > /tmp/wake-query-output 2>&1 &
query_pid=$!

echo "Waiting up to ${WAKE_WAIT_SECONDS}s for the wake to scale the engine back up..."
deadline=$(( SECONDS + WAKE_WAIT_SECONDS ))
woke=false
while (( SECONDS < deadline )); do
  replicas=$(kubectl get fireboltengine "$ENGINE_NAME" -n "$NAMESPACE" \
    -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "")
  reason=$(kubectl get fireboltengine "$ENGINE_NAME" -n "$NAMESPACE" \
    -o jsonpath='{.status.autoStopReason}' 2>/dev/null || echo "")
  # Both conditions, polled together. runAutoStop writes spec.replicas and
  # then status in separate API calls — deliberately, so the spec write
  # cannot be clobbered — so there is a window where replicas is already 1
  # while autoStopReason still reads the previous value. Failing on the
  # first mismatch would make this test flaky in exactly the case where
  # wake worked especially fast.
  if [[ "${replicas}" == "1" && "${reason}" == "WakeRequested" ]]; then
    echo "Engine scaled back to 1 (autoStopReason=${reason})"
    woke=true
    break
  fi
  sleep 3
done

if [[ "${woke}" != "true" ]]; then
  replicas=$(kubectl get fireboltengine "$ENGINE_NAME" -n "$NAMESPACE" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "")
  reason=$(kubectl get fireboltengine "$ENGINE_NAME" -n "$NAMESPACE" -o jsonpath='{.status.autoStopReason}' 2>/dev/null || echo "")
  if [[ "${replicas}" == "1" ]]; then
    echo "Engine scaled up but for the wrong reason: got '${reason}', want 'WakeRequested'."
    echo "Something other than gateway demand did it, so this run proves nothing."
  else
    echo "Engine never woke (replicas=${replicas:-?}, autoStopReason=${reason:-<none>})."
    echo "Gateway demand did not reach the operator."
  fi
  kubectl logs "$gateway_pod" -n "$NAMESPACE" -c wake-agent --tail=80 || true
  kill "$query_pid" 2>/dev/null || true
  dump_namespace_debug "$NAMESPACE"
  exit 1
fi

echo "Waiting for the held query to be released and answered..."
if ! wait "$query_pid"; then
  echo "The query that should have woken the engine failed:"
  cat /tmp/wake-query-output
  kubectl logs "$gateway_pod" -n "$NAMESPACE" -c wake-agent --tail=80 || true
  dump_namespace_debug "$NAMESPACE"
  exit 1
fi

query_output=$(cat /tmp/wake-query-output)
printf '%s\n' "$query_output"

if ! grep -q "HTTP_STATUS=200" <<<"$query_output"; then
  echo "The held query did not return 200. Wake is only useful if the triggering"
  echo "query survives; a 503 here means the client saw an error and would have"
  echo "had to retry, which is the behavior this feature exists to remove."
  kubectl logs "$gateway_pod" -n "$NAMESPACE" -c wake-agent --tail=80 || true
  dump_namespace_debug "$NAMESPACE"
  exit 1
fi
if ! grep -q "42" <<<"$query_output"; then
  echo "The held query returned 200 but not the expected result; it may have been"
  echo "answered by something other than the woken engine."
  exit 1
fi

echo "=== verify-wake-on-zero PASSED ==="
echo "Health probes on the parked engine's gateway were answered without waking"
echo "it. A query to the stopped engine was held by the gateway, the operator"
echo "scaled the engine on the demand the agent reported, and the same query was"
echo "released and answered — no client-visible error at any point."
