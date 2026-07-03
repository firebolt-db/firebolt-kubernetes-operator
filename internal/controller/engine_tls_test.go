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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	enginemetrics "github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
)

// testInstanceInfoWithTLS returns testInstanceInfo() with TLS populated,
// satisfying every field renderEndpointsConfig/buildEngineTLSVolumes
// reads.
func testInstanceInfoWithTLS() InstanceInfo {
	info := testInstanceInfo()
	info.TLS = &ResolvedEngineTLSInfo{SecretName: "test-instance-engine-tls"}
	return info
}

func TestBuildConfigMap_TLSDisabled_NoEndpointsKey(t *testing.T) {
	root := renderConfigWithInstanceInfo(t, testInstanceInfo())
	if _, present := root["endpoints"]; present {
		t.Errorf("endpoints present with TLS disabled: %v", root["endpoints"])
	}
}

func TestBuildConfigMap_TLSEnabled_RendersExpectedShape(t *testing.T) {
	root := renderConfigWithInstanceInfo(t, testInstanceInfoWithTLS())
	endpoints := nestedMap(t, root, "endpoints")
	http := nestedMap(t, endpoints, "http")
	listeners, ok := http["listeners"].([]interface{})
	if !ok || len(listeners) != 1 {
		t.Fatalf("endpoints.http.listeners = %v, want a 1-element array", http["listeners"])
	}
	listener, ok := listeners[0].(map[string]interface{})
	if !ok {
		t.Fatalf("endpoints.http.listeners[0] = %v, want an object", listeners[0])
	}
	if listener["type"] != "tcp" {
		t.Errorf("listeners[0].type = %v, want tcp", listener["type"])
	}
	gotPort, ok := listener["port"].(float64)
	if !ok || int32(gotPort) != EngineHTTPQueryPort {
		t.Errorf("listeners[0].port = %v, want %d", listener["port"], EngineHTTPQueryPort)
	}
	tlsCfg := nestedMap(t, listener, "tls")
	wantCertPath := EngineTLSMountPath + "/" + corev1.TLSCertKey
	wantKeyPath := EngineTLSMountPath + "/" + corev1.TLSPrivateKeyKey
	if tlsCfg["certificate_file"] != wantCertPath {
		t.Errorf("tls.certificate_file = %v, want %v", tlsCfg["certificate_file"], wantCertPath)
	}
	if tlsCfg["private_key_file"] != wantKeyPath {
		t.Errorf("tls.private_key_file = %v, want %v", tlsCfg["private_key_file"], wantKeyPath)
	}
	if _, present := endpoints["postgres"]; present {
		t.Errorf("endpoints.postgres present; packdb rejects tls on a postgres listener and the operator "+
			"does not expose one: %v", endpoints["postgres"])
	}
}

// TestBuildConfigMap_TLSEnabled_CustomEngineConfigCannotOverrideEndpoints
// mirrors TestBuildConfigMap_AuthEnabled_CustomEngineConfigCannotOverrideAuth:
// a customEngineConfig that tries to disable/replace the TLS listener must
// be stripped, since every engine in the Instance must present the same
// certificate.
func TestBuildConfigMap_TLSEnabled_CustomEngineConfigCannotOverrideEndpoints(t *testing.T) {
	spec := testSpec()
	spec.CustomEngineConfig = jsonRaw(`{
		"endpoints": {
			"http": {
				"listeners": [{"type": "tcp", "port": 3473}]
			}
		}
	}`)

	cm := buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfoWithTLS(), nil)
	var root map[string]interface{}
	if err := yaml.Unmarshal([]byte(cm.Data[ConfigFileName]), &root); err != nil {
		t.Fatalf("rendered config.yaml is not valid YAML: %v", err)
	}
	endpoints := nestedMap(t, root, "endpoints")
	http := nestedMap(t, endpoints, "http")
	listeners, ok := http["listeners"].([]interface{})
	if !ok || len(listeners) != 1 {
		t.Fatalf("endpoints.http.listeners = %v, want a 1-element array", http["listeners"])
	}
	listener, ok := listeners[0].(map[string]interface{})
	if !ok {
		t.Fatalf("endpoints.http.listeners[0] = %v, want an object", listeners[0])
	}
	if _, present := listener["tls"]; !present {
		t.Error("listeners[0].tls was stripped by customEngineConfig; endpoints must be operator-owned")
	}
}

func TestBuildStatefulSet_TLSDisabled_NoTLSVolumesOrMounts(t *testing.T) {
	sts := buildStatefulSet(testSpec(), testEngineName, testNamespace, 0, testInstanceInfo(), nil)
	pod := sts.Spec.Template.Spec

	for _, v := range pod.Volumes {
		if v.Name == computev1alpha1.EngineTLSVolumeName {
			t.Errorf("unexpected TLS volume present with TLS disabled: %+v", v)
		}
	}
	container := findEngineContainer(t, pod.Containers)
	for _, m := range container.VolumeMounts {
		if m.Name == computev1alpha1.EngineTLSVolumeName {
			t.Errorf("unexpected TLS volume mount present with TLS disabled: %+v", m)
		}
	}
	if sts.Annotations[AnnotationEngineTLSHash] != "" {
		t.Errorf("AnnotationEngineTLSHash = %q, want absent when TLS is disabled", sts.Annotations[AnnotationEngineTLSHash])
	}
}

