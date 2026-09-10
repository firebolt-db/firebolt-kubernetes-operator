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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/wakeagent"
)

const testWakeAgentImage = "ghcr.io/firebolt-db/firebolt-operator:v1.2.3"

func wakeInstance(t *testing.T, template *corev1.PodTemplateSpec) *computev1alpha1.FireboltInstance {
	t.Helper()
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "default"},
	}
	if template != nil {
		inst.Spec.Gateway.Template = template
	}
	return inst
}

func renderGatewayPod(t *testing.T, inst *computev1alpha1.FireboltInstance, cfg wakeAgentConfig) *corev1.PodTemplateSpec {
	t.Helper()
	pt := effectiveGatewayPodTemplate(inst, "fb-gateway-config", "hash", map[string]string{}, cfg)
	return &pt
}

func containerByName(pt *corev1.PodTemplateSpec, name string) *corev1.Container {
	for i := range pt.Spec.Containers {
		if pt.Spec.Containers[i].Name == name {
			return &pt.Spec.Containers[i]
		}
	}
	return nil
}

func volumeByName(pt *corev1.PodTemplateSpec, name string) *corev1.Volume {
	for i := range pt.Spec.Volumes {
		if pt.Spec.Volumes[i].Name == name {
			return &pt.Spec.Volumes[i]
		}
	}
	return nil
}

func TestGatewayPodOmitsWakeAgentWithoutImage(t *testing.T) {
	t.Parallel()
	pt := renderGatewayPod(t, wakeInstance(t, nil), wakeAgentConfig{})

	if c := containerByName(pt, computev1alpha1.GatewayWakeAgentContainerName); c != nil {
		t.Errorf("wake-agent container rendered with no image configured: %+v", c)
	}
	if v := volumeByName(pt, computev1alpha1.GatewayWakeAgentTokenVolumeName); v != nil {
		t.Errorf("wake-agent token volume rendered with no image configured: %+v", v)
	}
}

func TestGatewayPodRendersWakeAgent(t *testing.T) {
	t.Parallel()
	pt := renderGatewayPod(t, wakeInstance(t, nil), wakeAgentConfig{
		Image:           testWakeAgentImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
	})

	agent := containerByName(pt, computev1alpha1.GatewayWakeAgentContainerName)
	if agent == nil {
		t.Fatalf("wake-agent container missing; containers = %v", containerNames(pt))
	}
	if agent.Image != testWakeAgentImage {
		t.Errorf("Image = %q, want %q", agent.Image, testWakeAgentImage)
	}
	if len(agent.Args) == 0 || agent.Args[0] != "wake-agent" {
		t.Errorf("Args = %v, want the wake-agent subcommand first", agent.Args)
	}
	joined := strings.Join(agent.Args, " ")
	if !strings.Contains(joined, fmt.Sprintf("--per-hold-bytes=%d", gatewayPerConnectionBufferLimitBytes)) {
		t.Errorf("Args = %v, want per-hold-bytes matching the listener buffer limit", agent.Args)
	}
	if !strings.Contains(joined, fmt.Sprintf("--envoy-admin-url=http://127.0.0.1:%d", gatewayAdminPort)) {
		t.Errorf("Args = %v, want the Envoy admin URL for the live memory reading", agent.Args)
	}
	if !strings.Contains(joined, fmt.Sprintf("--route-probe-url=http://127.0.0.1:%d/health/ready", gatewayWakeAgentRouteProbePort)) {
		t.Errorf("Args = %v, want the probe URL pointing at the routing_probe listener", agent.Args)
	}
}

func containerNames(pt *corev1.PodTemplateSpec) []string {
	out := make([]string, 0, len(pt.Spec.Containers))
	for i := range pt.Spec.Containers {
		out = append(out, pt.Spec.Containers[i].Name)
	}
	return out
}

