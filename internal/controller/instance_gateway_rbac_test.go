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
	"encoding/json"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/routing"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// A user-supplied account controls the bound identity. The operator still
// grants its narrowly scoped read-only routing permissions.
func TestUserGatewayServiceAccountName(t *testing.T) {
	mk := func(template *corev1.PodTemplateSpec) *computev1alpha1.FireboltInstance {
		return &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "default"},
			Spec: computev1alpha1.FireboltInstanceSpec{
				Gateway: computev1alpha1.GatewaySpec{Template: template},
			},
		}
	}

	t.Run("nil template returns empty", func(t *testing.T) {
		if got := userGatewayServiceAccountName(mk(nil)); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("empty template returns empty", func(t *testing.T) {
		if got := userGatewayServiceAccountName(mk(&corev1.PodTemplateSpec{})); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("template with empty serviceAccountName returns empty", func(t *testing.T) {
		inst := mk(&corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: ""}})
		if got := userGatewayServiceAccountName(inst); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("template with user serviceAccountName returns it verbatim", func(t *testing.T) {
		inst := mk(&corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: "my-gateway-sa"}})
		if got := userGatewayServiceAccountName(inst); got != "my-gateway-sa" {
			t.Errorf("got %q, want my-gateway-sa", got)
		}
	})
}

// The gateway uses either the supplied account or the managed default account.
func TestEffectiveGatewayPodTemplate_ServiceAccountFallback(t *testing.T) {
	envoyYAML := "" // contents don't matter for SA assertion
	baseLabels := map[string]string{"firebolt.io/instance": "fb"}

	t.Run("operator-managed SA when user did not set it", func(t *testing.T) {
		inst := &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "default"},
		}
		pt := effectiveGatewayPodTemplate(inst, "fb-gateway-config", contentHash(envoyYAML), baseLabels, wakeAgentConfig{})
		want := gatewayServiceAccountName(inst.Name)
		if pt.Spec.ServiceAccountName != want {
			t.Errorf("ServiceAccountName = %q, want %q (operator-managed default)", pt.Spec.ServiceAccountName, want)
		}
	})

	t.Run("user SA passes through when set", func(t *testing.T) {
		inst := &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "default"},
			Spec: computev1alpha1.FireboltInstanceSpec{
				Gateway: computev1alpha1.GatewaySpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{ServiceAccountName: "my-gateway-sa"},
					},
				},
			},
		}
		pt := effectiveGatewayPodTemplate(inst, "fb-gateway-config", contentHash(envoyYAML), baseLabels, wakeAgentConfig{})
		if pt.Spec.ServiceAccountName != "my-gateway-sa" {
			t.Errorf("ServiceAccountName = %q, want my-gateway-sa", pt.Spec.ServiceAccountName)
		}
	})
}

