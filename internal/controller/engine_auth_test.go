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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	enginemetrics "github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
)

// testResolvedAuthInfo returns a native-auth ResolvedAuthInfo with one
// signing key, satisfying every field renderAuthConfig/buildAuthVolumes
// reads.
func testResolvedAuthInfo() *ResolvedAuthInfo {
	return &ResolvedAuthInfo{
		Spec: &computev1alpha1.AuthSpec{
			Enabled: true,
			Local: &computev1alpha1.LocalAuthSpec{
				Admin: computev1alpha1.AdminSpec{
					Name: "firebolt",
					Password: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "test-instance-auth-admin"},
						Key:                  "password",
					},
				},
				SigningAlgorithm: "RS256",
			},
		},
		SigningKeys: []computev1alpha1.SigningKeyStatus{
			{ID: "signing-1", SecretName: "test-instance-auth-signing"},
		},
	}
}

// testInstanceInfoWithAuth returns testInstanceInfo() with Auth populated
// from testResolvedAuthInfo().
func testInstanceInfoWithAuth() InstanceInfo {
	info := testInstanceInfo()
	info.Auth = testResolvedAuthInfo()
	return info
}

func TestBuildConfigMap_AuthDisabled_NoAuthKey(t *testing.T) {
	root := renderConfigWithInstanceInfo(t, testInstanceInfo())
	instance := nestedMap(t, root, "instance")
	if _, present := instance["auth"]; present {
		t.Errorf("instance.auth present with auth disabled: %v", instance["auth"])
	}
}

func TestBuildConfigMap_AuthEnabled_NativeRendersExpectedShape(t *testing.T) {
	root := renderConfigWithInstanceInfo(t, testInstanceInfoWithAuth())
	instance := nestedMap(t, root, "instance")
	auth := nestedMap(t, instance, "auth")

	if enabled, ok := auth["enabled"].(bool); !ok || !enabled {
		t.Errorf("auth.enabled = %v, want true", auth["enabled"])
	}
	if _, present := auth["oidc"]; present {
		t.Errorf("auth.oidc present with no OIDC configured: %v", auth["oidc"])
	}
	if _, present := auth["preferred_authorization_server"]; present {
		t.Errorf("auth.preferred_authorization_server present when unset: %v", auth["preferred_authorization_server"])
	}

	admin := nestedMap(t, auth, "admin")
	if admin["name"] != "firebolt" {
		t.Errorf("auth.admin.name = %v, want firebolt", admin["name"])
	}
	wantPasswordPath := AuthAdminMountPath + "/password"
	if admin["password_file"] != wantPasswordPath {
		t.Errorf("auth.admin.password_file = %v, want %v", admin["password_file"], wantPasswordPath)
	}
	if _, present := admin["password_value"]; present {
		t.Error("auth.admin.password_value must never be rendered (plaintext password would leak into the ConfigMap)")
	}
	if _, present := admin["password_env"]; present {
		t.Error("auth.admin.password_env must never be rendered")
	}

	local := nestedMap(t, auth, "local")
	if local["signing_algorithm"] != "RS256" {
		t.Errorf("auth.local.signing_algorithm = %v, want RS256", local["signing_algorithm"])
	}
	keys, ok := local["signing_keys"].([]interface{})
	if !ok || len(keys) != 1 {
		t.Fatalf("auth.local.signing_keys = %v, want a 1-element array", local["signing_keys"])
	}
	key, ok := keys[0].(map[string]interface{})
	if !ok {
		t.Fatalf("auth.local.signing_keys[0] = %v, want an object", keys[0])
	}
	if key["id"] != "signing-1" {
		t.Errorf("auth.local.signing_keys[0].id = %v, want signing-1", key["id"])
	}
	wantKeyPath := AuthSigningMountPathBase + "/signing-1/" + corev1.TLSPrivateKeyKey
	if key["private_key_path"] != wantKeyPath {
		t.Errorf("auth.local.signing_keys[0].private_key_path = %v, want %v", key["private_key_path"], wantKeyPath)
	}
}