// The security property the whole design rests on: Envoy, the process that
// terminates untrusted traffic, must not be able to reach a Kubernetes
// credential. Containers share the network namespace but not the mount
// namespace, so a token projected into the agent alone is invisible to it.
func TestGatewayPodKeepsTokenOutOfEnvoy(t *testing.T) {
	t.Parallel()
	pt := renderGatewayPod(t, wakeInstance(t, nil), wakeAgentConfig{Image: testWakeAgentImage})

	if pt.Spec.AutomountServiceAccountToken == nil || *pt.Spec.AutomountServiceAccountToken {
		t.Errorf("AutomountServiceAccountToken = %v, want *false", pt.Spec.AutomountServiceAccountToken)
	}

	envoy := containerByName(pt, computev1alpha1.GatewayContainerName)
	if envoy == nil {
		t.Fatal("envoy container missing")
	}
	for _, m := range envoy.VolumeMounts {
		if m.Name == computev1alpha1.GatewayWakeAgentTokenVolumeName || strings.HasPrefix(m.MountPath, serviceAccountTokenMountPath) {
			t.Errorf("envoy mounts the ServiceAccount token at %s; it must never hold a credential", m.MountPath)
		}
	}

	agent := containerByName(pt, computev1alpha1.GatewayWakeAgentContainerName)
	if agent == nil {
		t.Fatal("wake-agent container missing")
	}
	var mounted bool
	for _, m := range agent.VolumeMounts {
		if m.Name == computev1alpha1.GatewayWakeAgentTokenVolumeName {
			mounted = true
			if m.MountPath != serviceAccountTokenMountPath {
				t.Errorf("token MountPath = %q, want %q", m.MountPath, serviceAccountTokenMountPath)
			}
			if !m.ReadOnly {
				t.Error("token mount is writable, want read-only")
			}
		}
	}
	if !mounted {
		t.Error("wake-agent does not mount the token it needs to watch EndpointSlices")
	}
}

// A projected token rather than the legacy kubelet mount: it rotates, and
// client-go re-reads the file. The three sources reproduce what automount
// would have provided, so rest.InClusterConfig still works.
func TestGatewayPodProjectsRotatingToken(t *testing.T) {
	t.Parallel()
	pt := renderGatewayPod(t, wakeInstance(t, nil), wakeAgentConfig{Image: testWakeAgentImage})

	vol := volumeByName(pt, computev1alpha1.GatewayWakeAgentTokenVolumeName)
	if vol == nil {
		t.Fatal("wake-agent token volume missing")
	}
	if vol.Projected == nil {
		t.Fatalf("token volume is not projected: %+v", vol.VolumeSource)
	}

	var sawToken, sawCA, sawNamespace bool
	for _, src := range vol.Projected.Sources {
		switch {
		case src.ServiceAccountToken != nil:
			sawToken = true
			if src.ServiceAccountToken.ExpirationSeconds == nil {
				t.Error("projected token has no expiry, so it would not rotate")
			}
			if src.ServiceAccountToken.Path != "token" {
				t.Errorf("token path = %q, want \"token\"", src.ServiceAccountToken.Path)
			}
		case src.ConfigMap != nil:
			sawCA = true
		case src.DownwardAPI != nil:
			sawNamespace = true
		}
	}
	if !sawToken || !sawCA || !sawNamespace {
		t.Errorf("projected sources incomplete (token=%v ca=%v namespace=%v); "+
			"rest.InClusterConfig needs token and ca.crt on disk", sawToken, sawCA, sawNamespace)
	}
}

// A user who brings their own ServiceAccount owns their pod's credential
// story, sidecars included, so the operator must not override their choice.
func TestGatewayPodHonorsUserAutomountWithCustomSA(t *testing.T) {
	t.Parallel()
	userValue := true
	inst := wakeInstance(t, &corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			ServiceAccountName:           "my-gateway-sa",
			AutomountServiceAccountToken: &userValue,
		},
	})
	pt := renderGatewayPod(t, inst, wakeAgentConfig{Image: testWakeAgentImage})

	if pt.Spec.AutomountServiceAccountToken == nil || !*pt.Spec.AutomountServiceAccountToken {
		t.Errorf("AutomountServiceAccountToken = %v, want the user's *true to pass through",
			pt.Spec.AutomountServiceAccountToken)
	}
}

