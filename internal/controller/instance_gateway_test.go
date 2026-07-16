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
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// TestGatewayTLSSecretVersions covers FB-896 finding #5: a bring-your-own
// gateway cert rotated in place (same Secret name, new bytes) must change the
// gateway config hash so the Deployment rolls and Envoy reloads the cert.
func TestGatewayTLSSecretVersions(t *testing.T) {
	sch := authTestScheme(t)

	t.Run("no gateway TLS returns empty", func(t *testing.T) {
		cli := fake.NewClientBuilder().WithScheme(sch).Build()
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}
		inst := &computev1alpha1.FireboltInstance{ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"}}
		got, err := r.gatewayTLSSecretVersions(context.Background(), inst)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "" {
			t.Errorf("gatewayTLSSecretVersions = %q, want empty when the gateway mounts no TLS Secret", got)
		}
	})

	t.Run("gateway TLS secret RV is included and changes on in-place rotation", func(t *testing.T) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "gw-tls", Namespace: "ns-1"},
			Data:       map[string][]byte{corev1.TLSCertKey: []byte("cert"), corev1.TLSPrivateKeyKey: []byte("key")},
		}
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(secret).Build()
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}
		inst := &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
			Status:     computev1alpha1.FireboltInstanceStatus{GatewayTLS: &computev1alpha1.GatewayTLSStatus{SecretName: "gw-tls"}},
		}
		before, err := r.gatewayTLSSecretVersions(context.Background(), inst)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if before == "" {
			t.Fatal("gatewayTLSSecretVersions returned empty for a configured gateway TLS secret")
		}

		var live corev1.Secret
		if err := cli.Get(context.Background(), types.NamespacedName{Namespace: "ns-1", Name: "gw-tls"}, &live); err != nil {
			t.Fatalf("get: %v", err)
		}
		live.Data[corev1.TLSCertKey] = []byte("rotated-cert")
		if err := cli.Update(context.Background(), &live); err != nil {
			t.Fatalf("update: %v", err)
		}
		after, err := r.gatewayTLSSecretVersions(context.Background(), inst)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if after == before {
			t.Error("gatewayTLSSecretVersions did not change after in-place secret rotation; the gateway Deployment would never roll")
		}
	})
}

// TestEngineFleetTLSState covers FB-896 finding #3: the gateway may only switch
// its upstream protocol based on the engine fleet's OBSERVED serving state, not
// on certificate existence. allOnTLS gates the enable ramp (switch to TLS only
// when every engine has rolled onto it); anyOnTLS gates the disable drain (keep
// TLS while any engine still serves it).
func TestEngineFleetTLSState(t *testing.T) {
	sch := authTestScheme(t)
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Status: computev1alpha1.FireboltInstanceStatus{
			EngineTLS: &computev1alpha1.EngineTLSStatus{SecretName: "inst-engine-tls"},
		},
	}
	onHash := tlsHash(&ResolvedEngineTLSInfo{SecretName: "inst-engine-tls"})
	newEngine := func(name, ref, observed string) *computev1alpha1.FireboltEngine {
		return &computev1alpha1.FireboltEngine{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns-1"},
			Spec:       computev1alpha1.FireboltEngineSpec{InstanceRef: ref},
			Status:     computev1alpha1.FireboltEngineStatus{ObservedEngineTLSHash: observed},
		}
	}
	cases := []struct {
		name             string
		engines          []*computev1alpha1.FireboltEngine
		wantAll, wantAny bool
	}{
		{"no engines is vacuously converged", nil, true, false},
		{"all engines on TLS", []*computev1alpha1.FireboltEngine{newEngine("e1", "inst", onHash), newEngine("e2", "inst", onHash)}, true, true},
		{"mixed fleet (mid-rollout)", []*computev1alpha1.FireboltEngine{newEngine("e1", "inst", onHash), newEngine("e2", "inst", "")}, false, true},
		{"all engines plaintext (drained)", []*computev1alpha1.FireboltEngine{newEngine("e1", "inst", ""), newEngine("e2", "inst", "")}, false, false},
		{"another instance's engine is ignored", []*computev1alpha1.FireboltEngine{newEngine("e1", "other-inst", onHash)}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(sch)
			for _, e := range tc.engines {
				b = b.WithObjects(e)
			}
			r := &FireboltInstanceReconciler{Client: b.Build(), Scheme: sch}
			allOn, anyOn, err := r.engineFleetTLSState(context.Background(), instance)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if allOn != tc.wantAll || anyOn != tc.wantAny {
				t.Errorf("engineFleetTLSState = (all=%v, any=%v), want (all=%v, any=%v)", allOn, anyOn, tc.wantAll, tc.wantAny)
			}
		})
	}
}