func TestBuildConfigMap_AuthEnabled_OmitsEmptyOptionalFields(t *testing.T) {
	// LocalAuthSpec.PasswordLogin/TokenExpiry/MaxTokenAge/ClockSkewTolerance
	// are all left at their Go zero value ("") in testResolvedAuthInfo, so
	// none of them — nor local.jwt itself — should appear in the rendered
	// config: packdb applies its own built-in defaults for anything absent,
	// but would reject an explicit empty-string value.
	root := renderConfigWithInstanceInfo(t, testInstanceInfoWithAuth())
	instance := nestedMap(t, root, "instance")
	auth := nestedMap(t, instance, "auth")

	if _, present := auth["password_login"]; present {
		t.Errorf("auth.password_login present despite empty PasswordLogin: %v", auth["password_login"])
	}
	local := nestedMap(t, auth, "local")
	if _, present := local["jwt"]; present {
		t.Errorf("auth.local.jwt present despite all three duration fields being empty: %v", local["jwt"])
	}
}

func TestBuildConfigMap_AuthEnabled_OIDCRendersProvidersAndPreferredServer(t *testing.T) {
	info := testInstanceInfoWithAuth()
	info.Auth.Spec.PreferredAuthorizationServer = "okta"
	info.Auth.Spec.OIDC = &computev1alpha1.OIDCAuthSpec{
		Providers: []computev1alpha1.OIDCProviderSpec{
			{
				Name:            "okta",
				DiscoveryURL:    "https://okta.example.com/.well-known/openid-configuration",
				UsernameMapping: "{{ email }}",
				JITProvisioning: &computev1alpha1.JITProvisioningSpec{
					Enabled:      true,
					DefaultRoles: []string{"public", "analyst"},
				},
			},
		},
	}

	root := renderConfigWithInstanceInfo(t, info)
	instance := nestedMap(t, root, "instance")
	auth := nestedMap(t, instance, "auth")

	if auth["preferred_authorization_server"] != "okta" {
		t.Errorf("auth.preferred_authorization_server = %v, want okta", auth["preferred_authorization_server"])
	}

	oidc := nestedMap(t, auth, "oidc")
	providers, ok := oidc["providers"].([]interface{})
	if !ok || len(providers) != 1 {
		t.Fatalf("auth.oidc.providers = %v, want a 1-element array", oidc["providers"])
	}
	provider, ok := providers[0].(map[string]interface{})
	if !ok {
		t.Fatalf("auth.oidc.providers[0] = %v, want an object", providers[0])
	}
	if provider["name"] != "okta" {
		t.Errorf("providers[0].name = %v, want okta", provider["name"])
	}
	if provider["discovery_url"] != "https://okta.example.com/.well-known/openid-configuration" {
		t.Errorf("providers[0].discovery_url = %v", provider["discovery_url"])
	}
	if provider["username_mapping"] != "{{ email }}" {
		t.Errorf("providers[0].username_mapping = %v, want {{ email }}", provider["username_mapping"])
	}
	jit, ok := provider["jit_provisioning"].(map[string]interface{})
	if !ok {
		t.Fatalf("providers[0].jit_provisioning = %v, want an object", provider["jit_provisioning"])
	}
	if enabled, ok := jit["enabled"].(bool); !ok || !enabled {
		t.Errorf("providers[0].jit_provisioning.enabled = %v, want true", jit["enabled"])
	}
	roles, ok := jit["default_roles"].([]interface{})
	if !ok || len(roles) != 2 {
		t.Fatalf("providers[0].jit_provisioning.default_roles = %v, want [public analyst]", jit["default_roles"])
	}
}

