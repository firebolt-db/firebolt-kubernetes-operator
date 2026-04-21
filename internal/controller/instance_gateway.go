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

	computev1alpha1 "github.com/firebolt-analytics/firebolt-kubernetes-operator/api/v1alpha1"
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
	gatewayContainerName       = "envoy"
	gatewayConfigKey           = "envoy.yaml"
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
//     unconditionally appends advanced_mode=true to the query string, and
//     rewrites :authority to "<engine>-service.<instance-ns>.svc.cluster.local:3473".
//   - The dynamic_forward_proxy cluster resolves that hostname at request time.
//     With the engine Service being headless, DNS returns the set of ready pod
//     IPs directly, bypassing kube-proxy and its endpoint-propagation lag.
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

                            -- Unconditionally append advanced_mode=true. The
                            -- engine accepts the flag regardless of how it is
                            -- configured; clients don't need to know or set it.
                            local path = handle:headers():get(":path")
                            if path:find("?", 1, true) then
                              handle:headers():replace(":path", path .. "&advanced_mode=true")
                            else
                              handle:headers():replace(":path", path .. "?advanced_mode=true")
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
                              retry_on: connect-failure,refused-stream,reset
                              num_retries: 50
                              retry_back_off:
                                base_interval: 0.025s
                                max_interval: 0.25s
                              retry_host_predicate:
                                - name: envoy.retry_host_predicates.previous_hosts
                                  typed_config:
                                    "@type": type.googleapis.com/envoy.extensions.retry.host.previous_hosts.v3.PreviousHostsPredicate
                              host_selection_retry_max_attempts: 5
  clusters:
    - name: dynamic_forward_proxy
      lb_policy: CLUSTER_PROVIDED
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
admin:
  address:
    socket_address:
      address: 127.0.0.1
      port_value: %d
