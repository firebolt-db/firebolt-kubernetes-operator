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
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// TestBuildEnvoyConfigYAMLParses guards two invariants:
//  1. The emitted Envoy static config is valid YAML (catches any
//     fmt.Sprintf mis-escaping, e.g. Lua patterns containing `%`).
//  2. The config is a pure function of the FireboltInstance. It must not
//     contain anything engine-specific, otherwise every engine lifecycle event
//     would regenerate the ConfigMap and force a gateway rollout.
func TestBuildEnvoyConfigYAMLParses(t *testing.T) {
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
	}
	got := buildEnvoyConfigYAML(inst)

	var out map[string]any
	if err := yaml.Unmarshal([]byte(got), &out); err != nil {
		t.Fatalf("emitted envoy config is not valid YAML: %v\n---\n%s", err, got)
	}

	// Namespace must be baked into the :authority rewrite.
	if !strings.Contains(got, "-service.ns-1.svc.cluster.local:3473") {
		t.Errorf("emitted config does not contain the expected per-namespace :authority rewrite; got:\n%s", got)
	}

	// Engine-set independence: sentinel substrings that would indicate we
	// are still templating per-engine content.
	for _, forbidden := range []string{"advanced_engines", "advancedMode"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("emitted config contains %q; the gateway must be engine-agnostic", forbidden)
		}
	}
}

// TestBuildEnvoyConfigYAMLDFPSubClusterMode guards that the dynamic
// forward proxy is configured in sub-cluster mode (one synthesized
// STRICT_DNS sub-cluster per authority) rather than the default
// DNS-cache mode. Sub-cluster mode is what makes the gateway actually
// load-balance across the pod IPs of a headless engine Service; a
// regression back to dns_cache_config would silently collapse traffic
// onto a single pod per authority. See the file-level comment on
// instance_gateway.go for the full rationale.
func TestBuildEnvoyConfigYAMLDFPSubClusterMode(t *testing.T) {
	got := buildEnvoyConfigYAML(&computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
	})

	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("emitted envoy config is not valid YAML: %v", err)
	}

	filterTyped := dfpFilterTypedConfig(t, parsed)
	if _, ok := filterTyped["sub_cluster_config"]; !ok {
		t.Errorf("dynamic_forward_proxy HTTP filter missing sub_cluster_config; got typed_config keys = %v", keysOf(filterTyped))
	}
	if _, ok := filterTyped["dns_cache_config"]; ok {
		t.Error("dynamic_forward_proxy HTTP filter still has dns_cache_config; DNS-cache mode disables proper LB across pod IPs")
	}

	clusterTyped := dfpClusterTypedConfig(t, parsed)
	subClusters, ok := clusterTyped["sub_clusters_config"].(map[string]any)
	if !ok {
		t.Fatalf("dynamic_forward_proxy cluster missing sub_clusters_config; got typed_config keys = %v", keysOf(clusterTyped))
	}
	if _, ok := clusterTyped["dns_cache_config"]; ok {
		t.Error("dynamic_forward_proxy cluster still has dns_cache_config; DNS-cache mode disables proper LB across pod IPs")
	}
	if lb, _ := subClusters["lb_policy"].(string); lb != "ROUND_ROBIN" {
		t.Errorf("sub_clusters_config.lb_policy = %q; expected ROUND_ROBIN so requests fan out across the engine pod set", lb)
	}
}