// TestBuildConfigMap_AuthEnabled_CustomEngineConfigCannotOverrideAuth is the
// end-to-end counterpart to TestEffectiveCustomEngineConfig_StripsInstanceAuth
// (engine_class_settings_test.go): it renders a full config.yaml through
// buildConfigMap with a FireboltEngineSpec whose customEngineConfig tries to
// disable auth and swap in a different admin/signing-key Secret, and asserts
// the operator-rendered auth section — built from InstanceInfo.Auth, the
// single source of truth every engine in the Instance shares — wins
// unconditionally. Without instance.auth in OperatorOwnedEngineConfigPaths
// (api/v1alpha1/operatorauthority.go) this customEngineConfig would deep-merge
// on top of the operator's auth block and let one engine silently diverge
// from the rest of the Instance.
func TestBuildConfigMap_AuthEnabled_CustomEngineConfigCannotOverrideAuth(t *testing.T) {
	spec := testSpec()
	spec.CustomEngineConfig = jsonRaw(`{
		"instance": {
			"auth": {
				"enabled": false,
				"admin": {"name": "attacker", "password_file": "/tmp/pwned"}
			}
		}
	}`)

	cm := buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfoWithAuth(), nil)
	var root map[string]interface{}
	if err := yaml.Unmarshal([]byte(cm.Data[ConfigFileName]), &root); err != nil {
		t.Fatalf("rendered config.yaml is not valid YAML: %v", err)
	}
	instance := nestedMap(t, root, "instance")
	auth := nestedMap(t, instance, "auth")

	if enabled, ok := auth["enabled"].(bool); !ok || !enabled {
		t.Errorf("auth.enabled = %v, want true (customEngineConfig's enabled:false must be stripped)", auth["enabled"])
	}
	admin := nestedMap(t, auth, "admin")
	if admin["name"] != "firebolt" {
		t.Errorf("auth.admin.name = %v, want firebolt (customEngineConfig's admin override must be stripped)", admin["name"])
	}
	if admin["password_file"] == "/tmp/pwned" {
		t.Error("auth.admin.password_file was overridden by customEngineConfig; instance.auth must be operator-owned")
	}
}

// renderConfigWithInstanceInfo is renderConfig (engine_reconcile_test.go)
// generalized to accept an arbitrary InstanceInfo, since the existing
// helper hard-codes testInstanceInfo() (no auth).
func renderConfigWithInstanceInfo(t *testing.T, instanceInfo InstanceInfo) map[string]interface{} {
	t.Helper()
	cm := buildConfigMap(testSpec(), testEngineName, testNamespace, 0, instanceInfo, nil)
	var root map[string]interface{}
	if err := yaml.Unmarshal([]byte(cm.Data[ConfigFileName]), &root); err != nil {
		t.Fatalf("rendered config.yaml is not valid YAML: %v", err)
	}
	return root
}

func TestBuildStatefulSet_AuthDisabled_NoAuthVolumesOrMounts(t *testing.T) {
	sts := buildStatefulSet(testSpec(), testEngineName, testNamespace, 0, testInstanceInfo(), nil)
	pod := sts.Spec.Template.Spec

	for _, v := range pod.Volumes {
		if v.Name == computev1alpha1.EngineAuthAdminVolumeName {
			t.Errorf("unexpected admin auth volume present with auth disabled: %+v", v)
		}
	}
	container := findEngineContainer(t, pod.Containers)
	for _, m := range container.VolumeMounts {
		if m.Name == computev1alpha1.EngineAuthAdminVolumeName {
			t.Errorf("unexpected admin auth volume mount present with auth disabled: %+v", m)
		}
	}
	if sts.Annotations[AnnotationAuthHash] != "" {
		t.Errorf("AnnotationAuthHash = %q, want absent when auth is disabled", sts.Annotations[AnnotationAuthHash])
	}
}