// TestEngineUpstreamTLSReady confirms the gateway render gates purely on the
// pre-computed Reencrypting signal, not on certificate existence alone.
func TestEngineUpstreamTLSReady(t *testing.T) {
	cases := []struct {
		name string
		tls  *computev1alpha1.EngineTLSStatus
		want bool
	}{
		{"no engine TLS status", nil, false},
		{"cert provisioned but fleet not converged", &computev1alpha1.EngineTLSStatus{SecretName: "s"}, false},
		{"fleet converged", &computev1alpha1.EngineTLSStatus{SecretName: "s", Reencrypting: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inst := &computev1alpha1.FireboltInstance{Status: computev1alpha1.FireboltInstanceStatus{EngineTLS: tc.tls}}
			if got := engineUpstreamTLSReady(inst); got != tc.want {
				t.Errorf("engineUpstreamTLSReady = %v, want %v", got, tc.want)
			}
		})
	}
}

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
			EngineTLS: &computev1alpha1.EngineTLSStatus{SecretName: "inst-engine-tls", Reencrypting: true},
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

// TestBuildEnvoyConfigYAML_BothEngineAndGatewayTLSReady_BothTransportSocketsConfigured
// covers the case a real "full TLS" deployment actually runs: engine TLS
// (upstream, on the dynamic_forward_proxy cluster) and gateway TLS
// (downstream, on the client-facing listener) enabled at once. Each is
// covered in isolation by the tests above; this locks down that the two
// %s placeholders — positioned at different points in the same
// fmt.Sprintf template — don't clobber each other or drift out of
// positional alignment with the rest of the argument list when someone
// edits the template later. Mirrors the auth-and-engine-tls combined case
// in TestBuildConfigMap_ConformsToPackdbSchema on the engine-config side.
func TestBuildEnvoyConfigYAML_BothEngineAndGatewayTLSReady_BothTransportSocketsConfigured(t *testing.T) {
	got := buildEnvoyConfigYAML(&computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			TLS: &computev1alpha1.TLSSpec{
				Engine:  &computev1alpha1.TLSListenerSpec{Enabled: true},
				Gateway: &computev1alpha1.TLSListenerSpec{Enabled: true},
			},
		},
		Status: computev1alpha1.FireboltInstanceStatus{
			EngineTLS:  &computev1alpha1.EngineTLSStatus{SecretName: "inst-engine-tls", Reencrypting: true},
			GatewayTLS: &computev1alpha1.GatewayTLSStatus{SecretName: "inst-gateway-tls"},
		},
	})
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("emitted envoy config is not valid YAML: %v\n---\n%s", err, got)
	}

	dfp := dfpCluster(t, parsed)
	upstreamTS, ok := dfp["transport_socket"].(map[string]any)
	if !ok {
		t.Fatalf("dynamic_forward_proxy cluster missing transport_socket with both TLS features ready; keys = %v", keysOf(dfp))
	}
	if upstreamTS["name"] != "envoy.transport_sockets.tls" {
		t.Errorf("upstream transport_socket.name = %v, want envoy.transport_sockets.tls", upstreamTS["name"])
	}

	fc := listenerFilterChain(t, parsed)
	downstreamTS, ok := fc["transport_socket"].(map[string]any)
	if !ok {
		t.Fatalf("client-facing listener missing transport_socket with both TLS features ready; keys = %v", keysOf(fc))
	}
	if downstreamTS["name"] != "envoy.transport_sockets.tls" {
		t.Errorf("downstream transport_socket.name = %v, want envoy.transport_sockets.tls", downstreamTS["name"])
	}

	// Cross-check that each transport_socket points at ITS OWN secret's
	// files, not the other one's — the clearest sign a positional-arg
	// mixup would produce.
	upstreamTyped := upstreamTS["typed_config"].(map[string]any)
	upstreamCA := upstreamTyped["common_tls_context"].(map[string]any)["validation_context"].(map[string]any)["trusted_ca"].(map[string]any)
	wantUpstreamCA := gatewayEngineCAMountPath + "/" + engineTLSCASecretKey
	if upstreamCA["filename"] != wantUpstreamCA {
		t.Errorf("upstream trusted_ca.filename = %v, want %v", upstreamCA["filename"], wantUpstreamCA)
	}

	downstreamTyped := downstreamTS["typed_config"].(map[string]any)
	downstreamCert := downstreamTyped["common_tls_context"].(map[string]any)["tls_certificates"].([]any)[0].(map[string]any)
	wantDownstreamChain := gatewayTLSMountPath + "/" + string(corev1.TLSCertKey)
	if downstreamCert["certificate_chain"].(map[string]any)["filename"] != wantDownstreamChain {
		t.Errorf("downstream certificate_chain.filename = %v, want %v",
			downstreamCert["certificate_chain"].(map[string]any)["filename"], wantDownstreamChain)
	}
}