func TestGatewayPodLeavesAutomountUnsetForUnopinionatedCustomSA(t *testing.T) {
	t.Parallel()
	inst := wakeInstance(t, &corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{ServiceAccountName: "my-gateway-sa"},
	})
	pt := renderGatewayPod(t, inst, wakeAgentConfig{Image: testWakeAgentImage})

	if pt.Spec.AutomountServiceAccountToken != nil {
		t.Errorf("AutomountServiceAccountToken = %v, want nil so Kubernetes' own default applies",
			*pt.Spec.AutomountServiceAccountToken)
	}
}

// The downward API substitutes node allocatable for a container with no
// memory limit, which would size the hold cap off the whole machine. The
// variable is therefore only set when a real limit exists.
func TestWakeAgentMemoryLimitEnvOnlyWithLimit(t *testing.T) {
	t.Parallel()

	t.Run("absent when envoy has no memory limit", func(t *testing.T) {
		pt := renderGatewayPod(t, wakeInstance(t, nil), wakeAgentConfig{Image: testWakeAgentImage})
		agent := containerByName(pt, computev1alpha1.GatewayWakeAgentContainerName)
		if agent == nil {
			t.Fatal("wake-agent container missing")
		}
		if v := envByName(agent, "ENVOY_MEMORY_LIMIT_BYTES"); v != nil {
			t.Errorf("ENVOY_MEMORY_LIMIT_BYTES set with no limit on envoy: %+v", v)
		}
	})

	t.Run("present and scoped to the envoy container when a limit is set", func(t *testing.T) {
		inst := wakeInstance(t, &corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: computev1alpha1.GatewayContainerName,
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
				}},
			},
		})
		pt := renderGatewayPod(t, inst, wakeAgentConfig{Image: testWakeAgentImage})
		agent := containerByName(pt, computev1alpha1.GatewayWakeAgentContainerName)
		if agent == nil {
			t.Fatal("wake-agent container missing")
		}
		v := envByName(agent, "ENVOY_MEMORY_LIMIT_BYTES")
		if v == nil {
			t.Fatal("ENVOY_MEMORY_LIMIT_BYTES absent despite a limit on envoy")
		}
		if v.ValueFrom == nil || v.ValueFrom.ResourceFieldRef == nil {
			t.Fatalf("ENVOY_MEMORY_LIMIT_BYTES is not a resourceFieldRef: %+v", v)
		}
		ref := v.ValueFrom.ResourceFieldRef
		if ref.ContainerName != computev1alpha1.GatewayContainerName {
			t.Errorf("ContainerName = %q, want %q — the memory at risk is Envoy's, not the agent's",
				ref.ContainerName, computev1alpha1.GatewayContainerName)
		}
		if ref.Resource != "limits.memory" {
			t.Errorf("Resource = %q, want limits.memory", ref.Resource)
		}
	})
}

func envByName(c *corev1.Container, name string) *corev1.EnvVar {
	for i := range c.Env {
		if c.Env[i].Name == name {
			return &c.Env[i]
		}
	}
	return nil
}