func TestBuildStatefulSet_AuthEnabled_VolumesAndMountsWired(t *testing.T) {
	sts := buildStatefulSet(testSpec(), testEngineName, testNamespace, 0, testInstanceInfoWithAuth(), nil)
	pod := sts.Spec.Template.Spec

	adminVol := findVolume(t, pod.Volumes, computev1alpha1.EngineAuthAdminVolumeName)
	if adminVol.Secret == nil || adminVol.Secret.SecretName != "test-instance-auth-admin" {
		t.Errorf("admin volume Secret = %+v, want SecretName=test-instance-auth-admin", adminVol.Secret)
	}

	signingVolName := authSigningVolumeName("signing-1")
	signingVol := findVolume(t, pod.Volumes, signingVolName)
	if signingVol.Secret == nil || signingVol.Secret.SecretName != "test-instance-auth-signing" {
		t.Errorf("signing volume Secret = %+v, want SecretName=test-instance-auth-signing", signingVol.Secret)
	}

	container := findEngineContainer(t, pod.Containers)
	adminMount := findVolumeMount(t, container.VolumeMounts, computev1alpha1.EngineAuthAdminVolumeName)
	if adminMount.MountPath != AuthAdminMountPath || !adminMount.ReadOnly {
		t.Errorf("admin mount = %+v, want MountPath=%s ReadOnly=true", adminMount, AuthAdminMountPath)
	}
	signingMount := findVolumeMount(t, container.VolumeMounts, signingVolName)
	wantSigningPath := AuthSigningMountPathBase + "/signing-1"
	if signingMount.MountPath != wantSigningPath || !signingMount.ReadOnly {
		t.Errorf("signing mount = %+v, want MountPath=%s ReadOnly=true", signingMount, wantSigningPath)
	}

	if sts.Annotations[AnnotationAuthHash] == "" {
		t.Error("AnnotationAuthHash must be set once auth is enabled")
	}
}

func findVolume(t *testing.T, volumes []corev1.Volume, name string) corev1.Volume {
	t.Helper()
	for i := range volumes {
		if volumes[i].Name == name {
			return volumes[i]
		}
	}
	t.Fatalf("no volume named %q found in %+v", name, volumes)
	return corev1.Volume{}
}

func findVolumeMount(t *testing.T, mounts []corev1.VolumeMount, name string) corev1.VolumeMount {
	t.Helper()
	for _, m := range mounts {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("no volume mount named %q found in %+v", name, mounts)
	return corev1.VolumeMount{}
}

func findEngineContainer(t *testing.T, containers []corev1.Container) corev1.Container {
	t.Helper()
	for i := range containers {
		if containers[i].Name == computev1alpha1.EngineContainerName {
			return containers[i]
		}
	}
	t.Fatalf("no %q container found in %+v", computev1alpha1.EngineContainerName, containers)
	return corev1.Container{}
}

// TestStsMatchesSpec_AuthDrift pins down the gap AnnotationAuthHash exists
// to close: a Secret NAME change behind an identically-named/pathed
// volume is invisible to a VolumeMounts-only comparison, since
// VolumeMounts never carry the Secret name — only the Volume's
// VolumeSource does.
func TestStsMatchesSpec_AuthDrift(t *testing.T) {
	spec := testSpec()

	t.Run("no drift when auth config is unchanged", func(t *testing.T) {
		info := testInstanceInfoWithAuth()
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, info, nil)
		if !stsMatchesSpec(sts, spec, info, nil) {
			t.Error("stsMatchesSpec: false, want true for identical auth config")
		}
	})

	t.Run("drift when auth becomes enabled", func(t *testing.T) {
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, testInstanceInfo(), nil)
		if stsMatchesSpec(sts, spec, testInstanceInfoWithAuth(), nil) {
			t.Error("stsMatchesSpec: true, want false when auth transitions from disabled to enabled")
		}
	})

	t.Run("drift when the admin secret name changes", func(t *testing.T) {
		original := testInstanceInfoWithAuth()
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, original, nil)

		changed := testInstanceInfoWithAuth()
		changed.Auth.Spec.Local.Admin.Password.Name = "a-different-secret"
		if stsMatchesSpec(sts, spec, changed, nil) {
			t.Error("stsMatchesSpec: true, want false when the admin Secret name changes " +
				"(VolumeMounts alone cannot see this — see AnnotationAuthHash)")
		}
	})

	t.Run("drift when the signing key secret name changes", func(t *testing.T) {
		original := testInstanceInfoWithAuth()
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, original, nil)

		changed := testInstanceInfoWithAuth()
		changed.Auth.SigningKeys[0].SecretName = "a-different-signing-secret"
		if stsMatchesSpec(sts, spec, changed, nil) {
			t.Error("stsMatchesSpec: true, want false when the signing key's Secret name changes")
		}
	})

	t.Run("drift when a config-only auth field changes with no Secret-name change", func(t *testing.T) {
		// preferred_authorization_server (and every other rendered field
		// that isn't a Secret name — signing_algorithm, password_login,
		// JWT durations, the whole OIDC block) changes what config.yaml
		// contains but touches no volume/mount. Since packdb reads
		// config.yaml only at startup, this MUST still roll a new
		// generation, or the engine runs stale auth config forever.
		original := testInstanceInfoWithAuth()
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, original, nil)

		changed := testInstanceInfoWithAuth()
		changed.Auth.Spec.PreferredAuthorizationServer = "_local"
		if stsMatchesSpec(sts, spec, changed, nil) {
			t.Error("stsMatchesSpec: true, want false when PreferredAuthorizationServer changes " +
				"(no Secret name involved — VolumeMounts/Volumes are identical, only authHash can see this)")
		}
	})
}