// TestBuildEnvoyConfigYAML_GatewayTLSDisabled_NoTransportSocket guards
// that the client-facing listener stays plaintext (no transport_socket
// key on its filter_chain) when spec.tls.gateway is unset — the status
// quo this feature must not silently change for existing instances.
func TestBuildEnvoyConfigYAML_GatewayTLSDisabled_NoTransportSocket(t *testing.T) {
	got := buildEnvoyConfigYAML(&computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
	})
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("emitted envoy config is not valid YAML: %v", err)
	}
	fc := listenerFilterChain(t, parsed)
	if _, present := fc["transport_socket"]; present {
		t.Errorf("client-facing listener has transport_socket with gateway TLS disabled: %v", fc["transport_socket"])
	}
}

// TestBuildEnvoyConfigYAML_GatewayTLSDisabled_ByteIdenticalAcrossReconciles
// mirrors TestBuildEnvoyConfigYAML_EngineTLSDisabled_ByteIdenticalAcrossReconciles:
// the transport_socket %s-placeholder substitution must not leave a stray
// blank line when gateway TLS is disabled, or contentHash(envoyYAML) —
// and therefore AnnotationConfigHash — changes for every existing
// non-TLS instance the moment this feature ships, forcing a gateway
// rollout nobody asked for.
func TestBuildEnvoyConfigYAML_GatewayTLSDisabled_ByteIdenticalAcrossReconciles(t *testing.T) {
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
	}
	first := buildEnvoyConfigYAML(inst)
	if strings.Contains(first, "host_selection_retry_max_attempts: 5\n\n") {
		t.Error("emitted config has a blank line after 'host_selection_retry_max_attempts: 5'; " +
			"buildListenerDownstreamTLSTransportSocket's empty-string case must not leave a stray newline")
	}
	if !strings.Contains(first, "host_selection_retry_max_attempts: 5\n    - name: stats_listener") {
		t.Error("emitted config does not have stats_listener immediately following the retry policy " +
			"with no intervening blank line; got:\n" + first)
	}
	if second := buildEnvoyConfigYAML(inst); first != second {
		t.Error("buildEnvoyConfigYAML is not deterministic across calls with the same instance")
	}
}