// No liveness probe, deliberately: restarting a wedged agent would reset
// every request it is holding, turning a degraded wake into client-visible
// errors. Envoy fails open, so an unready agent costs wake, not routing.
// The sidecar must carry no probes.
//
// A readiness probe is the dangerous one: a pod is Ready only when EVERY
// container is, so probing the agent would let a wedged one evict the whole
// gateway pod from its Service and take Envoy with it — the outage the
// fail-open design exists to prevent, arriving through the back door. An
// earlier revision had exactly that, and this test asserted its presence.
//
// A liveness probe would restart the agent and reset every request it is
// holding, turning a degraded wake into client-visible errors.
func TestWakeAgentHasNoProbes(t *testing.T) {
	t.Parallel()
	pt := renderGatewayPod(t, wakeInstance(t, nil), wakeAgentConfig{Image: testWakeAgentImage})
	agent := containerByName(pt, computev1alpha1.GatewayWakeAgentContainerName)
	if agent == nil {
		t.Fatal("wake-agent container missing")
	}
	if agent.LivenessProbe != nil {
		t.Error("wake-agent has a liveness probe; a restart would reset held requests")
	}
	if agent.ReadinessProbe != nil {
		t.Error("wake-agent has a readiness probe; an unready agent would take " +
			"the gateway pod out of its Service and break the data path")
	}
	if agent.StartupProbe != nil {
		t.Error("wake-agent has a startup probe; same failure mode as readiness")
	}
}

func TestWakeAgentRunsLockedDown(t *testing.T) {
	t.Parallel()
	pt := renderGatewayPod(t, wakeInstance(t, nil), wakeAgentConfig{Image: testWakeAgentImage})
	agent := containerByName(pt, computev1alpha1.GatewayWakeAgentContainerName)
	if agent == nil {
		t.Fatal("wake-agent container missing")
	}
	sc := agent.SecurityContext
	if sc == nil {
		t.Fatal("wake-agent has no SecurityContext")
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("RunAsNonRoot is not true")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("ReadOnlyRootFilesystem is not true")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation is not false")
	}
}

// The Envoy half of the contract: a loopback cluster for the agent, and a
// Lua call that consults it before the :authority rewrite sends the request
// at an engine that may not be there.
func TestEnvoyConfigCallsWakeAgent(t *testing.T) {
	t.Parallel()
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "default"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			Gateway: computev1alpha1.GatewaySpec{MetricsPort: 9090},
		},
	}
	cfg := buildEnvoyConfigYAML(inst, true)

	if !strings.Contains(cfg, "- name: wake_agent") {
		t.Error("envoy config has no wake_agent cluster")
	}
	if !strings.Contains(cfg, fmt.Sprintf("port_value: %d", gatewayWakeAgentHoldPort)) {
		t.Errorf("wake_agent cluster does not point at the hold port %d", gatewayWakeAgentHoldPort)
	}
	if !strings.Contains(cfg, `handle:httpCall(`) || !strings.Contains(cfg, `"wake_agent"`) {
		t.Error("Lua filter does not call the wake agent")
	}
	if !strings.Contains(cfg, "/hold?engine=") {
		t.Error("Lua filter does not pass the engine to the hold endpoint")
	}
}

// Envoy invalidates the Lua header object across the coroutine yield that
// httpCall performs. A headers:replace() after the call raises "object used
// outside of proper scope", which the client never sees: :authority is left
// unrewritten and the query is routed straight back at the gateway. Every
// header mutation must therefore be ordered BEFORE the hold.
//
// This inverts an assertion an earlier revision of this test made, which is
// how the bug shipped past unit coverage in the first place — nothing but a
// live Envoy could tell the two orderings apart.
func TestWakeHoldRunsAfterHeaderRewrites(t *testing.T) {
	t.Parallel()
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "default"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			Gateway: computev1alpha1.GatewaySpec{MetricsPort: 9090},
		},
	}
	cfg := buildEnvoyConfigYAML(inst, true)

	holdAt := strings.Index(cfg, "/hold?engine=")
	if holdAt < 0 {
		t.Fatal("wake hold absent from the rendered config")
	}
	for _, rewrite := range []string{
		`headers:replace(":authority"`,
		`headers:replace("x-firebolt-engine"`,
		`headers:replace(":path"`,
	} {
		at := strings.Index(cfg, rewrite)
		if at < 0 {
			t.Errorf("%s absent from the rendered config", rewrite)
			continue
		}
		if at > holdAt {
			t.Errorf("%s at %d runs after the wake hold at %d; "+
				"the header object is invalid past the httpCall yield",
				rewrite, at, holdAt)
		}
	}
}