// --- resolveInstanceInfo auth-gating tests (engine_controller.go) ---

func authGatingTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme.AddToScheme: %v", err)
	}
	if err := computev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("computev1alpha1.AddToScheme: %v", err)
	}
	return s
}

// authEnabledInstanceFixture returns a FireboltInstance with auth enabled,
// Status.MetadataEndpoint/Spec.ID populated (so the pre-auth gates in
// resolveInstanceInfo pass), and the given AuthReady condition + signing
// keys — the two knobs the auth-gating tests below flip independently.
func authEnabledInstanceFixture(authReady bool, signingKeys []computev1alpha1.SigningKeyStatus) *computev1alpha1.FireboltInstance {
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "test-instance", Namespace: testNamespace},
		Spec: computev1alpha1.FireboltInstanceSpec{
			ID: testInstanceID,
			Auth: &computev1alpha1.AuthSpec{
				Enabled: true,
				Local: &computev1alpha1.LocalAuthSpec{
					Admin: computev1alpha1.AdminSpec{
						Password: corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "test-instance-auth-admin"},
							Key:                  "password",
						},
					},
				},
			},
		},
		Status: computev1alpha1.FireboltInstanceStatus{
			MetadataEndpoint: testMetadataEndpoint,
		},
	}
	if authReady {
		inst.Status.Conditions = []metav1.Condition{{
			Type:               computev1alpha1.InstanceConditionAuthReady,
			Status:             metav1.ConditionTrue,
			Reason:             "Ready",
			ObservedGeneration: inst.Generation,
			LastTransitionTime: metav1.Now(),
		}}
	}
	if signingKeys != nil {
		inst.Status.Auth = &computev1alpha1.AuthStatus{SigningKeys: signingKeys}
	}
	return inst
}

// engineForAuthFixture returns a minimal FireboltEngine referencing
// "test-instance" (matching authEnabledInstanceFixture's name) —
// engineRefingClassFixture (engine_classref_test.go) hard-codes
// InstanceRef: "inst", which doesn't match.
func engineForAuthFixture() *computev1alpha1.FireboltEngine {
	return &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: "e", Namespace: testNamespace},
		Spec:       computev1alpha1.FireboltEngineSpec{InstanceRef: "test-instance", Replicas: 1},
	}
}

func adminSecretFixture() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-instance-auth-admin", Namespace: testNamespace},
		Data:       map[string][]byte{"password": []byte("hunter2")},
	}
}

func signingSecretFixture() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-instance-auth-signing", Namespace: testNamespace},
		// A parseable tls.crt alongside tls.key so resolveInstanceInfo's FB-896 #4
		// public-key fingerprint fold can read a real public key.
		Data: map[string][]byte{
			corev1.TLSPrivateKeyKey: []byte("fake-pem"),
			corev1.TLSCertKey:       mustGenSigningCertPEM(),
		},
	}
}