// TestEffectiveGatewayPodTemplate_EngineCAVolume pins down that the
// engine-CA volume/mount (needed for buildDFPUpstreamTLSTransportSocket's
// trusted_ca) is wired only once engine TLS is enabled AND ready, and
// points at the same Secret instance.Status.EngineTLS names.
func TestEffectiveGatewayPodTemplate_EngineCAVolume(t *testing.T) {
	baseLabels := map[string]string{"firebolt.io/instance": "fb"}
	findVol := func(pt corev1.PodTemplateSpec, name string) *corev1.Volume {
		for i := range pt.Spec.Volumes {
			if pt.Spec.Volumes[i].Name == name {
				return &pt.Spec.Volumes[i]
			}
		}
		return nil
	}
	findMount := func(pt corev1.PodTemplateSpec, name string) *corev1.VolumeMount {
		for i := range pt.Spec.Containers[0].VolumeMounts {
			if pt.Spec.Containers[0].VolumeMounts[i].Name == name {
				return &pt.Spec.Containers[0].VolumeMounts[i]
			}
		}
		return nil
	}

	t.Run("absent when engine TLS is disabled", func(t *testing.T) {
		inst := &computev1alpha1.FireboltInstance{ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "default"}}
		pt := effectiveGatewayPodTemplate(inst, "fb-gateway-config", "", baseLabels, wakeAgentConfig{})
		if v := findVol(pt, computev1alpha1.GatewayEngineCAVolumeName); v != nil {
			t.Errorf("unexpected engine-CA volume with TLS disabled: %+v", v)
		}
		if m := findMount(pt, computev1alpha1.GatewayEngineCAVolumeName); m != nil {
			t.Errorf("unexpected engine-CA mount with TLS disabled: %+v", m)
		}
	})

	t.Run("absent when engine TLS is enabled but not yet ready", func(t *testing.T) {
		inst := &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "default"},
			Spec: computev1alpha1.FireboltInstanceSpec{
				TLS: &computev1alpha1.TLSSpec{Engine: &computev1alpha1.TLSListenerSpec{Enabled: true}},
			},
		}
		pt := effectiveGatewayPodTemplate(inst, "fb-gateway-config", "", baseLabels, wakeAgentConfig{})
		if v := findVol(pt, computev1alpha1.GatewayEngineCAVolumeName); v != nil {
			t.Errorf("unexpected engine-CA volume before EngineTLS is ready: %+v", v)
		}
	})

	t.Run("wired once ready", func(t *testing.T) {
		inst := &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "default"},
			Spec: computev1alpha1.FireboltInstanceSpec{
				TLS: &computev1alpha1.TLSSpec{Engine: &computev1alpha1.TLSListenerSpec{Enabled: true}},
			},
			Status: computev1alpha1.FireboltInstanceStatus{
				// Reencrypting=true is the signal the gateway render gates on
				// (engineUpstreamTLSReady): the fleet has converged on TLS.
				EngineTLS: &computev1alpha1.EngineTLSStatus{SecretName: "fb-engine-tls", Reencrypting: true},
			},
		}
		pt := effectiveGatewayPodTemplate(inst, "fb-gateway-config", "", baseLabels, wakeAgentConfig{})
		v := findVol(pt, computev1alpha1.GatewayEngineCAVolumeName)
		// The gateway mounts the operator-assembled trust BUNDLE, not
		// the anchor Secret directly, so it can trust every live generation's CA.
		if v == nil || v.Secret == nil || v.Secret.SecretName != engineCABundleSecretName("fb") {
			t.Errorf("engine-CA volume = %+v, want Secret.SecretName=%s", v, engineCABundleSecretName("fb"))
		}
		// Least-privilege: only ca.crt is projected, so no private key
		// (tls.key) ever lands in the gateway pod.
		if v != nil && v.Secret != nil {
			if len(v.Secret.Items) != 1 || v.Secret.Items[0].Key != engineTLSCASecretKey || v.Secret.Items[0].Path != engineTLSCASecretKey {
				t.Errorf("engine-CA volume Items = %+v, want exactly [{Key:%s Path:%s}]", v.Secret.Items, engineTLSCASecretKey, engineTLSCASecretKey)
			}
		}
		m := findMount(pt, computev1alpha1.GatewayEngineCAVolumeName)
		if m == nil || m.MountPath != gatewayEngineCAMountPath || !m.ReadOnly {
			t.Errorf("engine-CA mount = %+v, want MountPath=%s ReadOnly=true", m, gatewayEngineCAMountPath)
		}
	})
}

