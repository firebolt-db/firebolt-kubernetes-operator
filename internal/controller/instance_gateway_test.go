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