// Rendering the hold without an agent behind it is worse than not having the
// feature: Envoy synthesizes a 503 for a refused loopback connection, so
// every query would be answered from a port with nothing listening on it.
func TestEnvoyConfigOmitsWakeWhenDisabled(t *testing.T) {
	t.Parallel()
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "default"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			Gateway: computev1alpha1.GatewaySpec{MetricsPort: 9090},
		},
	}
	cfg := buildEnvoyConfigYAML(inst, false)

	for _, fragment := range []string{"wake_agent", "/hold?engine=", "httpCall"} {
		if strings.Contains(cfg, fragment) {
			t.Errorf("wake-disabled config still contains %q", fragment)
		}
	}
	// The rest of the filter chain must be intact.
	if !strings.Contains(cfg, `headers:replace(":authority"`) {
		t.Error("wake-disabled config lost the :authority rewrite")
	}
}

// A non-200 is only honored when it carries the agent's own decision
// header. Envoy's synthesized failure response has no such header, which is
// what makes an unreachable agent fall through to normal routing instead of
// taking the gateway down.
func TestEnvoyConfigFailsOpenOnSynthesizedResponse(t *testing.T) {
	t.Parallel()
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "default"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			Gateway: computev1alpha1.GatewaySpec{MetricsPort: 9090},
		},
	}
	cfg := buildEnvoyConfigYAML(inst, true)

	guard := fmt.Sprintf(`wake_headers[%q] ~= nil`, wakeagent.DecisionHeader)
	if !strings.Contains(cfg, guard) {
		t.Errorf("Lua does not gate on the decision header; expected %q.\n"+
			"Without it, Envoy's synthesized 503 for an unreachable agent is "+
			"indistinguishable from a deliberate shed and every query fails.", guard)
	}
	statusCheck := strings.Index(cfg, `wake_headers[":status"] ~= "200"`)
	guardAt := strings.Index(cfg, guard)
	if statusCheck < 0 || guardAt < 0 || guardAt > statusCheck {
		t.Error("the decision-header guard must be evaluated before the status check")
	}
}

// The Lua timeout must outlast the agent's own hold timeout, so a
// never-arriving engine surfaces the agent's 503 rather than an opaque
// Lua-side timeout.
func TestWakeHoldTimeoutOutlastsAgent(t *testing.T) {
	t.Parallel()
	luaTimeout := time.Duration(gatewayWakeHoldTimeoutMillis) * time.Millisecond
	if luaTimeout <= wakeagent.DefaultHoldTimeout {
		t.Errorf("Lua httpCall timeout %v must exceed the agent's hold timeout %v",
			luaTimeout, wakeagent.DefaultHoldTimeout)
	}
}