// TestBuildEnvoyConfigYAML_GatewayTLSEnabledButNotReady_FailsClosed pins
// down the fail-closed behavior: when gateway TLS is requested but the
// certificate is not provisioned yet (Status.GatewayTLS nil), the
// client-facing listener is omitted entirely rather than served as
// plaintext. Only the always-present stats listener (which answers /healthz
// for the liveness probe) remains, so the pod stays alive while the
// readiness probe — which targets the now-absent client port — keeps the
// gateway NotReady until the certificate lands.
func TestBuildEnvoyConfigYAML_GatewayTLSEnabledButNotReady_FailsClosed(t *testing.T) {
	got := buildEnvoyConfigYAML(&computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			Gateway: computev1alpha1.GatewaySpec{MetricsPort: 9090},
			TLS: &computev1alpha1.TLSSpec{
				Gateway: &computev1alpha1.TLSListenerSpec{Enabled: true},
			},
		},
		// Status.GatewayTLS deliberately left nil: certificate not yet issued.
	})
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("emitted envoy config is not valid YAML: %v", err)
	}
	names := listenerNames(t, parsed)
	if slices.Contains(names, "listener") {
		t.Errorf("client-facing listener present during the fail-closed window; want it omitted (no plaintext). listeners=%v", names)
	}
	if !slices.Contains(names, "stats_listener") {
		t.Errorf("stats_listener missing from fail-closed config; the liveness probe needs its /healthz. listeners=%v", names)
	}
}

// listenerNames returns the "name" of every listener in the emitted config.
func listenerNames(t *testing.T, parsed map[string]any) []string {
	t.Helper()
	listeners := parsed["static_resources"].(map[string]any)["listeners"].([]any)
	var names []string
	for _, l := range listeners {
		if name, _ := l.(map[string]any)["name"].(string); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// TestBuildEnvoyConfigYAML_GatewayTLSReady_TransportSocketConfigured pins
// down the downstream TLS shape once gateway TLS is ready: a
// DownstreamTlsContext presenting the mounted certificate/key pair.
func TestBuildEnvoyConfigYAML_GatewayTLSReady_TransportSocketConfigured(t *testing.T) {
	got := buildEnvoyConfigYAML(&computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			TLS: &computev1alpha1.TLSSpec{
				Gateway: &computev1alpha1.TLSListenerSpec{Enabled: true},
			},
		},
		Status: computev1alpha1.FireboltInstanceStatus{
			GatewayTLS: &computev1alpha1.GatewayTLSStatus{SecretName: "inst-gateway-tls"},
		},
	})
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("emitted envoy config is not valid YAML: %v\n---\n%s", err, got)
	}
	fc := listenerFilterChain(t, parsed)
	ts, ok := fc["transport_socket"].(map[string]any)
	if !ok {
		t.Fatalf("client-facing listener missing transport_socket once GatewayTLS is ready; keys = %v", keysOf(fc))
	}
	if ts["name"] != "envoy.transport_sockets.tls" {
		t.Errorf("transport_socket.name = %v, want envoy.transport_sockets.tls", ts["name"])
	}
	typedConfig, ok := ts["typed_config"].(map[string]any)
	if !ok {
		t.Fatalf("transport_socket.typed_config missing or wrong type: %T", ts["typed_config"])
	}
	if wantType := "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.DownstreamTlsContext"; typedConfig["@type"] != wantType {
		t.Errorf("typed_config[@type] = %v, want %v", typedConfig["@type"], wantType)
	}
	commonTLS, ok := typedConfig["common_tls_context"].(map[string]any)
	if !ok {
		t.Fatalf("typed_config.common_tls_context missing or wrong type: %T", typedConfig["common_tls_context"])
	}
	certs, ok := commonTLS["tls_certificates"].([]any)
	if !ok || len(certs) != 1 {
		t.Fatalf("common_tls_context.tls_certificates = %v, want a 1-element array", commonTLS["tls_certificates"])
	}
	cert, ok := certs[0].(map[string]any)
	if !ok {
		t.Fatalf("tls_certificates[0] = %v, want an object", certs[0])
	}
	chain, ok := cert["certificate_chain"].(map[string]any)
	if !ok {
		t.Fatalf("tls_certificates[0].certificate_chain missing or wrong type: %T", cert["certificate_chain"])
	}
	wantChain := gatewayTLSMountPath + "/" + string(corev1.TLSCertKey)
	if chain["filename"] != wantChain {
		t.Errorf("certificate_chain.filename = %v, want %v", chain["filename"], wantChain)
	}
	key, ok := cert["private_key"].(map[string]any)
	if !ok {
		t.Fatalf("tls_certificates[0].private_key missing or wrong type: %T", cert["private_key"])
	}
	wantKey := gatewayTLSMountPath + "/" + string(corev1.TLSPrivateKeyKey)
	if key["filename"] != wantKey {
		t.Errorf("private_key.filename = %v, want %v", key["filename"], wantKey)
	}
}