// TestEffectiveGatewayPodTemplate_GatewayTLSVolumeAndProbeScheme pins
// down two things that must change together once gateway TLS is enabled
// AND ready: the tls-gateway volume/mount pointing at
// instance.Status.GatewayTLS's Secret, and the kubelet liveness/readiness
// probe scheme flipping from HTTP to HTTPS. A probe still speaking HTTP
// against a TLS-only listener would fail every probe and leave the
// gateway forever un-Ready — the direct analog of
// TestBuildStatefulSet_TLSEnabled_WebSidecarBackendSwitchesToHTTPS from
// Phase 2's engine web-UI sidecar.
func TestEffectiveGatewayPodTemplate_GatewayTLSVolumeAndProbeScheme(t *testing.T) {
	baseLabels := map[string]string{"firebolt.io/instance": "fb"}
	findVol := func(pt corev1.PodTemplateSpec, name string) *corev1.Volume {
		for i := range pt.Spec.Volumes {
			if pt.Spec.Volumes[i].Name == name {
				return &pt.Spec.Volumes[i]
			}
		}
		return nil
	}
	findMount := func(pt corev1.PodTemplateSpec, name string) *corev1.VolumeMount {
		for i := range pt.Spec.Containers[0].VolumeMounts {
			if pt.Spec.Containers[0].VolumeMounts[i].Name == name {
				return &pt.Spec.Containers[0].VolumeMounts[i]
			}
		}
		return nil
	}
	probeSchemes := func(pt corev1.PodTemplateSpec) (liveness, readiness corev1.URIScheme) {
		c := pt.Spec.Containers[0]
		return c.LivenessProbe.HTTPGet.Scheme, c.ReadinessProbe.HTTPGet.Scheme
	}

	t.Run("absent and HTTP when gateway TLS is disabled", func(t *testing.T) {
		inst := &computev1alpha1.FireboltInstance{ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "default"}}
		pt := effectiveGatewayPodTemplate(inst, "fb-gateway-config", "", baseLabels, wakeAgentConfig{})
		if v := findVol(pt, computev1alpha1.GatewayTLSVolumeName); v != nil {
			t.Errorf("unexpected gateway-TLS volume with TLS disabled: %+v", v)
		}
		if m := findMount(pt, computev1alpha1.GatewayTLSVolumeName); m != nil {
			t.Errorf("unexpected gateway-TLS mount with TLS disabled: %+v", m)
		}
		live, ready := probeSchemes(pt)
		if live != corev1.URISchemeHTTP || ready != corev1.URISchemeHTTP {
			t.Errorf("probe schemes = (liveness=%s, readiness=%s), want HTTP/HTTP with TLS disabled", live, ready)
		}
	})

	t.Run("absent and HTTP when gateway TLS is enabled but not yet ready", func(t *testing.T) {
		inst := &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "default"},
			Spec: computev1alpha1.FireboltInstanceSpec{
				TLS: &computev1alpha1.TLSSpec{Gateway: &computev1alpha1.TLSListenerSpec{Enabled: true}},
			},
		}
		pt := effectiveGatewayPodTemplate(inst, "fb-gateway-config", "", baseLabels, wakeAgentConfig{})
		if v := findVol(pt, computev1alpha1.GatewayTLSVolumeName); v != nil {
			t.Errorf("unexpected gateway-TLS volume before GatewayTLS is ready: %+v", v)
		}
		live, ready := probeSchemes(pt)
		if live != corev1.URISchemeHTTP || ready != corev1.URISchemeHTTP {
			t.Errorf("probe schemes = (liveness=%s, readiness=%s), want HTTP/HTTP before GatewayTLS is ready", live, ready)
		}
	})

	t.Run("wired and HTTPS once ready", func(t *testing.T) {
		inst := &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "default"},
			Spec: computev1alpha1.FireboltInstanceSpec{
				TLS: &computev1alpha1.TLSSpec{Gateway: &computev1alpha1.TLSListenerSpec{Enabled: true}},
			},
			Status: computev1alpha1.FireboltInstanceStatus{
				GatewayTLS: &computev1alpha1.GatewayTLSStatus{SecretName: "fb-gateway-tls"},
			},
		}
		pt := effectiveGatewayPodTemplate(inst, "fb-gateway-config", "", baseLabels, wakeAgentConfig{})
		v := findVol(pt, computev1alpha1.GatewayTLSVolumeName)
		if v == nil || v.Secret == nil || v.Secret.SecretName != "fb-gateway-tls" {
			t.Errorf("gateway-TLS volume = %+v, want Secret.SecretName=fb-gateway-tls", v)
		}
		m := findMount(pt, computev1alpha1.GatewayTLSVolumeName)
		if m == nil || m.MountPath != gatewayTLSMountPath || !m.ReadOnly {
			t.Errorf("gateway-TLS mount = %+v, want MountPath=%s ReadOnly=true", m, gatewayTLSMountPath)
		}
		// Both probes stay HTTP on the metrics port even once client TLS is
		// ready: they target the always-plaintext stats listener, never the
		// (now-TLS, possibly mutual-TLS) client listener.
		live, ready := probeSchemes(pt)
		if live != corev1.URISchemeHTTP || ready != corev1.URISchemeHTTP {
			t.Errorf("probe schemes = (liveness=%s, readiness=%s), want HTTP/HTTP (both on the metrics port)", live, ready)
		}
	})

	t.Run("both probes target the metrics port, never the client port", func(t *testing.T) {
		// mTLS on the client listener rejects the kubelet's cert-less probe,
		// and the client listener is absent during fail-closed provisioning —
		// so neither probe may target it. Both use the always-plaintext,
		// always-present metrics port.
		inst := &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "default"},
			Spec: computev1alpha1.FireboltInstanceSpec{
				TLS: &computev1alpha1.TLSSpec{Gateway: &computev1alpha1.TLSListenerSpec{
					Enabled:           true,
					ClientCASecretRef: &corev1.LocalObjectReference{Name: "clients-ca"},
				}},
			},
			Status: computev1alpha1.FireboltInstanceStatus{
				GatewayTLS: &computev1alpha1.GatewayTLSStatus{SecretName: "fb-gateway-tls"},
			},
		}
		pt := effectiveGatewayPodTemplate(inst, "fb-gateway-config", "", baseLabels, wakeAgentConfig{})
		c := pt.Spec.Containers[0]
		if got := c.LivenessProbe.HTTPGet.Port.StrVal; got != "metrics" {
			t.Errorf("liveness probe port = %q, want metrics", got)
		}
		if got := c.ReadinessProbe.HTTPGet.Port.StrVal; got != "metrics" {
			t.Errorf("readiness probe port = %q, want metrics (client listener requires a client cert the probe can't present)", got)
		}
	})

	t.Run("client-CA volume mounted (ca.crt only) when mutual TLS configured and ready", func(t *testing.T) {
		inst := &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "default"},
			Spec: computev1alpha1.FireboltInstanceSpec{
				TLS: &computev1alpha1.TLSSpec{Gateway: &computev1alpha1.TLSListenerSpec{
					Enabled:           true,
					ClientCASecretRef: &corev1.LocalObjectReference{Name: "clients-ca"},
				}},
			},
			Status: computev1alpha1.FireboltInstanceStatus{
				GatewayTLS: &computev1alpha1.GatewayTLSStatus{SecretName: "fb-gateway-tls"},
			},
		}
		pt := effectiveGatewayPodTemplate(inst, "fb-gateway-config", "", baseLabels, wakeAgentConfig{})
		v := findVol(pt, computev1alpha1.GatewayClientCAVolumeName)
		if v == nil || v.Secret == nil || v.Secret.SecretName != "clients-ca" {
			t.Fatalf("client-CA volume = %+v, want Secret.SecretName=clients-ca", v)
		}
		if len(v.Secret.Items) != 1 || v.Secret.Items[0].Key != "ca.crt" {
			t.Errorf("client-CA volume must project only ca.crt, got Items=%+v", v.Secret.Items)
		}
		m := findMount(pt, computev1alpha1.GatewayClientCAVolumeName)
		if m == nil || m.MountPath != gatewayClientCAMountPath || !m.ReadOnly {
			t.Errorf("client-CA mount = %+v, want MountPath=%s ReadOnly=true", m, gatewayClientCAMountPath)
		}
	})
}