func TestBuildStatefulSet_TLSEnabled_VolumesAndMountsWired(t *testing.T) {
	sts := buildStatefulSet(testSpec(), testEngineName, testNamespace, 0, testInstanceInfoWithTLS(), nil)
	pod := sts.Spec.Template.Spec

	vol := findVolume(t, pod.Volumes, computev1alpha1.EngineTLSVolumeName)
	if vol.Secret == nil || vol.Secret.SecretName != "test-instance-engine-tls" {
		t.Errorf("TLS volume Secret = %+v, want SecretName=test-instance-engine-tls", vol.Secret)
	}

	container := findEngineContainer(t, pod.Containers)
	mount := findVolumeMount(t, container.VolumeMounts, computev1alpha1.EngineTLSVolumeName)
	if mount.MountPath != EngineTLSMountPath || !mount.ReadOnly {
		t.Errorf("TLS mount = %+v, want MountPath=%s ReadOnly=true", mount, EngineTLSMountPath)
	}

	if sts.Annotations[AnnotationEngineTLSHash] == "" {
		t.Error("AnnotationEngineTLSHash must be set once TLS is enabled")
	}
}

// TestStsMatchesSpec_TLSDrift mirrors TestStsMatchesSpec_AuthDrift: a
// Secret NAME change behind the identically-named EngineTLSVolumeName
// volume is invisible to a VolumeMounts-only comparison.
func TestStsMatchesSpec_TLSDrift(t *testing.T) {
	spec := testSpec()

	t.Run("no drift when TLS config is unchanged", func(t *testing.T) {
		info := testInstanceInfoWithTLS()
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, info, nil)
		if !stsMatchesSpec(sts, spec, info, nil) {
			t.Error("stsMatchesSpec: false, want true for identical TLS config")
		}
	})

	t.Run("drift when TLS becomes enabled", func(t *testing.T) {
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, testInstanceInfo(), nil)
		if stsMatchesSpec(sts, spec, testInstanceInfoWithTLS(), nil) {
			t.Error("stsMatchesSpec: true, want false when TLS transitions from disabled to enabled")
		}
	})

	t.Run("drift when the TLS secret name changes", func(t *testing.T) {
		original := testInstanceInfoWithTLS()
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, original, nil)

		changed := testInstanceInfoWithTLS()
		changed.TLS.SecretName = "a-different-tls-secret"
		if stsMatchesSpec(sts, spec, changed, nil) {
			t.Error("stsMatchesSpec: true, want false when the TLS Secret name changes " +
				"(VolumeMounts alone cannot see this — see AnnotationEngineTLSHash)")
		}
	})
}

// TestBuildStatefulSet_TLSEnabled_WebSidecarBackendSwitchesToHTTPS pins
// down that the engine web UI sidecar's backend URL tracks engine TLS:
// once renderEndpointsConfig makes EngineHTTPQueryPort TLS-only (see its
// doc comment), the sidecar's loopback connection to the same port must
// switch to https in the same generation or it breaks.
func TestBuildStatefulSet_TLSEnabled_WebSidecarBackendSwitchesToHTTPS(t *testing.T) {
	findWebContainer := func(sts *appsv1.StatefulSet) *corev1.Container {
		for i := range sts.Spec.Template.Spec.Containers {
			if sts.Spec.Template.Spec.Containers[i].Name == computev1alpha1.EngineWebContainerName {
				return &sts.Spec.Template.Spec.Containers[i]
			}
		}
		return nil
	}
	findEnv := func(c *corev1.Container, name string) string {
		for _, e := range c.Env {
			if e.Name == name {
				return e.Value
			}
		}
		return ""
	}

	spec := testSpec()
	spec.UISidecar = ptr(true)

	t.Run("http when TLS disabled", func(t *testing.T) {
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, testInstanceInfo(), nil)
		c := findWebContainer(sts)
		if c == nil {
			t.Fatal("expected an engine-web container")
		}
		if got := findEnv(c, "FIREBOLT_CORE_URL"); got != "http://localhost:3473" {
			t.Errorf("FIREBOLT_CORE_URL = %q, want http://localhost:3473", got)
		}
	})

	t.Run("https when TLS enabled", func(t *testing.T) {
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, testInstanceInfoWithTLS(), nil)
		c := findWebContainer(sts)
		if c == nil {
			t.Fatal("expected an engine-web container")
		}
		if got := findEnv(c, "FIREBOLT_CORE_URL"); got != "https://localhost:3473" {
			t.Errorf("FIREBOLT_CORE_URL = %q, want https://localhost:3473", got)
		}
	})
}

// --- resolveInstanceInfo TLS-gating tests (engine_controller.go) ---

