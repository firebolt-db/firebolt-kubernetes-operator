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
	for i := range pt.Spec.InitContainers {
		if pt.Spec.InitContainers[i].Name == name {
			return &pt.Spec.InitContainers[i]
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

func TestGatewayPodWithoutAgentImageCannotBypassAdmission(t *testing.T) {
	t.Parallel()
	pt := renderGatewayPod(t, wakeInstance(t, nil), wakeAgentConfig{})
	if !strings.Contains(buildEnvoyConfigYAML(wakeInstance(t, nil), false), "failure_mode_allow: false") {
		t.Fatal("an unconfigured agent must fail closed, never bypass admission")
	}

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

// A custom ServiceAccount still projects credentials only into the agent.
func TestGatewayPodDisablesAutomountWithCustomSA(t *testing.T) {
	t.Parallel()
	boolPointer := func(value bool) *bool { return &value }
	for _, userValue := range []*bool{nil, boolPointer(true), boolPointer(false)} {
		inst := wakeInstance(t, &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			ServiceAccountName: "my-gateway-sa", AutomountServiceAccountToken: userValue,
		}})
		pt := renderGatewayPod(t, inst, wakeAgentConfig{Image: testWakeAgentImage})
		if pt.Spec.AutomountServiceAccountToken == nil || *pt.Spec.AutomountServiceAccountToken {
			t.Fatal("custom ServiceAccount must not expose its credential to Envoy")
		}
		if containerByName(pt, computev1alpha1.GatewayWakeAgentContainerName) == nil {
			t.Fatal("custom ServiceAccount must retain the routing agent")
		}
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

// Readiness requires durable registration; liveness must not erase accounting.
func TestWakeAgentRequiresRegisteredReadiness(t *testing.T) {
	t.Parallel()
	pt := renderGatewayPod(t, wakeInstance(t, nil), wakeAgentConfig{Image: testWakeAgentImage})
	agent := containerByName(pt, computev1alpha1.GatewayWakeAgentContainerName)
	if agent == nil || agent.ReadinessProbe == nil || agent.ReadinessProbe.HTTPGet.Path != "/readyz" {
		t.Fatal("agent must gate readiness on registration")
	}
	if agent.LivenessProbe != nil {
		t.Fatal("liveness restart would erase accounting")
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

// The admission filter must run after engine validation and before DNS routing.
// Response headers stay enabled so Envoy reports the request lifetime, including
// cancellation, instead of closing the processor immediately after admission.
func TestEnvoyConfigTracksAdmissionBeforeRouting(t *testing.T) {
	t.Parallel()
	cfg := buildEnvoyConfigYAML(wakeInstance(t, nil), true)
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(cfg), &parsed); err != nil {
		t.Fatal(err)
	}
	chain := listenerFilterChain(t, parsed)
	filters := chain["filters"].([]any)[0].(map[string]any)["typed_config"].(map[string]any)["http_filters"].([]any)
	want := []string{"envoy.filters.http.health_check", "envoy.filters.http.lua", "envoy.filters.http.ext_proc", "envoy.filters.http.dynamic_forward_proxy", "envoy.filters.http.router"}
	if len(filters) != len(want) {
		t.Fatalf("HTTP filters = %v", filters)
	}
	for i, name := range want {
		if filters[i].(map[string]any)["name"] != name {
			t.Fatalf("filter %d must be %s", i, name)
		}
	}
	processor := filters[2].(map[string]any)["typed_config"].(map[string]any)
	if failOpen, ok := processor["failure_mode_allow"].(bool); !ok || failOpen {
		t.Fatal("processor transport failures must not bypass admission")
	}
	mode := processor["processing_mode"].(map[string]any)
	if mode["request_header_mode"] != "SEND" || mode["response_header_mode"] != "SEND" {
		t.Fatal("processor must observe both admission and response headers")
	}
	for _, body := range []string{"request_body_mode", "response_body_mode"} {
		if mode[body] != "NONE" {
			t.Fatalf("%s must keep SQL and result bodies out of the agent", body)
		}
	}
	if strings.Contains(cfg, `headers:replace(":authority"`) || strings.Contains(cfg, "/hold?engine=") {
		t.Fatal("Lua must not assign an engine authority or perform an untracked hold")
	}
	grpc := processor["grpc_service"].(map[string]any)["envoy_grpc"].(map[string]any)
	if grpc["cluster_name"] != "routing_agent" {
		t.Fatal("external processor must target the routing agent")
	}
}

func TestEnvoyAdmissionCannotBeDisabled(t *testing.T) {
	t.Parallel()
	inst := wakeInstance(t, nil)
	for _, enabled := range []bool{false, true} {
		cfg := buildEnvoyConfigYAML(inst, enabled)
		for _, required := range []string{"name: envoy.filters.http.ext_proc", "failure_mode_allow: false", "response_header_mode: SEND", "ext_proc_graceful_grpc_close: true"} {
			if !strings.Contains(cfg, required) {
				t.Errorf("missing %q", required)
			}
		}
		if strings.Contains(cfg, "/hold?engine=") {
			t.Fatal("legacy hold path remains")
		}
	}
}

// The rendered processor deadline must let the agent return its bounded wake
// rejection before Envoy's processor or stream-idle timeout resets the request.
func TestAdmissionTimeoutFitsInsideStreamIdleTimeout(t *testing.T) {
	t.Parallel()
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(buildEnvoyConfigYAML(wakeInstance(t, nil), true)), &parsed); err != nil {
		t.Fatal(err)
	}
	hcm := listenerFilterChain(t, parsed)["filters"].([]any)[0].(map[string]any)["typed_config"].(map[string]any)
	processor := hcm["http_filters"].([]any)[2].(map[string]any)["typed_config"].(map[string]any)
	deadline, err := time.ParseDuration(processor["message_timeout"].(string))
	if err != nil {
		t.Fatal(err)
	}
	idle, err := time.ParseDuration(hcm["stream_idle_timeout"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if deadline <= wakeagent.DefaultHoldTimeout || idle <= deadline {
		t.Fatalf("timeouts must satisfy hold < processor < idle, got %s < %s < %s", wakeagent.DefaultHoldTimeout, deadline, idle)
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

func TestGatewayPodNativeSidecarOrderAndGracePeriod(t *testing.T) {
	t.Parallel()
	seconds := int64(240)
	template := &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		InitContainers:                []corev1.Container{{Name: "prepare", Image: "busybox:1"}},
		Containers:                    []corev1.Container{{Name: "exporter", Image: "busybox:1"}},
		TerminationGracePeriodSeconds: &seconds,
	}}
	pt := renderGatewayPod(t, wakeInstance(t, template), wakeAgentConfig{Image: testWakeAgentImage})
	if len(pt.Spec.InitContainers) != 2 || pt.Spec.InitContainers[0].Name != "prepare" || pt.Spec.InitContainers[1].Name != computev1alpha1.GatewayWakeAgentContainerName {
		t.Fatalf("init sequence = %+v", pt.Spec.InitContainers)
	}
	agent := pt.Spec.InitContainers[1]
	if agent.RestartPolicy == nil || *agent.RestartPolicy != corev1.ContainerRestartPolicyAlways || agent.Lifecycle != nil {
		t.Fatalf("agent must use native sidecar ordering without a sleep hook: %+v", agent)
	}
	if len(pt.Spec.Containers) != 2 || pt.Spec.Containers[0].Name != computev1alpha1.GatewayContainerName || pt.Spec.Containers[1].Name != "exporter" {
		t.Fatalf("main containers = %+v", pt.Spec.Containers)
	}
	if *pt.Spec.TerminationGracePeriodSeconds != seconds {
		t.Fatalf("grace period = %d", *pt.Spec.TerminationGracePeriodSeconds)
	}
	if len(template.Spec.InitContainers) != 1 {
		t.Fatal("rendering modified the input template")
	}
	defaults := renderGatewayPod(t, wakeInstance(t, nil), wakeAgentConfig{})
	if *defaults.Spec.TerminationGracePeriodSeconds != gatewayTerminationGracePeriodSeconds {
		t.Fatal("default shutdown deadline was not applied")
	}
}
