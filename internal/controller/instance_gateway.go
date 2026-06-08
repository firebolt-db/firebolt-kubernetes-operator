/*
Copyright 2026 Firebolt Analytics.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// The dynamic forward proxy is configured in "sub cluster" mode rather
// than the simpler (default) DNS cache mode. In DNS cache mode Envoy
// keeps ONE host entry per authority, even when the authority's DNS
// name resolves to N A-records: every request for that authority is
// pinned to the same IP until the cache entry is refreshed, so a
// headless Kubernetes Service backing N pods effectively collapses to
// a single-pod LB target, with all load funneled at one pod. Worse,
// retries inside such a cluster have no alternative host to pick, so
// the "previous_hosts" retry predicate is a no-op.
//
// In sub-cluster mode, Envoy synthesizes a full-featured STRICT_DNS
// cluster per authority on first use. STRICT_DNS resolves the name to
// the complete set of A-records and creates one host per IP, so the
// normal load-balancer, outlier-detection and retry-host-predicate
// machinery all work as expected: requests round-robin across the pod
// set and retries actually land on a different pod than the failing
// one. This also dissolves the DNS-cache-vs-pod-teardown race that
// previously hid behind the cache's host_ttl: a stale IP is just one
// entry in a load-balanced pool now, not the only target.
//
// The HTTP filter and the cluster share the same dynamic-forward-proxy
// mode (sub-clusters) but their configs live in different protobufs
// and use slightly different field names (sub_cluster_config on the
// filter, sub_clusters_config on the cluster), so each is inlined at
// its use site rather than shared through a constant.

const (
	gatewayContainerPort int32 = 8080
	gatewayAdminPort     int32 = 9901
	gatewayServicePort   int32 = 80
	gatewayConfigKey           = "envoy.yaml"

	// gatewayPerConnectionBufferLimitBytes is the value the operator stamps
	// onto Envoy's per_connection_buffer_limit_bytes on BOTH the listener
	// and the dynamic_forward_proxy cluster. It is hard-coded — not exposed
	// on the FireboltInstance spec — because the same number governs three
	// invariants the operator owns end-to-end:
	//
	//   1. **Request-replay budget for retries.** Envoy's router can only
	//      replay a request whose full body fits in this buffer. The
	//      X-Firebolt-Drained retry rule (see retry_policy below) and the
	//      transport-failure retries (`connect-failure`, `refused-stream`,
	//      `reset`) ALL share this constraint. A request body larger than
	//      this limit is dispatched without buffering and any 5xx it
	//      receives — including a retry-safe shutdown-fence 503 —
	//      propagates to the client unretried, breaking the zero-downtime
	//      contract for that request. The chosen 2 MiB covers typical
	//      Firebolt SQL plus modest COPY ingest with headroom; jobs that
	//      send single requests larger than this are out of scope for the
	//      operator-managed retry path and should be split client-side.
	//
	//   2. **Gateway memory budget.** Peak buffering is roughly
	//      `concurrent_in_flight_requests * (1 + retry_factor) * this`.
	//      Doubling this value doubles the steady-state memory floor for
	//      gateway pods at any given concurrency. The hard-coded value is
	//      matched in helm/firebolt-operator/values.yaml's gateway
	//      resources defaults; raising it without also raising
	//      `spec.gateway.resources.limits.memory` and `replicas` invites
	//      OOMKills under load. Keeping it operator-controlled means the
	//      two move together.
	//
	//   3. **Cross-component agreement.** The standalone helm chart in
	//      ../firebolt-instance-helm renders the same buffer value through
	//      its own gateway-configmap.yaml; the operator-managed gateway
	//      and the chart-managed gateway behave identically here. If you
	//      change this value, change it in the helm chart's values.yaml
	//      in the same release so the two paths do not drift.
	//
	// See docs/architecture.mdx "Gateway query routing" → "Graceful pod
	// shutdown" → step 5 for the retry-coverage caveat as a user-facing
	// constraint, and the README's "Gateway sizing" section for the
	// memory-budget formula.
	gatewayPerConnectionBufferLimitBytes int64 = 2 << 20 // 2 MiB

	// gatewayMaxConnectionsPerEngine is the Envoy circuit-breaker cap on
	// concurrent upstream TCP connections in one DFP sub-cluster (i.e.
	// per engine, per gateway pod). With max_requests_per_connection=1
	// this is also the cap on concurrent in-flight queries to one
	// engine. See the cluster-level comment in buildEnvoyConfigYAML for
	// the rationale; keep this in lockstep with
	// gatewayMaxRequestsPerEngine (HTTP/2 active streams) and
	// gatewayMaxPendingRequestsPerEngine (queue depth).
	gatewayMaxConnectionsPerEngine     = 1024
	gatewayMaxPendingRequestsPerEngine = 1024
	gatewayMaxRequestsPerEngine        = 1024
	// gatewayMaxRetriesPerEngine bounds the simultaneous retry budget
	// across one engine sub-cluster. Higher than Envoy's default (3)
	// because the route's num_retries is 50 and a cutover can have many
	// in-flight retries at once; still bounded so a pathological retry
	// storm cannot consume the whole gateway.
	gatewayMaxRetriesPerEngine = 256
)

// ensureGatewayResources creates or updates the ConfigMap, Deployment, Service,
// and PDB for the Envoy gateway proxy.
//
// The gateway configuration is a pure function of the FireboltInstance - it does
// not depend on the engine set. Engines are discovered at request time via the
// X-Firebolt-Engine header and resolved through the (headless) engine Service's
// DNS. This keeps the ConfigMap (and therefore the gateway pod template) stable
// across engine create/delete/scale/blue-green events, eliminating gateway
// rollouts on engine lifecycle changes.
func (r *FireboltInstanceReconciler) ensureGatewayResources(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	log := logf.FromContext(ctx)

	envoyYAML := buildEnvoyConfigYAML(instance)

	// RBAC: the operator manages the gateway ServiceAccount, Role, and
	// RoleBinding only when the user has NOT supplied a custom
	// spec.gateway.template.spec.serviceAccountName. Setting that field
	// is the explicit opt-out signal: the user is taking responsibility
	// for the SA and the RBAC it needs (see
	// docs/crd-reference/instance-crd-reference.mdx "Gateway custom ServiceAccount"
	// for the verb set). Operator-managed RBAC would otherwise bind to
	// a different SA name than the one the gateway pod runs as, and
	// the wake-on-zero patch would silently 403 at runtime — worse
	// than the loud "ServiceAccount not found" the kubelet logs when
	// the user-supplied SA is missing.
	if userSA := userGatewayServiceAccountName(instance); userSA != "" {
		log.Info(
			"Skipping operator-managed gateway RBAC; user supplied spec.gateway.template.spec.serviceAccountName, so the user is responsible for the ServiceAccount and RBAC (get/list/patch on compute.firebolt.io/fireboltengines for wake-on-zero)",
			"serviceAccountName", userSA,
		)
	} else if err := r.ensureGatewayRBAC(ctx, instance); err != nil {
		return fmt.Errorf("ensuring gateway RBAC: %w", err)
	}

	if err := r.ensureGatewayConfigMap(ctx, instance, envoyYAML); err != nil {
		return fmt.Errorf("ensuring gateway configmap: %w", err)
	}

	if err := r.ensureGatewayDeployment(ctx, instance, envoyYAML); err != nil {
		return fmt.Errorf("ensuring gateway deployment: %w", err)
	}

	if err := r.ensureGatewayService(ctx, instance); err != nil {
		return fmt.Errorf("ensuring gateway service: %w", err)
	}

	if err := r.ensureGatewayPDB(ctx, instance); err != nil {
		return fmt.Errorf("ensuring gateway pdb: %w", err)
	}

	log.Info("Gateway resources ensured")
	return nil
}

func (r *FireboltInstanceReconciler) isGatewayReady(ctx context.Context, instance *computev1alpha1.FireboltInstance) (bool, error) {
	name := instance.Name + SuffixGateway
	var dep appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: instance.Namespace}, &dep); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return dep.Status.ReadyReplicas > 0, nil
}

// buildEnvoyConfigYAML generates the Envoy static config for the gateway.
//
// Routing model:
//   - The gateway requires an X-Firebolt-Engine header on every request.
//   - The Lua filter validates the header value matches an RFC 1123 DNS label
//     (so it cannot inject a path into :authority, cross namespaces, etc.),
//     rewrites :authority to "<engine>-service.<instance-ns>.svc.cluster.local:3473".
//   - The dynamic_forward_proxy cluster resolves that hostname at request time.
//     With the engine Service being headless, DNS returns the set of ready pod
//     IPs directly, bypassing Cilium LB and its endpoint-propagation lag.
//
// This config is deliberately engine-set agnostic so the ConfigMap never has to
// be regenerated in response to engine create/delete/scale events.
//
// WARNING: the port number "3473" in the :authority rewrite below is
// hardcoded and MUST be kept in sync with the "http-query" service port
// exposed by FireboltEngine (see GetServicePorts / GetContainerPorts in
// constants.go). Changing the engine query port without also updating
// this Lua string will silently break gateway -> engine routing: Envoy
// will try to connect to an arbitrary, unused port and every request
// will fail with a 503 from the dynamic_forward_proxy cluster. There is
// no compile-time link between the two today; consider extracting a
// shared constant if you need to change this port.
func buildEnvoyConfigYAML(instance *computev1alpha1.FireboltInstance) string {
	return fmt.Sprintf(`static_resources:
  listeners:
    - name: listener
      # per_connection_buffer_limit_bytes caps both downstream-receive and
      # upstream-send buffering on this listener, AND it is the budget the
      # router uses when deciding whether to BUFFER a request for retry.
      # A request whose body exceeds this cap is dispatched without
      # buffering and any 5xx it returns - including a retry-safe
      # X-Firebolt-Drained 503 - propagates to the client unretried.
      # Hard-coded by the operator (see gatewayPerConnectionBufferLimitBytes
      # in instance_gateway.go) and intentionally NOT exposed on
      # FireboltInstance.spec.gateway: the value is part of the operator's
      # zero-downtime + memory-budget contract.
      per_connection_buffer_limit_bytes: %d
      address:
        socket_address:
          address: 0.0.0.0
          port_value: %d
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: gateway
                access_log:
                  - name: envoy.access_loggers.stdout
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.access_loggers.stream.v3.StdoutAccessLog
                      # Explicit format: we care about the *why* behind 5xx, not just
                      # the code. RESPONSE_FLAGS gives the broad category (UH/UF/UC/UT
                      # /NR/LR/DPE/...), RESPONSE_CODE_DETAILS narrows it to the exact
                      # code path that produced the status, and
                      # UPSTREAM_TRANSPORT_FAILURE_REASON surfaces the TLS/TCP-level
                      # error string when the connection itself failed. Together they
                      # let us decide whether a 5xx was synthesized by Envoy before the
                      # request could have reached the engine (safe to retry) or was
                      # returned by the engine itself (unsafe - side effects may have
                      # executed).
                      log_format:
                        text_format_source:
                          inline_string: |
                            [%%START_TIME%%] "%%REQ(:METHOD)%% %%REQ(X-ENVOY-ORIGINAL-PATH?:PATH)%% %%PROTOCOL%%" %%RESPONSE_CODE%% flags=%%RESPONSE_FLAGS%% details=%%RESPONSE_CODE_DETAILS%% tx_fail=%%UPSTREAM_TRANSPORT_FAILURE_REASON%% upstream=%%UPSTREAM_HOST%% cluster=%%UPSTREAM_CLUSTER%% duration=%%DURATION%%ms rx=%%BYTES_RECEIVED%% tx=%%BYTES_SENT%% authority=%%REQ(:AUTHORITY)%% engine=%%REQ(X-FIREBOLT-ENGINE)%%

                http_filters:
                  - name: envoy.filters.http.health_check
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.health_check.v3.HealthCheck
                      pass_through_mode: false
                      headers:
                        - name: ":path"
                          string_match:
                            exact: "/healthz"
                  - name: envoy.filters.http.lua
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.lua.v3.Lua
                      default_source_code:
                        inline_string: |
                          -- Validates the engine name is a single RFC 1123 DNS
                          -- label: only lowercase alphanumerics and hyphens,
                          -- no dots (so the caller cannot reach across
                          -- namespaces or inject path separators into the
                          -- rewritten :authority), max 63 chars, no leading or
                          -- trailing hyphen.
                          local function is_valid_engine(s)
                            if s == nil or #s == 0 or #s > 63 then return false end
                            if not string.match(s, "^[%%l%%d][-%%l%%d]*$") then return false end
                            if string.sub(s, -1) == "-" then return false end
                            return true
                          end

                          function envoy_on_request(handle)
                            local engine = handle:headers():get("x-firebolt-engine")
                            if not is_valid_engine(engine) then
                              handle:respond({[":status"] = "400"}, "invalid or missing X-Firebolt-Engine header")
                              return
                            end

                            handle:headers():replace(":authority", engine .. "-service.%s.svc.cluster.local:3473")
                          end
                  - name: envoy.filters.http.dynamic_forward_proxy
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_forward_proxy.v3.FilterConfig
                      sub_cluster_config:
                        # Wait up to this long for the on-demand sub-cluster
                        # (per authority) to initialize on the very first
                        # request to a given engine. Steady-state requests
                        # don't pay this cost. 5s matches Envoy's own default
                        # and is generous enough for DNS resolution against
                        # kube-dns on a cold start.
                        cluster_init_timeout: 5s
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: local_route
                  virtual_hosts:
                    - name: default
                      domains: ["*"]
                      routes:
                        - match:
                            prefix: "/"
                          route:
                            cluster: dynamic_forward_proxy
                            # No route-level timeout: the caller's own deadline
                            # (HTTP client timeout) is the only overall cap. This
                            # lets the retry loop below ride out an arbitrary
                            # DNS-refresh window without having to match it to a
                            # magic constant here.
                            timeout: 0s
                            # Retry strategy.
                            #
                            # We retry ONLY on transport-level failures where the
                            # engine could not have observed the request as
                            # accepted work, and therefore retrying cannot
                            # duplicate any side effect:
                            #   - connect-failure: TCP connect failed (SYN RST,
                            #     timeout, etc.) - no bytes ever reached the
                            #     engine.
                            #   - refused-stream:  HTTP/2 REFUSED_STREAM - the
                            #     peer explicitly told us the stream was not
                            #     processed.
                            #   - reset:           stream reset before any
                            #     response bytes - same guarantee as
                            #     connect-failure.
                            #   - retriable-headers (X-Firebolt-Drained):
                            #     a 503 emitted by the engine's pre-work
                            #     shutdown fence (HTTPHandler::handleRequestImpl
                            #     fast-fails before any executor / Storage
                            #     Manager work). The fence sets this header
                            #     ONLY on that early-bail path, so the same
                            #     side-effect-free guarantee as the transport
                            #     failures above holds. Without this trigger,
                            #     a request that lands on a pod between
                            #     SIGTERM and the EndpointSlice update
                            #     would propagate a 503 to the client.
                            #
                            # We deliberately do NOT list "5xx" or
                            # "gateway-error" here: those match 5xx responses
                            # RETURNED BY THE ENGINE, which may have already
                            # executed side effects (e.g. a DML statement that
                            # partially mutated state). "5xx" returned by
                            # Envoy itself (flags=UF/URX, zero upstream bytes)
                            # falls under connect-failure/reset and is already
                            # covered.
                            #
                            # num_retries is set well above the steady-state
                            # replica count of any one engine. Combined with
                            # the previous_hosts retry predicate, this means
                            # each successive retry is directed to a pod we
                            # have not tried yet, until every pod in the
                            # sub-cluster's load-balanced set has been tried
                            # or the client-side deadline expires. Short
                            # exponential back-off lets the sub-cluster's
                            # own STRICT_DNS refresh tick (and any outlier
                            # ejection) take effect between attempts without
                            # needing to match its timer to a magic constant
                            # here. Each per-try attempt is bounded only by
                            # the cluster's connect_timeout, not by
                            # per_try_timeout, so legitimate long-running
                            # queries are never cut off mid-flight.
                            retry_policy:
                              retry_on: connect-failure,refused-stream,reset,retriable-headers
                              retriable_headers:
                                - name: X-Firebolt-Drained
                                  present_match: true
                              num_retries: 50
                              retry_back_off:
                                base_interval: 0.025s
                                max_interval: 0.25s
                              retry_host_predicate:
                                - name: envoy.retry_host_predicates.previous_hosts
                                  typed_config:
                                    "@type": type.googleapis.com/envoy.extensions.retry.host.previous_hosts.v3.PreviousHostsPredicate
                              host_selection_retry_max_attempts: 5
    - name: stats_listener
      address:
        socket_address:
          address: 0.0.0.0
          port_value: %d
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: stats
                route_config:
                  name: stats_route
                  virtual_hosts:
                    - name: stats
                      domains: ["*"]
                      routes:
                        - match:
                            prefix: "/stats/prometheus"
                          route:
                            cluster: admin_stats
                http_filters:
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
  clusters:
    - name: dynamic_forward_proxy
      lb_policy: CLUSTER_PROVIDED
      # Mirror the listener's per_connection_buffer_limit_bytes onto the
      # cluster so upstream connection buffers are sized identically. The
      # router consults the smaller of the two when deciding whether a
      # retry's request body fits, so leaving these in lockstep avoids a
      # surprise where raising the listener limit alone has no effect on
      # retry coverage. Hard-coded by the operator; see the listener-side
      # comment for the full rationale.
      per_connection_buffer_limit_bytes: %d
      # Short per-attempt TCP connect budget. "Connection refused" fails in
      # <1ms, but if the route to a stale pod IP is silently black-holed
      # (e.g. iptables DROP instead of REJECT) the connect can otherwise
      # hang for the kernel default (~130s) before the retry loop gets to
      # try another host / a freshly resolved address. 250ms is long enough
      # for a healthy intra-cluster connect (sub-millisecond in practice)
      # but short enough that the retry policy above can iterate many
      # times within a single client-side deadline.
      connect_timeout: 0.25s
      # One request per TCP connection.  Forces a fresh DNS lookup on every
      # query, so after the engine service selector switches (gen N → gen N+1)
      # the stale-IP window collapses to a single TCP connect rather than the
      # STRICT_DNS TTL (~5s).  Firebolt queries are long-running (seconds to
      # minutes), so the per-query handshake overhead (~1–3ms TLS) is
      # negligible.  Without this, HTTP/2 connection reuse means Envoy keeps
      # dispatching new streams to gen N pod IPs for up to the full DNS TTL
      # after the selector switch, and "Killing all queries" responses (HTTP
      # 200 + error body) from draining pods are not covered by the
      # transport-failure retry policy.
      max_requests_per_connection: 1
      # Circuit breakers — per-engine concurrency caps for the gateway.
      #
      # Each unique authority (one per engine's headless Service) gets
      # its own STRICT_DNS sub-cluster, and the thresholds below apply
      # per sub-cluster (Envoy circuit-breaker semantics). The effect is
      # per-engine isolation of gateway resources: a runaway or
      # misbehaving engine's traffic cannot consume more than its share
      # of connection pool slots, pending-request queue, in-flight
      # request budget, or retries, and therefore cannot starve sibling
      # engines on the same gateway pod.
      #
      # Sizing rationale (priority DEFAULT only — we do not route to
      # priority HIGH):
      #
      #   - max_connections: 1024. With max_requests_per_connection=1
      #     above, this is also the maximum concurrent in-flight queries
      #     to one engine through one gateway pod. The memory floor it
      #     implies (1024 * per_connection_buffer_limit_bytes = 2 GiB
      #     worst case per engine) is bounded; operators that expect
      #     higher per-engine concurrency raise gateway.replicas rather
      #     than this value, so the memory budget per pod stays
      #     predictable (see docs/instance/gateway/gateway-sizing.mdx).
      #
      #   - max_pending_requests: 1024. Queue depth before Envoy starts
      #     returning a synthetic "upstream connection pool overflow"
      #     503. Matched to max_connections so the gateway either
      #     dispatches the request or rejects it promptly; we never
      #     amplify backlog beyond the in-flight cap.
      #
      #   - max_requests: 1024. HTTP/2 active-streams cap; since
      #     max_requests_per_connection=1 collapses streams to
      #     connections, this is the same dimension as max_connections
      #     and is set in lockstep.
      #
      #   - max_retries: 256. Cluster-wide simultaneous retry budget.
      #     Higher than Envoy's default (3) because num_retries on the
      #     route policy is 50 and during a cutover many in-flight
      #     requests may be retrying at once. Still bounded so a
      #     pathological retry storm cannot consume the whole gateway.
      #
      # All four limits are hard-coded for the same reason as
      # per_connection_buffer_limit_bytes (see the file-level comment):
      # they sit at the center of the operator's per-engine isolation
      # contract, and exposing them on the CR would invite settings
      # that silently break the contract on one side without the other.
      circuit_breakers:
        thresholds:
          - priority: DEFAULT
            max_connections: %d
            max_pending_requests: %d
            max_requests: %d
            max_retries: %d
      # Active health checks — fast ejection of draining pods.
      #
      # Envoy probes every pod IP in the sub-cluster's STRICT_DNS set on
      # the interval below. When an engine pod receives SIGTERM it must
      # immediately start returning a non-2xx from /health/ready while
      # still accepting and completing any query that arrived before the
      # shutdown signal. Once Envoy sees unhealthy_threshold consecutive
      # failures it removes that pod from the load-balanced set, so no new
      # queries are dispatched to it for the remainder of its graceful-
      # shutdown window. Combined with max_requests_per_connection: 1
      # (which collapses the DNS-TTL staleness window to a single TCP
      # connect) this gives two independent layers of zero-downtime
      # protection without requiring xDS dynamic configuration.
      #
      # Why not a ClusterIP service instead of a headless one?
      # A ClusterIP VIP would remove the DNS-TTL race at the source, but
      # Envoy would see only one endpoint and lose the ability to load-
      # balance across pod IPs itself. That breaks the previous_hosts
      # retry predicate — which guarantees each retry attempt is directed
      # to a pod that has not already been tried — turning the retry loop
      # into a probabilistic gamble rather than an exhaustive sweep.
      # Keeping the headless service preserves Envoy's per-pod LB and
      # makes the retry policy meaningful.
      #
      # Active health checks on the query port (3473). DFP sub-clusters
      # create endpoints dynamically from DNS and do not support per-
      # endpoint port overrides (Endpoint.HealthCheckConfig.port_value
      # requires a static load_assignment; envoyproxy/envoy#14045), so
      # Envoy probes the same port it forwards queries to. The engine
      # exposes GET /health/ready on the query port: 200 when ready,
      # 503 on SIGTERM.
      #
      # When an engine pod receives SIGTERM it immediately returns 503
      # from /health/ready while still completing in-flight queries.
      # Once Envoy sees unhealthy_threshold consecutive failures it
      # removes that pod from the load-balanced set so no new queries
      # are dispatched to it for the remainder of its graceful-shutdown
      # window.
      health_checks:
        - timeout: 0.5s
          interval: 1s
          # no_traffic_interval: probe idle sub-clusters at the same cadence
          # as active ones. The default (60s) would mean Envoy only checks a
          # pod once per minute if no queries are in-flight, which defeats the
          # purpose of fast drain detection.
          no_traffic_interval: 1s
          healthy_threshold: 1
          unhealthy_threshold: 1
          http_health_check:
            path: /health/ready
      cluster_type:
        name: envoy.clusters.dynamic_forward_proxy
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.clusters.dynamic_forward_proxy.v3.ClusterConfig
          # sub_clusters_config replaces dns_cache_config. The DFP cluster
          # becomes a factory for per-authority sub-clusters; each one is a
          # full STRICT_DNS cluster that resolves the authority to all of
          # its A-records and load-balances across them. See the file-level
          # comment above for why this beats the default DNS-cache mode.
          sub_clusters_config:
            # ROUND_ROBIN across the pod IPs behind a headless engine
            # service. Any LB policy other than CLUSTER_PROVIDED is valid
            # here; ROUND_ROBIN is the simplest fair choice for a stateless
            # query fan-out.
            lb_policy: ROUND_ROBIN
            # Garbage-collect sub-clusters for engines that have not seen
            # traffic recently so a long-lived gateway doesn't accumulate
            # one cluster per ever-deleted engine over time.
            sub_cluster_ttl: 300s
    - name: admin_stats
      connect_timeout: 0.25s
      type: STATIC
      load_assignment:
        cluster_name: admin_stats
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address:
                      address: 127.0.0.1
                      port_value: %d
admin:
  address:
    socket_address:
      address: 127.0.0.1
      port_value: %d
`,
		gatewayPerConnectionBufferLimitBytes, // listener: per_connection_buffer_limit_bytes
		gatewayContainerPort,                 // listener: port_value
		instance.Namespace,                   // Lua: :authority rewrite
		instance.Spec.Gateway.MetricsPort,    // stats_listener: port_value
		gatewayPerConnectionBufferLimitBytes, // dynamic_forward_proxy cluster: per_connection_buffer_limit_bytes
		gatewayMaxConnectionsPerEngine,       // circuit_breakers: max_connections
		gatewayMaxPendingRequestsPerEngine,   // circuit_breakers: max_pending_requests
		gatewayMaxRequestsPerEngine,          // circuit_breakers: max_requests
		gatewayMaxRetriesPerEngine,           // circuit_breakers: max_retries
		gatewayAdminPort,                     // admin_stats endpoint: port_value
		gatewayAdminPort,                     // admin: port_value
	)
}

// Design note for the ensure* functions in this file (and their
// siblings in instance_metadata.go and instance_postgres.go): each one
// writes through Server-Side Apply (applySSA) with FieldManager
// OperatorFieldManager and ForceOwnership. The operator declares
// exactly the fields it owns (everything in the `desired` literal)
// and the apiserver tracks that ownership under
// metadata.managedFields; foreign-managed fields a user adds via
// kubectl/SSA from a different field manager — extra labels,
// annotations, sidecar containers, additional volumes — are
// preserved across reconciles. ForceOwnership keeps the operator
// authoritative on every field it does declare.
//
// The apiserver short-circuits no-op applies: an Apply whose
// post-defaulted object matches what is already stored does not bump
// .metadata.generation, so the Deployment controller never sees a
// spurious rollout even though we Apply on every reconcile.
// Pod-template changes propagate through the AnnotationConfigHash
// annotation (set from contentHash of the rendered config) so a real
// config change is always reflected in the stored spec and picked up
// by the Deployment controller.
//
// The engine controller's ensure* paths (engine_apply.go) follow the
// same SSA idiom. The "do I need a new blue-green generation?"
// decision still lives in stsMatchesSpec in engine_reconcile.go and
// runs before applyEngineState; SSA only changes how the chosen
// resources are written, not how the operator decides what to write.
func (r *FireboltInstanceReconciler) ensureGatewayConfigMap(ctx context.Context, instance *computev1alpha1.FireboltInstance, envoyYAML string) error {
	log := logf.FromContext(ctx).WithValues("instance", instance.Name)

	name := instance.Name + SuffixGateway + "-config"
	labels := instanceLabels(instance.Name, "gateway")

	desired := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels:    labels,
		},
		Data: map[string]string{
			gatewayConfigKey: envoyYAML,
		},
	}

	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return err
	}

	log.V(1).Info("Applying gateway ConfigMap", "name", name)
	return applySSA(ctx, r.Client, desired)
}

func (r *FireboltInstanceReconciler) ensureGatewayDeployment(ctx context.Context, instance *computev1alpha1.FireboltInstance, envoyYAML string) error {
	name := instance.Name + SuffixGateway
	configMapName := name + "-config"
	labels := instanceLabels(instance.Name, "gateway")

	gw := &instance.Spec.Gateway

	var replicas int32 = 2
	if gw.Replicas != nil {
		replicas = *gw.Replicas
	}

	configHash := contentHash(envoyYAML)

	maxSurge := intstr.FromString("25%")
	maxUnavailable := intstr.FromInt32(0)

	podTemplate := effectiveGatewayPodTemplate(instance, configMapName, configHash, labels)

	desired := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: &maxUnavailable,
					MaxSurge:       &maxSurge,
				},
			},
			Template: podTemplate,
		},
	}

	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return err
	}

	log := logf.FromContext(ctx).WithValues("instance", instance.Name)
	log.V(1).Info("Applying gateway Deployment", "name", name, "replicas", replicas, "image", podTemplate.Spec.Containers[0].Image)
	return applySSA(ctx, r.Client, desired)
}

func (r *FireboltInstanceReconciler) ensureGatewayService(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	name := instance.Name + SuffixGateway
	labels := instanceLabels(instance.Name, "gateway")

	desired := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			Ports: []corev1.ServicePort{
				{Name: "http", Port: gatewayServicePort, TargetPort: intstr.FromInt32(gatewayContainerPort), Protocol: corev1.ProtocolTCP},
			},
		},
	}

	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return err
	}

	log := logf.FromContext(ctx).WithValues("instance", instance.Name)
	log.V(1).Info("Applying gateway Service", "name", name)
	return applySSA(ctx, r.Client, desired)
}

func (r *FireboltInstanceReconciler) ensureGatewayPDB(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	name := instance.Name + SuffixGateway
	labels := instanceLabels(instance.Name, "gateway")
	maxUnavailable := intstr.FromInt32(1)

	desired := &policyv1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{APIVersion: "policy/v1", Kind: "PodDisruptionBudget"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels:    labels,
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &maxUnavailable,
			Selector:       &metav1.LabelSelector{MatchLabels: labels},
		},
	}

	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return err
	}

	log := logf.FromContext(ctx).WithValues("instance", instance.Name)
	log.V(1).Info("Applying gateway PDB", "name", name)
	return applySSA(ctx, r.Client, desired)
}

func boolPtr(v bool) *bool { return &v }

// gatewayMetricsPort returns the port number to expose Envoy's
// Prometheus metrics endpoint on. Defaults to 9090 when the user did
// not set spec.gateway.metricsPort.
func gatewayMetricsPort(gw *computev1alpha1.GatewaySpec) int32 {
	if gw.MetricsPort > 0 {
		return gw.MetricsPort
	}
	return 9090
}

// effectiveGatewayPodTemplate produces the gateway Deployment's pod
// template by merging the user-supplied
// FireboltInstance.spec.gateway.template with operator-rendered
// fields. The validating webhook (GatewayPodTemplateRules) has
// already rejected user input on any field this function overwrites,
// so callers can trust that the user template only carries fields the
// merge here is willing to forward.
//
// Field-by-field rules:
//
//   - Pod labels: operator-rendered base labels (firebolt.io/instance,
//     firebolt.io/component) override any user values on those exact
//     keys; user labels otherwise pass through.
//   - Pod annotations: operator AnnotationConfigHash overrides any
//     user value on that key; user annotations otherwise pass through.
//   - Scheduling fields (nodeSelector, tolerations, affinity,
//     topologySpreadConstraints, priorityClassName), pod-level
//     securityContext, imagePullSecrets, user-supplied initContainers
//     and additional containers: all pass through deep-copied from the
//     user template.
//   - ServiceAccountName: pass through from user when set; otherwise
//     the operator-built per-instance name.
//   - terminationGracePeriodSeconds, enableServiceLinks: operator-stamped.
//   - Primary container: operator-rendered Envoy at index 0; image
//     and ImagePullPolicy and Resources merged from any user-supplied
//     container with name == GatewayContainerName. User sidecars
//     (non-Envoy entries in user.template.spec.containers) appended
//     after the operator container.
//   - Volumes: operator config-volume + tmp at the head; user volumes
//     (excluding operator-reserved names — webhook already rejected
//     those, but the merge is defensive in case the webhook is off)
//     appended.
func effectiveGatewayPodTemplate(
	instance *computev1alpha1.FireboltInstance,
	configMapName string,
	configHash string,
	baseLabels map[string]string,
) corev1.PodTemplateSpec {
	var userPodMeta metav1.ObjectMeta
	var userPodSpec corev1.PodSpec
	if t := instance.Spec.Gateway.Template; t != nil {
		user := t.DeepCopy()
		userPodMeta = user.ObjectMeta
		userPodSpec = user.Spec
	}

	userPrimary, userSidecars := splitUserContainers(userPodSpec.Containers, computev1alpha1.GatewayContainerName)

	image := envoyImageFromUser(userPrimary)
	pullPolicy := envoyImagePullPolicyFromUser(userPrimary)

	var gracePeriod int64 = 15
	var runAsUser int64 = 101 // Envoy default UID

	// preStopScript uses bash's /dev/tcp pseudo-device to POST to Envoy's
	// admin API without requiring curl/wget in the image. The POST flips
	// the envoy.filters.http.health_check filter (pass_through_mode=false
	// in the gateway envoy.yaml) to return 503 on /healthz, which is what
	// the kubelet readiness probe hits.
	preStopScript := fmt.Sprintf(`exec 3<>/dev/tcp/127.0.0.1/%d
printf 'POST /healthcheck/fail HTTP/1.1\r\nHost: localhost\r\nContent-Length: 0\r\nConnection: close\r\n\r\n' >&3
cat <&3 >/dev/null
exec 3<&- 3>&-
sleep 8
`, gatewayAdminPort)

	envoy := corev1.Container{
		Name:            computev1alpha1.GatewayContainerName,
		Image:           image,
		ImagePullPolicy: pullPolicy,
		Args:            []string{"envoy", "-c", "/etc/envoy/envoy.yaml"},
		Ports: []corev1.ContainerPort{
			{Name: "http", ContainerPort: gatewayContainerPort, Protocol: corev1.ProtocolTCP},
			{Name: "metrics", ContainerPort: gatewayMetricsPort(&instance.Spec.Gateway), Protocol: corev1.ProtocolTCP},
		},
		Lifecycle: &corev1.Lifecycle{
			PreStop: &corev1.LifecycleHandler{
				Exec: &corev1.ExecAction{
					Command: []string{"bash", "-c", preStopScript},
				},
			},
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromString("http")},
			},
			InitialDelaySeconds: 1,
			PeriodSeconds:       15,
			TimeoutSeconds:      3,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromString("http")},
			},
			InitialDelaySeconds: 1,
			PeriodSeconds:       2,
			TimeoutSeconds:      2,
			FailureThreshold:    2,
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                &runAsUser,
			RunAsNonRoot:             boolPtr(true),
			ReadOnlyRootFilesystem:   boolPtr(true),
			AllowPrivilegeEscalation: boolPtr(false),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: computev1alpha1.GatewayConfigVolumeName, MountPath: "/etc/envoy", ReadOnly: true},
			{Name: computev1alpha1.GatewayTmpVolumeName, MountPath: "/tmp"},
		},
	}
	if userPrimary != nil && computev1alpha1.HasContainerResources(userPrimary.Resources) {
		envoy.Resources = *userPrimary.Resources.DeepCopy()
	}

	operatorVolumes := []corev1.Volume{
		{
			Name: computev1alpha1.GatewayConfigVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
					Items: []corev1.KeyToPath{
						{Key: gatewayConfigKey, Path: gatewayConfigKey},
					},
				},
			},
		},
		{
			Name:         computev1alpha1.GatewayTmpVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}

	containers := append([]corev1.Container{envoy}, userSidecars...)
	volumes := appendUserVolumes(operatorVolumes, userPodSpec.Volumes, computev1alpha1.GatewayConfigVolumeName, computev1alpha1.GatewayTmpVolumeName)

	sa := userPodSpec.ServiceAccountName
	if sa == "" {
		sa = gatewayServiceAccountName(instance.Name)
	}

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: mergeMaps(userPodMeta.Labels, baseLabels),
			Annotations: mergeMaps(userPodMeta.Annotations, map[string]string{
				AnnotationConfigHash: configHash,
			}),
		},
		Spec: corev1.PodSpec{
			ServiceAccountName:            sa,
			TerminationGracePeriodSeconds: &gracePeriod,
			EnableServiceLinks:            boolPtr(false),
			NodeSelector:                  userPodSpec.NodeSelector,
			Tolerations:                   userPodSpec.Tolerations,
			Affinity:                      userPodSpec.Affinity,
			TopologySpreadConstraints:     userPodSpec.TopologySpreadConstraints,
			PriorityClassName:             userPodSpec.PriorityClassName,
			SecurityContext:               userPodSpec.SecurityContext,
			ImagePullSecrets:              userPodSpec.ImagePullSecrets,
			InitContainers:                userPodSpec.InitContainers,
			Containers:                    containers,
			Volumes:                       volumes,
		},
	}
}

// splitUserContainers separates the user's template containers into
// the primary (matching primaryName) and the remaining sidecars.
// Returns nil for primary if the user did not declare it; an empty
// slice for sidecars if none.
func splitUserContainers(containers []corev1.Container, primaryName string) (primary *corev1.Container, sidecars []corev1.Container) {
	for i := range containers {
		c := containers[i]
		if c.Name == primaryName && primary == nil {
			cp := c
			primary = &cp
			continue
		}
		sidecars = append(sidecars, c)
	}
	return primary, sidecars
}

// envoyImageFromUser returns the user-supplied image on the gateway
// primary container, falling back to the operator's default Envoy
// image when the user did not set one.
func envoyImageFromUser(primary *corev1.Container) string {
	if primary != nil && primary.Image != "" {
		return primary.Image
	}
	return resolveImageRef(nil, DefaultEnvoyRepository, DefaultEnvoyTag)
}

// envoyImagePullPolicyFromUser returns the user-supplied pull policy
// on the gateway primary container, falling back to the operator's
// default-resolution rule (Always for :latest, IfNotPresent otherwise).
func envoyImagePullPolicyFromUser(primary *corev1.Container) corev1.PullPolicy {
	if primary != nil && primary.ImagePullPolicy != "" {
		return primary.ImagePullPolicy
	}
	if primary != nil && primary.Image != "" {
		return resolveContainerImagePullPolicy(primary.Image, "")
	}
	return resolveImagePullPolicy(nil)
}

// appendUserVolumes appends user-supplied volumes to the
// operator-rendered set, skipping any user entry whose name collides
// with an operator-reserved name. The validating webhook already
// rejects such collisions, but the merge is defensive so that a
// webhook outage cannot let a user-rendered volume shadow an
// operator-rendered one (which would silently break the matching
// volumeMount on the operator container).
func appendUserVolumes(operator []corev1.Volume, userVolumes []corev1.Volume, reserved ...string) []corev1.Volume {
	if len(userVolumes) == 0 {
		return operator
	}
	reservedSet := make(map[string]struct{}, len(reserved))
	for _, n := range reserved {
		reservedSet[n] = struct{}{}
	}
	out := make([]corev1.Volume, 0, len(operator)+len(userVolumes))
	out = append(out, operator...)
	for i := range userVolumes {
		if _, taken := reservedSet[userVolumes[i].Name]; taken {
			continue
		}
		out = append(out, *userVolumes[i].DeepCopy())
	}
	return out
}