// TestBuildEnvoyConfigYAMLRetryPolicy guards the retry contract:
//   - We retry on connect-failure / refused-stream / reset (transport-level
//     failures where the engine could not have observed the request).
//   - We retry on retriable-headers, gated by present_match on
//     X-Firebolt-Drained. The engine's shutdown fence sets that header
//     before any executor / Storage Manager work runs, so the request is
//     provably side-effect free; treating those 503s as retriable is the
//     only way the gateway can return a successful response when a query
//     lands on a pod between SIGTERM and the EndpointSlice update.
//   - We do NOT retry on bare 5xx; that would risk replaying a write that
//     the engine partially applied before failing.
//   - previous_hosts retry-host predicate is preserved; without it the
//     retry could land on the same draining pod and fail again.
func TestBuildEnvoyConfigYAMLRetryPolicy(t *testing.T) {
	got := buildEnvoyConfigYAML(&computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
	})

	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("emitted envoy config is not valid YAML: %v", err)
	}

	retry := dfpRouteRetryPolicy(t, parsed)

	retryOn, _ := retry["retry_on"].(string)
	for _, want := range []string{"connect-failure", "refused-stream", "reset", "retriable-headers"} {
		if !strings.Contains(retryOn, want) {
			t.Errorf("retry_on %q is missing %q", retryOn, want)
		}
	}
	for _, banned := range []string{"5xx", "gateway-error"} {
		if strings.Contains(retryOn, banned) {
			t.Errorf("retry_on %q must not include %q (would retry side-effecting 5xx)", retryOn, banned)
		}
	}

	// retriable_headers must include X-Firebolt-Drained with present_match.
	headers, ok := retry["retriable_headers"].([]any)
	if !ok || len(headers) == 0 {
		t.Fatalf("retriable_headers missing or not a list; got %T = %v", retry["retriable_headers"], retry["retriable_headers"])
	}
	foundDrained := false
	for _, h := range headers {
		hm, _ := h.(map[string]any)
		if hm == nil {
			continue
		}
		if name, _ := hm["name"].(string); strings.EqualFold(name, "X-Firebolt-Drained") {
			if pm, _ := hm["present_match"].(bool); pm {
				foundDrained = true
			}
		}
	}
	if !foundDrained {
		t.Errorf("retriable_headers does not include X-Firebolt-Drained with present_match=true; got %v", headers)
	}

	// previous_hosts predicate must be preserved.
	preds, _ := retry["retry_host_predicate"].([]any)
	hasPrev := false
	for _, p := range preds {
		pm, _ := p.(map[string]any)
		if name, _ := pm["name"].(string); strings.Contains(name, "previous_hosts") {
			hasPrev = true
		}
	}
	if !hasPrev {
		t.Error("retry_host_predicate missing previous_hosts; without it a retry can land on the same draining pod")
	}
}

// TestBuildEnvoyConfigYAMLBufferLimit guards the operator's hard-coded
// per_connection_buffer_limit_bytes. The value is intentionally NOT
// user-configurable (see the comment on gatewayPerConnectionBufferLimitBytes
// in instance_gateway.go for the rationale): if it changes, both the
// listener and the DFP cluster must change in lockstep, and that change
// must be deliberate. This test fails loudly on any drift between the
// two sites or any drift away from the documented 2 MiB.
func TestBuildEnvoyConfigYAMLBufferLimit(t *testing.T) {
	const want2MiB = 2 << 20

	got := buildEnvoyConfigYAML(&computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
	})

	want := fmt.Sprintf("per_connection_buffer_limit_bytes: %d", want2MiB)
	if n := strings.Count(got, want); n != 2 {
		t.Errorf("expected exactly 2 occurrences of %q (listener + DFP cluster); got %d in:\n%s",
			want, n, got)
	}
}