// engineTLSEnabledInstanceFixture returns a FireboltInstance with engine
// TLS enabled, Status.MetadataEndpoint/Spec.ID populated (so the pre-TLS
// gates in resolveInstanceInfo pass), and the given EngineTLSReady
// condition + Status.EngineTLS.
func engineTLSEnabledInstanceFixture(tlsReady bool, engineTLS *computev1alpha1.EngineTLSStatus) *computev1alpha1.FireboltInstance {
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "test-instance", Namespace: testNamespace},
		Spec: computev1alpha1.FireboltInstanceSpec{
			ID: testInstanceID,
			TLS: &computev1alpha1.TLSSpec{
				Engine: &computev1alpha1.TLSListenerSpec{
					Enabled: true,
					CertManager: &computev1alpha1.CertManagerSpec{
						IssuerRef: computev1alpha1.CertManagerIssuerRef{Name: "internal-ca"},
					},
				},
			},
		},
		Status: computev1alpha1.FireboltInstanceStatus{
			MetadataEndpoint: testMetadataEndpoint,
		},
	}
	if tlsReady {
		inst.Status.Conditions = []metav1.Condition{{
			Type:               computev1alpha1.InstanceConditionEngineTLSReady,
			Status:             metav1.ConditionTrue,
			Reason:             "Ready",
			ObservedGeneration: inst.Generation,
			LastTransitionTime: metav1.Now(),
		}}
	}
	inst.Status.EngineTLS = engineTLS
	return inst
}

func engineTLSSecretFixture() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-instance-engine-tls", Namespace: testNamespace},
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte("fake-cert"),
			corev1.TLSPrivateKeyKey: []byte("fake-key"),
		},
	}
}

func TestResolveInstanceInfo_TLSDisabled(t *testing.T) {
	sch := authGatingTestScheme(t)
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "test-instance", Namespace: testNamespace},
		Spec:       computev1alpha1.FireboltInstanceSpec{ID: testInstanceID},
		Status:     computev1alpha1.FireboltInstanceStatus{MetadataEndpoint: testMetadataEndpoint},
	}
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(inst).Build()
	r := &FireboltEngineReconciler{Client: cli, Scheme: sch, MetricsRecorder: enginemetrics.NoOpEngineRecorder{}}

	info, err := r.resolveInstanceInfo(context.Background(), engineForAuthFixture())
	if err != nil {
		t.Fatalf("resolveInstanceInfo: unexpected error: %v", err)
	}
	if info.TLS != nil {
		t.Errorf("info.TLS = %+v, want nil when spec.tls is unset", info.TLS)
	}
}

func TestResolveInstanceInfo_TLSEnabledButNotReadyBlocks(t *testing.T) {
	sch := authGatingTestScheme(t)
	inst := engineTLSEnabledInstanceFixture(false, nil) // EngineTLSReady condition absent
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(inst, engineTLSSecretFixture()).Build()
	r := &FireboltEngineReconciler{Client: cli, Scheme: sch, MetricsRecorder: enginemetrics.NoOpEngineRecorder{}}

	_, err := r.resolveInstanceInfo(context.Background(), engineForAuthFixture())
	if err == nil {
		t.Fatal("resolveInstanceInfo: expected error when EngineTLSReady is not True, got nil")
	}
}

func TestResolveInstanceInfo_TLSReadyButSecretMissingBlocks(t *testing.T) {
	sch := authGatingTestScheme(t)
	inst := engineTLSEnabledInstanceFixture(true, &computev1alpha1.EngineTLSStatus{SecretName: "test-instance-engine-tls"})
	// Deliberately omit the TLS Secret: EngineTLSReady claims ready (stale
	// or racy), but the engine controller's own preflight must still
	// catch the missing Secret rather than trust the condition blindly.
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(inst).Build()
	r := &FireboltEngineReconciler{Client: cli, Scheme: sch, MetricsRecorder: enginemetrics.NoOpEngineRecorder{}}

	_, err := r.resolveInstanceInfo(context.Background(), engineForAuthFixture())
	if err == nil {
		t.Fatal("resolveInstanceInfo: expected error when the TLS Secret is missing, got nil")
	}
}

func TestResolveInstanceInfo_TLSReadyAndSecretPresentPopulatesTLS(t *testing.T) {
	sch := authGatingTestScheme(t)
	inst := engineTLSEnabledInstanceFixture(true, &computev1alpha1.EngineTLSStatus{SecretName: "test-instance-engine-tls"})
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(inst, engineTLSSecretFixture()).Build()
	r := &FireboltEngineReconciler{Client: cli, Scheme: sch, MetricsRecorder: enginemetrics.NoOpEngineRecorder{}}

	info, err := r.resolveInstanceInfo(context.Background(), engineForAuthFixture())
	if err != nil {
		t.Fatalf("resolveInstanceInfo: unexpected error: %v", err)
	}
	if info.TLS == nil {
		t.Fatal("info.TLS is nil, want populated")
	}
	if info.TLS.SecretName != "test-instance-engine-tls" {
		t.Errorf("info.TLS.SecretName = %q, want test-instance-engine-tls", info.TLS.SecretName)
	}
}