`,
		gatewayContainerPort,
		instance.Namespace,
		gatewayAdminPort,
	)
}

// Design note for the ensure* functions in this file (and their
// siblings in instance_metadata.go): each one calls r.Update
// unconditionally after Get, with no client-side equality check
// against the stored Spec. We rely on the API server — and, for
// Deployments, the built-in Deployment controller's own spec
// comparison — to short-circuit no-op writes: an Update whose
// post-defaulted object matches what is already stored does not
// bump .metadata.generation, so no spurious rollout is triggered.
// Pod-template changes do propagate through the AnnotationConfigHash
// annotation (set from contentHash of the rendered config), which
// means a real config change is always reflected in the stored spec
// and picked up by the Deployment controller.
//
// A bespoke client-side equality helper here would duplicate that
// logic and, worse, create a silent-drift hazard: any managed field
// we forget to include in the helper would mask a real change on
// every subsequent reconcile. The engine controller does keep
// stsSpecEqual in engine_apply.go because (a) the rollout cost of a
// false-positive update on a StatefulSet is much higher and (b) the
// engine generation model relies on explicit in-place vs new-gen
// decisions. The asymmetry is deliberate.
func (r *FireboltInstanceReconciler) ensureGatewayConfigMap(ctx context.Context, instance *computev1alpha1.FireboltInstance, envoyYAML string) error {
	log := logf.FromContext(ctx).WithValues("instance", instance.Name)

	name := instance.Name + SuffixGateway + "-config"
	labels := instanceLabels(instance.Name, "gateway")

	desired := &corev1.ConfigMap{
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

	existing := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: instance.Namespace}, existing)
	if errors.IsNotFound(err) {
		log.Info("Creating gateway ConfigMap", "name", name)
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	existing.Data = desired.Data
	existing.Labels = desired.Labels
	return r.Update(ctx, existing)
}

func (r *FireboltInstanceReconciler) ensureGatewayDeployment(ctx context.Context, instance *computev1alpha1.FireboltInstance, envoyYAML string) error {
	name := instance.Name + SuffixGateway
	configMapName := name + "-config"
	labels := instanceLabels(instance.Name, "gateway")

	spec := &instance.Spec.Gateway.ComponentSpec

	var replicas int32 = 2
	if spec.Replicas != nil {
		replicas = *spec.Replicas
	}

	image := DefaultEnvoyImage
	pullPolicy := corev1.PullIfNotPresent
	if spec.Image != nil {
		image = spec.Image.Repository + ":" + spec.Image.Tag
		pullPolicy = spec.Image.PullPolicy
		if pullPolicy == "" {
			pullPolicy = corev1.PullIfNotPresent
		}
	}

	configHash := contentHash(envoyYAML)

	maxSurge := intstr.FromString("25%")
	maxUnavailable := intstr.FromInt32(0)
	var runAsUser int64 = 101 // Envoy default UID

	// terminationGracePeriodSeconds budget for graceful shutdown:
	//   - preStop POSTs /healthcheck/fail to the Envoy admin (bash /dev/tcp
	//     because the stock envoyproxy/envoy image ships without curl/wget),
	//     then sleeps 8s so kube-proxy can drop this pod from Service
	//     endpoints before SIGTERM. The readiness probe below is tuned so
	//     that the flip is visible to kubelet within that window without
	//     being so tight that it causes steady-state flapping.
	//   - After preStop returns, kubelet sends SIGTERM and Envoy has the
	//     remaining ~7s to drain in-flight HTTP/2 streams and exit. Short
	//     requests finish; a hang in Envoy gets SIGKILLed at the grace
	//     deadline rather than stalling the whole rollout.
	//
	// The 15s / 8s split is a trade-off: lower it and Envoy loses drain
	// time; raise it and the rollout wall-clock grows. 15s keeps pod-level
	// shutdown well under the default pod-deletion timeout most schedulers
	// expect while still giving a proxy enough room to drain gracefully.
	var gracePeriod int64 = 15

	// preStopScript uses bash's /dev/tcp pseudo-device to POST to Envoy's
	// admin API without requiring curl/wget in the image. The POST flips
	// the envoy.filters.http.health_check filter (pass_through_mode=false
	// in the gateway envoy.yaml) to return 503 on /healthz, which is what
	// the kubelet readiness probe hits.
	//
	// Timing chain after the flip:
	//   - next probe tick:              up to 2s (PeriodSeconds=2)
	//   - FailureThreshold=2:           up to 2s more
	//   - EndpointSlice fanout:         ~1s
	//   - kube-proxy iptables rewrite:  ~1s
	// Worst case ~6s, which fits inside the 8s sleep with ~2s of margin.
	// By the time SIGTERM arrives, new client SYNs are no longer being
	// DNAT'd to this pod - eliminating the terminating-endpoint race where
	// new connections would hit a listener that is already shutting down.
	preStopScript := fmt.Sprintf(`exec 3<>/dev/tcp/127.0.0.1/%d