func TestGatewayRoutingRBACRestrictsBothAccountKinds(t *testing.T) {
	for _, customAccount := range []string{"", "existing-gateway"} {
		t.Run("account="+customAccount, func(t *testing.T) {
			inst := &computev1alpha1.FireboltInstance{
				ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "work", UID: "instance-uid"},
				Spec: computev1alpha1.FireboltInstanceSpec{Gateway: computev1alpha1.GatewaySpec{
					Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: customAccount}},
				}},
			}
			var role rbacv1.Role
			var binding rbacv1.RoleBinding
			var accounts int
			scheme := authTestScheme(t)
			c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
				Apply: func(_ context.Context, _ client.WithWatch, object runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
					data, err := json.Marshal(object)
					if err != nil {
						return err
					}
					var kind metav1.TypeMeta
					if err := json.Unmarshal(data, &kind); err != nil {
						return err
					}
					switch kind.Kind {
					case "Role":
						return json.Unmarshal(data, &role)
					case "RoleBinding":
						return json.Unmarshal(data, &binding)
					case "ServiceAccount":
						accounts++
					default:
						t.Errorf("unexpected RBAC object %s", data)
					}
					return nil
				},
			}).Build()
			r := &FireboltInstanceReconciler{Client: c, Scheme: scheme}
			if err := r.ensureGatewayRBAC(context.Background(), inst); err != nil {
				t.Fatal(err)
			}
			wantAccount := customAccount
			if customAccount == "" {
				wantAccount = gatewayServiceAccountName(inst.Name)
				if accounts != 1 {
					t.Fatal("managed ServiceAccount was not created")
				}
			} else if accounts != 0 {
				t.Fatal("custom ServiceAccount must not be modified")
			}
			if len(binding.Subjects) != 1 || binding.Subjects[0].Name != wantAccount || binding.Subjects[0].Namespace != inst.Namespace {
				t.Fatalf("wrong routing identity: %+v", binding.Subjects)
			}
			if binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != role.Name || role.Namespace != inst.Namespace {
				t.Fatal("routing permission must bind the namespaced Role")
			}
			wantRules := []rbacv1.PolicyRule{
				{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{routing.ConfigMapName(inst.Name)}, Verbs: []string{"get", "list", "watch"}},
				{APIGroups: []string{"discovery.k8s.io"}, Resources: []string{"endpointslices"}, Verbs: []string{"get", "list", "watch"}},
			}
			if !reflect.DeepEqual(role.Rules, wantRules) {
				t.Fatalf("agent RBAC must be read-only and ConfigMap access name-scoped: %+v", role.Rules)
			}
		})
	}
}