// The routability half of the release condition: a loopback listener that
// forwards the agent's probe to the dynamic_forward_proxy cluster, so the
// probe exercises the same DNS cache, transport socket, and health view the
// released query will. Present exactly when the wake hold is.
func TestEnvoyConfigRoutingProbeListener(t *testing.T) {
	t.Parallel()
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "default"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			Gateway: computev1alpha1.GatewaySpec{MetricsPort: 9090},
		},
	}
	cfg := buildEnvoyConfigYAML(inst, true)

	if !strings.Contains(cfg, "- name: routing_probe") {
		t.Error("wake-enabled config has no routing_probe listener")
	}
	if !strings.Contains(cfg, fmt.Sprintf("port_value: %d", gatewayWakeAgentRouteProbePort)) {
		t.Errorf("routing_probe listener does not bind port %d", gatewayWakeAgentRouteProbePort)
	}
	var out map[string]any
	if err := yaml.Unmarshal([]byte(cfg), &out); err != nil {
		t.Fatalf("wake-enabled envoy config is not valid YAML: %v\n---\n%s", err, cfg)
	}
	// Walk the parsed listener rather than string-matching the whole config:
	// the admin interface is also loopback-bound, so only a per-listener
	// assertion proves THIS listener cannot land on 0.0.0.0. With
	// domains ["*"] over the dynamic-forward-proxy cluster, a pod-IP bind
	// would be a Host-keyed open proxy.
	probe := findListenerByName(t, out, "routing_probe")
	address := dig[map[string]any](t, probe, "address", "socket_address")
	if got := address["address"]; got != "127.0.0.1" {
		t.Errorf("routing_probe binds %v, want 127.0.0.1", got)
	}
	if got := address["port_value"]; fmt.Sprintf("%v", got) != fmt.Sprintf("%d", gatewayWakeAgentRouteProbePort) {
		t.Errorf("routing_probe port = %v, want %d", got, gatewayWakeAgentRouteProbePort)
	}
	routes := dig[[]any](t, probe, "filter_chains", "0", "filters", "0", "typed_config", "route_config", "virtual_hosts", "0", "routes")
	if len(routes) != 1 {
		t.Fatalf("routing_probe has %d routes, want exactly the probe route", len(routes))
	}
	route, _ := routes[0].(map[string]any)
	if got := dig[map[string]any](t, route, "match")["path"]; got != "/health/ready" {
		t.Errorf("routing_probe route path = %v, want /health/ready", got)
	}
	if got := dig[map[string]any](t, route, "route")["cluster"]; got != "dynamic_forward_proxy" {
		t.Errorf("routing_probe route cluster = %v, want dynamic_forward_proxy; "+
			"probing any other cluster would not observe the sub-cluster queries use", got)
	}

	disabled := buildEnvoyConfigYAML(inst, false)
	for _, fragment := range []string{"routing_probe", fmt.Sprintf("port_value: %d", gatewayWakeAgentRouteProbePort)} {
		if strings.Contains(disabled, fragment) {
			t.Errorf("wake-disabled config still contains %q", fragment)
		}
	}
}

// The agent's default probe URL and the operator-rendered listener are two
// halves of one loopback contract; pin them together.
func TestWakeAgentRouteProbeURLMatchesListener(t *testing.T) {
	t.Parallel()
	want := fmt.Sprintf("http://127.0.0.1:%d/health/ready", gatewayWakeAgentRouteProbePort)
	if wakeagent.DefaultRouteProbeURL != want {
		t.Errorf("wakeagent.DefaultRouteProbeURL = %q, want %q; "+
			"the agent would probe a port the routing_probe listener does not serve",
			wakeagent.DefaultRouteProbeURL, want)
	}
}

// The agent duplicates the engine query port rather than importing it, for
// the same dependency reason as the service suffix below; pin the two
// together so a port change cannot leave the probe authority behind.
func TestWakeAgentEngineQueryPortMatches(t *testing.T) {
	t.Parallel()
	if wakeagent.EngineHTTPQueryPort != EngineHTTPQueryPort {
		t.Errorf("wakeagent.EngineHTTPQueryPort = %d, controller.EngineHTTPQueryPort = %d; "+
			"the probe would key a different DFP sub-cluster than queries use",
			wakeagent.EngineHTTPQueryPort, EngineHTTPQueryPort)
	}
}

// The probe only observes the right sub-cluster if its Host is byte-for-byte
// the :authority the Lua filter rewrites queries to. Pin the agent's
// authority construction against the rendered rewrite.
func TestWakeAgentAuthorityMatchesLuaRewrite(t *testing.T) {
	t.Parallel()
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "ns-1"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			Gateway: computev1alpha1.GatewaySpec{MetricsPort: 9090},
		},
	}
	cfg := buildEnvoyConfigYAML(inst, true)

	suffix := strings.TrimPrefix(wakeagent.EngineAuthority("e", "ns-1"), "e")
	want := `headers:replace(":authority", engine .. "` + suffix + `")`
	if !strings.Contains(cfg, want) {
		t.Errorf("rendered config does not rewrite :authority to the agent's probe authority; want %q", want)
	}
}