func TestResolveInstanceInfo_AuthDisabled(t *testing.T) {
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
	if info.Auth != nil {
		t.Errorf("info.Auth = %+v, want nil when spec.auth is unset", info.Auth)
	}
}

func TestResolveInstanceInfo_AuthEnabledButNotReadyBlocks(t *testing.T) {
	sch := authGatingTestScheme(t)
	inst := authEnabledInstanceFixture(false, nil) // AuthReady condition absent
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(inst, adminSecretFixture(), signingSecretFixture()).Build()
	r := &FireboltEngineReconciler{Client: cli, Scheme: sch, MetricsRecorder: enginemetrics.NoOpEngineRecorder{}}

	_, err := r.resolveInstanceInfo(context.Background(), engineForAuthFixture())
	if err == nil {
		t.Fatal("resolveInstanceInfo: expected error when AuthReady is not True, got nil")
	}
}

func TestResolveInstanceInfo_AuthReadyButSecretMissingBlocks(t *testing.T) {
	sch := authGatingTestScheme(t)
	keys := []computev1alpha1.SigningKeyStatus{{ID: "signing-1", SecretName: "test-instance-auth-signing"}}
	inst := authEnabledInstanceFixture(true, keys)
	// Deliberately omit the signing Secret: AuthReady claims ready (stale
	// or racy), but the engine controller's own preflight
	// (checkSecretKeyPresent) must still catch the missing Secret rather
	// than trust the condition blindly.
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(inst, adminSecretFixture()).Build()
	r := &FireboltEngineReconciler{Client: cli, Scheme: sch, MetricsRecorder: enginemetrics.NoOpEngineRecorder{}}

	_, err := r.resolveInstanceInfo(context.Background(), engineForAuthFixture())
	if err == nil {
		t.Fatal("resolveInstanceInfo: expected error when the signing key Secret is missing, got nil")
	}
}

func TestResolveInstanceInfo_AuthReadyButLocalNilBlocks(t *testing.T) {
	sch := authGatingTestScheme(t)
	keys := []computev1alpha1.SigningKeyStatus{{ID: "signing-1", SecretName: "test-instance-auth-signing"}}
	inst := authEnabledInstanceFixture(true, keys)
	// Simulate the cache-consistency window where a new spec (Local
	// stripped) has propagated but AuthReady still reflects the prior
	// reconcile: resolveInstanceInfo must error rather than nil-deref on
	// inst.Spec.Auth.Local.
	inst.Spec.Auth.Local = nil
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(inst, adminSecretFixture(), signingSecretFixture()).Build()
	r := &FireboltEngineReconciler{Client: cli, Scheme: sch, MetricsRecorder: enginemetrics.NoOpEngineRecorder{}}

	_, err := r.resolveInstanceInfo(context.Background(), engineForAuthFixture())
	if err == nil {
		t.Fatal("resolveInstanceInfo: expected error when spec.auth.local is nil, got nil")
	}
}

func TestResolveInstanceInfo_AuthReadyAndSecretsPresentPopulatesAuth(t *testing.T) {
	sch := authGatingTestScheme(t)
	keys := []computev1alpha1.SigningKeyStatus{{ID: "signing-1", SecretName: "test-instance-auth-signing"}}
	inst := authEnabledInstanceFixture(true, keys)
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(inst, adminSecretFixture(), signingSecretFixture()).Build()
	r := &FireboltEngineReconciler{Client: cli, Scheme: sch, MetricsRecorder: enginemetrics.NoOpEngineRecorder{}}

	info, err := r.resolveInstanceInfo(context.Background(), engineForAuthFixture())
	if err != nil {
		t.Fatalf("resolveInstanceInfo: unexpected error: %v", err)
	}
	if info.Auth == nil {
		t.Fatal("info.Auth = nil, want populated once AuthReady and every Secret exist")
	}
	if len(info.Auth.SigningKeys) != 1 || info.Auth.SigningKeys[0].ID != "signing-1" {
		t.Errorf("info.Auth.SigningKeys = %+v, want [{signing-1 test-instance-auth-signing ...}]", info.Auth.SigningKeys)
	}
}
