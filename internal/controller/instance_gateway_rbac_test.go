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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// TestUserGatewayServiceAccountName pins down the helper that decides
// whether the operator manages the gateway ServiceAccount and RBAC
// (returns "") or hands ownership over to the user (returns the
// user-supplied name). The behavior is load-bearing: when this
// returns non-empty, ensureGatewayResources skips ensureGatewayRBAC
// entirely; otherwise the operator creates the SA + Role + RoleBinding
// at the default name.
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

// TestEffectiveGatewayPodTemplate_ServiceAccountFallback pins down
// the partner behavior on the pod-template side: when the user did
// not set spec.gateway.template.spec.serviceAccountName, the operator
// stamps gatewayServiceAccountName(instance.Name) on the rendered pod
// so it binds to the operator-managed SA. When the user did set it,
// the user value passes through and the operator skips RBAC creation
// (verified by TestUserGatewayServiceAccountName).
func TestEffectiveGatewayPodTemplate_ServiceAccountFallback(t *testing.T) {
	envoyYAML := "" // contents don't matter for SA assertion
	baseLabels := map[string]string{"firebolt.io/instance": "fb"}

	t.Run("operator-managed SA when user did not set it", func(t *testing.T) {
		inst := &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "default"},
		}
		pt := effectiveGatewayPodTemplate(inst, "fb-gateway-config", contentHash(envoyYAML), baseLabels)
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
		pt := effectiveGatewayPodTemplate(inst, "fb-gateway-config", contentHash(envoyYAML), baseLabels)
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
		pt := effectiveGatewayPodTemplate(inst, "fb-gateway-config", "", baseLabels)
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
		pt := effectiveGatewayPodTemplate(inst, "fb-gateway-config", "", baseLabels)
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
		pt := effectiveGatewayPodTemplate(inst, "fb-gateway-config", "", baseLabels)
		v := findVol(pt, computev1alpha1.GatewayEngineCAVolumeName)
		if v == nil || v.Secret == nil || v.Secret.SecretName != "fb-engine-tls" {
			t.Errorf("engine-CA volume = %+v, want Secret.SecretName=fb-engine-tls", v)
		}
		// Least-privilege: only ca.crt is projected, so the engine listener's
		// private key (tls.key) never lands in the gateway pod.
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
		pt := effectiveGatewayPodTemplate(inst, "fb-gateway-config", "", baseLabels)
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
		pt := effectiveGatewayPodTemplate(inst, "fb-gateway-config", "", baseLabels)
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
		pt := effectiveGatewayPodTemplate(inst, "fb-gateway-config", "", baseLabels)
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
		pt := effectiveGatewayPodTemplate(inst, "fb-gateway-config", "", baseLabels)
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
		pt := effectiveGatewayPodTemplate(inst, "fb-gateway-config", "", baseLabels)
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