// The agent duplicates the service suffix rather than importing it, to keep
// controller-runtime out of the sidecar's dependency graph. Pin the two
// together so the duplication cannot silently drift.
func TestWakeAgentServiceSuffixMatches(t *testing.T) {
	t.Parallel()
	if wakeagent.ServiceSuffix != SuffixService {
		t.Errorf("wakeagent.ServiceSuffix = %q, controller.SuffixService = %q; "+
			"the agent would derive engine names from the wrong Service naming",
			wakeagent.ServiceSuffix, SuffixService)
	}
}

// Three timeouts have to stay ordered or a held query dies in the wrong
// place: the agent's own hold ceiling, the Lua httpCall bounding it, and
// the stream idle timeout bounding both. Getting this wrong surfaces as a
// stream reset instead of the agent's 503 + Retry-After, which is much
// harder to diagnose from a client.
func TestWakeHoldFitsInsideStreamIdleTimeout(t *testing.T) {
	t.Parallel()
	agentHold := wakeagent.DefaultHoldTimeout
	luaCall := time.Duration(gatewayWakeHoldTimeoutMillis) * time.Millisecond
	streamIdle := time.Duration(gatewayStreamIdleTimeoutSeconds) * time.Second

	if luaCall <= agentHold {
		t.Errorf("Lua httpCall timeout %v must exceed the agent's hold %v, "+
			"so a never-arriving engine surfaces the agent's 503", luaCall, agentHold)
	}
	if streamIdle <= luaCall {
		t.Errorf("stream_idle_timeout %v must exceed the Lua httpCall timeout %v, "+
			"or Envoy resets the held stream before the filter can answer", streamIdle, luaCall)
	}
}

// The value must actually reach the rendered config, not just the constant.
func TestEnvoyConfigSetsStreamIdleTimeout(t *testing.T) {
	t.Parallel()
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "default"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			Gateway: computev1alpha1.GatewaySpec{MetricsPort: 9090},
		},
	}
	want := fmt.Sprintf("stream_idle_timeout: %ds", gatewayStreamIdleTimeoutSeconds)
	if got := buildEnvoyConfigYAML(inst, true); !strings.Contains(got, want) {
		t.Errorf("rendered config missing %q", want)
	}
}

// findListenerByName walks static_resources.listeners of a parsed envoy
// config and fails the test when the named listener is absent.
func findListenerByName(t *testing.T, cfg map[string]any, name string) map[string]any {
	t.Helper()
	listeners := dig[[]any](t, cfg, "static_resources", "listeners")
	for _, item := range listeners {
		l, _ := item.(map[string]any)
		if l["name"] == name {
			return l
		}
	}
	t.Fatalf("listener %q not found", name)
	return nil
}

// dig walks nested maps and slices by key (numeric strings index slices) and
// fails the test on the first missing step, naming the path walked so far.
func dig[T any](t *testing.T, node any, path ...string) T {
	t.Helper()
	current := node
	for i, step := range path {
		switch typed := current.(type) {
		case map[string]any:
			next, ok := typed[step]
			if !ok {
				t.Fatalf("missing %q at %v", step, path[:i+1])
			}
			current = next
		case []any:
			index := 0
			if _, err := fmt.Sscanf(step, "%d", &index); err != nil || index >= len(typed) {
				t.Fatalf("bad index %q at %v", step, path[:i+1])
			}
			current = typed[index]
		default:
			t.Fatalf("cannot descend into %T at %v", current, path[:i+1])
		}
	}
	out, ok := current.(T)
	if !ok {
		t.Fatalf("value at %v is %T", path, current)
	}
	return out
}