printf 'POST /healthcheck/fail HTTP/1.1\r\nHost: localhost\r\nContent-Length: 0\r\nConnection: close\r\n\r\n' >&3
cat <&3 >/dev/null
exec 3<&- 3>&-
sleep 8
`, gatewayAdminPort)

	podLabels := mergeMaps(labels, spec.Labels)
	podAnnotations := mergeMaps(map[string]string{
		AnnotationConfigHash: configHash,
	}, spec.Annotations)

	desired := &appsv1.Deployment{
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
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: &gracePeriod,
					Containers: []corev1.Container{{
						Name:            gatewayContainerName,
						Image:           image,
						ImagePullPolicy: pullPolicy,
						Args:            []string{"envoy", "-c", "/etc/envoy/envoy.yaml"},
						Ports: []corev1.ContainerPort{
							{Name: "http", ContainerPort: gatewayContainerPort, Protocol: corev1.ProtocolTCP},
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
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/healthz",
									Port: intstr.FromString("http"),
								},
							},
							InitialDelaySeconds: 1,
							PeriodSeconds:       15,
							TimeoutSeconds:      3,
						},
						// Readiness tuning balances two opposing requirements:
						//
						//   1. At shutdown, the preStop /healthcheck/fail flip must
						//      propagate to kube-proxy before SIGTERM. With the
						//      preStop sleep of 8s and terminationGracePeriodSeconds
						//      of 15s, we have budget for Period+Failure probe
						//      latency plus EndpointSlice/kube-proxy fanout.
						//   2. At steady state, a single probe hiccup (network
						//      blip, brief CPU throttle, transient listener stall)
						//      must not flap the pod out of the Service and cause
						//      cascading load onto the other replica.
						//
						// PeriodSeconds=2, TimeoutSeconds=2, FailureThreshold=2
						// gives worst-case ~4s detection - comfortably inside the
						// 8s preStop - while still requiring two consecutive bad
						// probes to mark NotReady, absorbing single-sample noise.
						//
						// The previous setting (default FailureThreshold=3 with
						// PeriodSeconds=3) gave ~9s worst-case detection, which is
						// past SIGKILL even with the old 5s TGPS; the preStop
						// script's comment claiming "readiness immediately goes
						// false" did not actually hold.
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/healthz",
									Port: intstr.FromString("http"),
								},
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
							{
								Name:      "config-volume",
								MountPath: "/etc/envoy",
								ReadOnly:  true,
							},
							{
								Name:      "tmp",
								MountPath: "/tmp",
							},
						},
					}},
					Volumes: []corev1.Volume{
						{
							Name: "config-volume",
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
							Name:         "tmp",
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
						},
					},
				},
			},
		},
	}

	if spec.Resources != nil {
		desired.Spec.Template.Spec.Containers[0].Resources = *spec.Resources
	}
	if len(spec.NodeSelector) > 0 {
		desired.Spec.Template.Spec.NodeSelector = spec.NodeSelector
	}
	if len(spec.Tolerations) > 0 {
		desired.Spec.Template.Spec.Tolerations = spec.Tolerations
	}
	if spec.Affinity != nil {
		desired.Spec.Template.Spec.Affinity = spec.Affinity
	}

	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return err
	}

	log := logf.FromContext(ctx).WithValues("instance", instance.Name)

	existing := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: instance.Namespace}, existing)
	if errors.IsNotFound(err) {
		log.Info("Creating gateway Deployment", "name", name, "replicas", replicas, "image", image)
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	existing.Spec.Replicas = desired.Spec.Replicas
	existing.Spec.Template = desired.Spec.Template
	existing.Spec.Strategy = desired.Spec.Strategy
	return r.Update(ctx, existing)
}

func (r *FireboltInstanceReconciler) ensureGatewayService(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	name := instance.Name + SuffixGateway
	labels := instanceLabels(instance.Name, "gateway")

	desired := &corev1.Service{
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

	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: instance.Namespace}, existing)
	if errors.IsNotFound(err) {
		log.Info("Creating gateway Service", "name", name)
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	existing.Spec.Ports = desired.Spec.Ports
	existing.Spec.Selector = desired.Spec.Selector
	return r.Update(ctx, existing)
}

func (r *FireboltInstanceReconciler) ensureGatewayPDB(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	name := instance.Name + SuffixGateway
	labels := instanceLabels(instance.Name, "gateway")
	maxUnavailable := intstr.FromInt32(1)

	desired := &policyv1.PodDisruptionBudget{
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

	existing := &policyv1.PodDisruptionBudget{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: instance.Namespace}, existing)
	if errors.IsNotFound(err) {
		log.Info("Creating gateway PDB", "name", name)
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	existing.Spec.MaxUnavailable = desired.Spec.MaxUnavailable
	existing.Spec.Selector = desired.Spec.Selector
	existing.Labels = desired.Labels
	return r.Update(ctx, existing)
}

func boolPtr(v bool) *bool { return &v }