// TestBuildEnvoyConfigYAMLCircuitBreakers guards the per-engine
// concurrency caps on the dynamic_forward_proxy cluster. Circuit
// breakers on the parent DFP cluster apply per sub-cluster (one per
// engine authority) — without them, a runaway engine could consume
// every connection slot, pending-request slot, in-flight stream, or
// retry on a gateway pod and starve sibling engines.
//
// The values themselves are hard-coded by the operator (see the
// gatewayMax* constants in instance_gateway.go); the test asserts
// presence and equality against those constants so an accidental
// removal of the block, or a typoed override, fails CI loudly.
func TestBuildEnvoyConfigYAMLCircuitBreakers(t *testing.T) {
	got := buildEnvoyConfigYAML(&computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
	})

	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("emitted envoy config is not valid YAML: %v", err)
	}

	dfp := dfpCluster(t, parsed)
	cb, ok := dfp["circuit_breakers"].(map[string]any)
	if !ok {
		t.Fatalf("dynamic_forward_proxy cluster missing circuit_breakers; keys = %v", keysOf(dfp))
	}
	thresholds, ok := cb["thresholds"].([]any)
	if !ok || len(thresholds) == 0 {
		t.Fatalf("circuit_breakers.thresholds missing or empty; got %T = %v", cb["thresholds"], cb["thresholds"])
	}

	// Find the DEFAULT-priority threshold. The route does not target
	// priority HIGH, so any HIGH threshold here is dead config; the
	// invariants we care about live on DEFAULT.
	var th map[string]any
	for _, raw := range thresholds {
		tm, _ := raw.(map[string]any)
		if pr, _ := tm["priority"].(string); pr == "" || pr == "DEFAULT" {
			th = tm
			break
		}
	}
	if th == nil {
		t.Fatalf("no DEFAULT-priority threshold in circuit_breakers.thresholds; got %v", thresholds)
	}

	checks := []struct {
		field string
		want  int
	}{
		{"max_connections", gatewayMaxConnectionsPerEngine},
		{"max_pending_requests", gatewayMaxPendingRequestsPerEngine},
		{"max_requests", gatewayMaxRequestsPerEngine},
		{"max_retries", gatewayMaxRetriesPerEngine},
	}
	for _, c := range checks {
		// YAML numeric fields decode as int / int64 depending on size;
		// normalise to int64 for comparison.
		var got64 int64
		switch v := th[c.field].(type) {
		case int:
			got64 = int64(v)
		case int64:
			got64 = v
		case float64:
			got64 = int64(v)
		default:
			t.Errorf("circuit_breakers.thresholds[0].%s missing or unexpected type %T", c.field, v)
			continue
		}
		if got64 != int64(c.want) {
			t.Errorf("circuit_breakers.thresholds[0].%s = %d; want %d", c.field, got64, c.want)
		}
	}
}

// TestBuildEnvoyConfigYAML_EngineTLSDisabled_NoTransportSocket guards
// that the dynamic_forward_proxy cluster stays plaintext (no
// transport_socket key) when spec.tls.engine is unset — the status quo
// this bundled TLS work must not silently change for existing instances.
func TestBuildEnvoyConfigYAML_EngineTLSDisabled_NoTransportSocket(t *testing.T) {
	got := buildEnvoyConfigYAML(&computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
	})
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("emitted envoy config is not valid YAML: %v", err)
	}
	dfp := dfpCluster(t, parsed)
	if _, present := dfp["transport_socket"]; present {
		t.Errorf("dynamic_forward_proxy cluster has transport_socket with engine TLS disabled: %v", dfp["transport_socket"])
	}
}

// TestBuildEnvoyConfigYAML_EngineTLSDisabled_ByteIdenticalAcrossReconciles
// guards a real regression found during review: the transport_socket
// %s-placeholder substitution must not leave a stray blank line (or any
// other byte difference) when engine TLS is disabled, or
// contentHash(envoyYAML) — and therefore AnnotationConfigHash — changes
// for every existing non-TLS instance the moment this feature ships,
// forcing a gateway rollout nobody asked for.
func TestBuildEnvoyConfigYAML_EngineTLSDisabled_ByteIdenticalAcrossReconciles(t *testing.T) {
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
	}
	first := buildEnvoyConfigYAML(inst)
	if strings.Contains(first, "CLUSTER_PROVIDED\n\n") {
		t.Error("emitted config has a blank line after 'lb_policy: CLUSTER_PROVIDED'; " +
			"buildDFPUpstreamTLSTransportSocket's empty-string case must not leave a stray newline")
	}
	// Rendering twice must be byte-for-byte stable, matching every other
	// buildEnvoyConfigYAML invariant (TestBuildEnvoyConfigYAMLStableAcrossInstances).
	if second := buildEnvoyConfigYAML(inst); first != second {
		t.Error("buildEnvoyConfigYAML is not deterministic across calls with the same instance")
	}
}