// TestBuildEnvoyConfigYAML_GatewayMutualTLS pins down the mTLS shape: when a
// client CA is configured on a ready gateway listener, the DownstreamTlsContext
// requires a client certificate and validates it against the mounted client-CA
// bundle. Without one, neither field is emitted (one-way TLS renders
// byte-identically to before this feature).
func TestBuildEnvoyConfigYAML_GatewayMutualTLS(t *testing.T) {
	build := func(clientCA *corev1.LocalObjectReference) map[string]any {
		got := buildEnvoyConfigYAML(&computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
			Spec: computev1alpha1.FireboltInstanceSpec{
				TLS: &computev1alpha1.TLSSpec{
					Gateway: &computev1alpha1.TLSListenerSpec{Enabled: true, ClientCASecretRef: clientCA},
				},
			},
			Status: computev1alpha1.FireboltInstanceStatus{
				GatewayTLS: &computev1alpha1.GatewayTLSStatus{SecretName: "inst-gateway-tls"},
			},
		})
		var parsed map[string]any
		if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
			t.Fatalf("emitted envoy config is not valid YAML: %v\n---\n%s", err, got)
		}
		return listenerFilterChain(t, parsed)["transport_socket"].(map[string]any)["typed_config"].(map[string]any)
	}

	t.Run("client CA requires and validates client certs", func(t *testing.T) {
		td := build(&corev1.LocalObjectReference{Name: "clients-ca"})
		if req, _ := td["require_client_certificate"].(bool); !req {
			t.Errorf("require_client_certificate = %v, want true", td["require_client_certificate"])
		}
		vc, ok := td["common_tls_context"].(map[string]any)["validation_context"].(map[string]any)
		if !ok {
			t.Fatal("common_tls_context.validation_context missing with a client CA configured")
		}
		want := gatewayClientCAMountPath + "/" + engineTLSCASecretKey
		if got := vc["trusted_ca"].(map[string]any)["filename"]; got != want {
			t.Errorf("trusted_ca.filename = %v, want %v", got, want)
		}
	})

	t.Run("no client CA keeps one-way TLS", func(t *testing.T) {
		td := build(nil)
		if _, present := td["require_client_certificate"]; present {
			t.Error("require_client_certificate present without a client CA configured")
		}
		if _, present := td["common_tls_context"].(map[string]any)["validation_context"]; present {
			t.Error("validation_context present without a client CA configured")
		}
	})
}

// listenerFilterChain returns the first filter_chain of the client-facing
// "listener" (as opposed to the metrics-only "stats_listener", which
// callers here never need).
func listenerFilterChain(t *testing.T, parsed map[string]any) map[string]any {
	t.Helper()
	const listenerName = "listener"
	listeners := parsed["static_resources"].(map[string]any)["listeners"].([]any)
	for _, l := range listeners {
		lm, _ := l.(map[string]any)
		if name, _ := lm["name"].(string); name == listenerName {
			chains := lm["filter_chains"].([]any)
			return chains[0].(map[string]any)
		}
	}
	t.Fatalf("listener %q not found in emitted config", listenerName)
	return nil
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