func TestGatewayAgentChartConfigurationIsMandatory(t *testing.T) {
	helmAvailable(t)
	for _, imageOverride := range []bool{false, true} {
		args := []string{"template", "firebolt-operator", "../../helm/firebolt-operator", "--kube-version", "1.33.0", "--show-only", "templates/deployment.yaml"}
		if imageOverride {
			args = append(args, "--set", "image.repository=example.com/custom/operator", "--set", "image.tag=custom")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := exec.CommandContext(ctx, "helm", args...).CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, out)
		}
		var deployment appsv1.Deployment
		if err := yaml.Unmarshal(out, &deployment); err != nil {
			t.Fatal(err)
		}
		if len(deployment.Spec.Template.Spec.Containers) == 0 {
			t.Fatal("operator container missing")
		}
		manager := deployment.Spec.Template.Spec.Containers[0]
		if !slices.Contains(manager.Args, "--wake-agent-image="+manager.Image) {
			t.Fatalf("gateway agent must use the same image as the manager: %v", manager.Args)
		}
	}
	for _, removedOption := range []string{"wakeAgent.enabled=false", "gatewayWakeClusterRole.create=false"} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := exec.CommandContext(ctx, "helm", "template", "firebolt-operator", "../../helm/firebolt-operator",
			"--kube-version", "1.33.0", "--set", removedOption).CombinedOutput()
		cancel()
		if err == nil || !strings.Contains(string(out), "schema") {
			t.Fatalf("removed option %s must be rejected by the values schema: %v\n%s", removedOption, err, out)
		}
	}
}