// TestBuildEnvoyConfigYAML_EngineTLSEnabledButNotReady_NoTransportSocket
// pins down that buildDFPUpstreamTLSTransportSocket gates on
// Status.EngineTLS (readiness), not just Spec.TLS.Engine.Enabled: a
// transport_socket referencing an unmounted CA file would break every
// upstream connection until the volume catches up.
func TestBuildEnvoyConfigYAML_EngineTLSEnabledButNotReady_NoTransportSocket(t *testing.T) {
	got := buildEnvoyConfigYAML(&computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			TLS: &computev1alpha1.TLSSpec{
				Engine: &computev1alpha1.TLSListenerSpec{Enabled: true},
			},
		},
		// Status.EngineTLS deliberately left nil: certificate not yet issued.
	})
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("emitted envoy config is not valid YAML: %v", err)
	}
	dfp := dfpCluster(t, parsed)
	if _, present := dfp["transport_socket"]; present {
		t.Errorf("dynamic_forward_proxy cluster has transport_socket before EngineTLS is ready: %v", dfp["transport_socket"])
	}
}

// TestBuildEnvoyConfigYAML_EngineTLSReady_TransportSocketConfigured pins
// down the upstream TLS shape once engine TLS is ready: a
// trusted_ca pointing at the mounted CA file, and a static
// match_typed_subject_alt_names suffix matcher against the certificate's
// namespace-wide wildcard SAN (see buildDFPUpstreamTLSTransportSocket's
// doc comment for why this is a static suffix match rather than
// auto_sni/auto_san_validation).
func TestBuildEnvoyConfigYAML_EngineTLSReady_TransportSocketConfigured(t *testing.T) {
	got := buildEnvoyConfigYAML(&computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			TLS: &computev1alpha1.TLSSpec{
				Engine: &computev1alpha1.TLSListenerSpec{Enabled: true},
			},
		},
		Status: computev1alpha1.FireboltInstanceStatus{
			EngineTLS: &computev1alpha1.EngineTLSStatus{SecretName: "inst-engine-tls"},
		},
	})
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("emitted envoy config is not valid YAML: %v\n---\n%s", err, got)
	}
	dfp := dfpCluster(t, parsed)
	ts, ok := dfp["transport_socket"].(map[string]any)
	if !ok {
		t.Fatalf("dynamic_forward_proxy cluster missing transport_socket once EngineTLS is ready; keys = %v", keysOf(dfp))
	}
	if ts["name"] != "envoy.transport_sockets.tls" {
		t.Errorf("transport_socket.name = %v, want envoy.transport_sockets.tls", ts["name"])
	}
	typedConfig, ok := ts["typed_config"].(map[string]any)
	if !ok {
		t.Fatalf("transport_socket.typed_config missing or wrong type: %T", ts["typed_config"])
	}
	commonTLS, ok := typedConfig["common_tls_context"].(map[string]any)
	if !ok {
		t.Fatalf("typed_config.common_tls_context missing or wrong type: %T", typedConfig["common_tls_context"])
	}
	validationContext, ok := commonTLS["validation_context"].(map[string]any)
	if !ok {
		t.Fatalf("common_tls_context.validation_context missing or wrong type: %T", commonTLS["validation_context"])
	}
	trustedCA, ok := validationContext["trusted_ca"].(map[string]any)
	if !ok {
		t.Fatalf("validation_context.trusted_ca missing or wrong type: %T", validationContext["trusted_ca"])
	}
	wantFilename := gatewayEngineCAMountPath + "/" + engineTLSCASecretKey
	if trustedCA["filename"] != wantFilename {
		t.Errorf("trusted_ca.filename = %v, want %v", trustedCA["filename"], wantFilename)
	}
	sans, ok := validationContext["match_typed_subject_alt_names"].([]any)
	if !ok || len(sans) != 1 {
		t.Fatalf("validation_context.match_typed_subject_alt_names = %v, want a 1-element array", validationContext["match_typed_subject_alt_names"])
	}
	san, ok := sans[0].(map[string]any)
	if !ok {
		t.Fatalf("match_typed_subject_alt_names[0] = %v, want an object", sans[0])
	}
	if san["san_type"] != "DNS" {
		t.Errorf("match_typed_subject_alt_names[0].san_type = %v, want DNS", san["san_type"])
	}
	matcher, ok := san["matcher"].(map[string]any)
	if !ok {
		t.Fatalf("match_typed_subject_alt_names[0].matcher missing or wrong type: %T", san["matcher"])
	}
	wantSuffix := ".ns-1.svc.cluster.local"
	if matcher["suffix"] != wantSuffix {
		t.Errorf("matcher.suffix = %v, want %v (must be a suffix match, not exact — see doc comment)", matcher["suffix"], wantSuffix)
	}
}

// dfpCluster returns the parsed dynamic_forward_proxy cluster (the
// top-level entry, not its typed_config). Callers needing the inner
// typed_config use dfpClusterTypedConfig instead.
func dfpCluster(t *testing.T, parsed map[string]any) map[string]any {
	t.Helper()
	clusters := parsed["static_resources"].(map[string]any)["clusters"].([]any)
	for _, c := range clusters {
		cm, _ := c.(map[string]any)
		if name, _ := cm["name"].(string); name == "dynamic_forward_proxy" {
			return cm
		}
	}
	t.Fatal("dynamic_forward_proxy cluster not found in emitted config")
	return nil
}

func dfpRouteRetryPolicy(t *testing.T, parsed map[string]any) map[string]any {
	t.Helper()
	listeners := parsed["static_resources"].(map[string]any)["listeners"].([]any)
	filterChains := listeners[0].(map[string]any)["filter_chains"].([]any)
	filters := filterChains[0].(map[string]any)["filters"].([]any)
	hcm := filters[0].(map[string]any)["typed_config"].(map[string]any)
	virtualHosts := hcm["route_config"].(map[string]any)["virtual_hosts"].([]any)
	routes := virtualHosts[0].(map[string]any)["routes"].([]any)
	route := routes[0].(map[string]any)["route"].(map[string]any)
	rp, ok := route["retry_policy"].(map[string]any)
	if !ok {
		t.Fatalf("route retry_policy missing or wrong type; got %T", route["retry_policy"])
	}
	return rp
}

func dfpFilterTypedConfig(t *testing.T, parsed map[string]any) map[string]any {
	t.Helper()
	listeners := parsed["static_resources"].(map[string]any)["listeners"].([]any)
	filterChains := listeners[0].(map[string]any)["filter_chains"].([]any)
	filters := filterChains[0].(map[string]any)["filters"].([]any)
	httpFilters := filters[0].(map[string]any)["typed_config"].(map[string]any)["http_filters"].([]any)
	for _, f := range httpFilters {
		fm := f.(map[string]any)
		if fm["name"] == "envoy.filters.http.dynamic_forward_proxy" {
			return fm["typed_config"].(map[string]any)
		}
	}
	t.Fatal("dynamic_forward_proxy HTTP filter not found in emitted config")
	return nil
}

func dfpClusterTypedConfig(t *testing.T, parsed map[string]any) map[string]any {
	t.Helper()
	clusters := parsed["static_resources"].(map[string]any)["clusters"].([]any)
	return clusters[0].(map[string]any)["cluster_type"].(map[string]any)["typed_config"].(map[string]any)
}

func keysOf(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func TestBuildEnvoyConfigYAMLEngineFromQueryParam(t *testing.T) {
	got := buildEnvoyConfigYAML(&computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
	})

	for _, want := range []string{
		"extract_engine_from_path",
		`string.find(path, "?", 1, true)`,
		`url_decode(raw_key)`,
		`engine = url_decode(raw_value)`,
		`engine_count > 1`,
		`query_engine ~= nil and query_engine ~= header_engine`,
		`headers:replace(":path", stripped_path)`,
		`headers:replace("x-firebolt-engine", engine)`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("emitted config missing %q; gateway must securely consume and strip the firebolt CLI engine query parameter", want)
		}
	}
}

// TestBuildEnvoyConfigYAMLStableAcrossInstances ensures two different
// namespaces produce configs that differ only in the namespace-derived
// authority rewrite, not in any other structural way.
func TestBuildEnvoyConfigYAMLStableAcrossInstances(t *testing.T) {
	a := buildEnvoyConfigYAML(&computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns-a"},
	})
	b := buildEnvoyConfigYAML(&computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns-b"},
	})
	// Replacing the namespace-specific fragment in b should yield a.
	normalised := strings.ReplaceAll(b, "-service.ns-b.svc.cluster.local:3473", "-service.ns-a.svc.cluster.local:3473")
	if normalised != a {
		t.Fatal("configs differ in more than the namespace-scoped authority rewrite; a and b should be structurally identical")
	}
}
