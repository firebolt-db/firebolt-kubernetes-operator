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

	corev1 "k8s.io/api/core/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// secretVolume builds a pod volume mounting secretName under an arbitrary
// volume name — the shape of the bypass: the name is the author's to choose, so
// no reserved-name rule can catch it.
func secretVolume(volName, secretName string) corev1.Volume {
	return corev1.Volume{
		Name:         volName,
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secretName}},
	}
}

func projectedSecretVolume(volName, secretName string) corev1.Volume {
	return corev1.Volume{
		Name: volName,
		VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{{Secret: &corev1.SecretProjection{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
			}}},
		}},
	}
}

func volumeNames(vols []corev1.Volume) []string {
	out := make([]string, 0, len(vols))
	for i := range vols {
		out = append(out, vols[i].Name)
	}
	return out
}

func hasVolume(vols []corev1.Volume, name string) bool {
	for i := range vols {
		if vols[i].Name == name {
			return true
		}
	}
	return false
}

// operatorAuthVolumes is the operator-built slice an auth-enabled engine pod
// carries, i.e. what the drop derives the protected Secret set from.
func operatorAuthVolumes() []corev1.Volume {
	return []corev1.Volume{
		{
			Name: "engine-config",
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "cfg"},
			}},
		},
		secretVolume(computev1alpha1.EngineAuthAdminVolumeName, "admin-pw"),
		secretVolume("auth-signing-key-1", "inst-auth-signing"),
		secretVolume(computev1alpha1.EngineTLSVolumeName, testEngineName+SuffixGen+"3"+SuffixEngineTLS),
	}
}

func TestAppendUserPodVolumesDropsSecretAliases(t *testing.T) {
	cases := []struct {
		name       string
		userVolume corev1.Volume
		wantKept   bool
	}{
		{"aliases the admin password", secretVolume("innocuous", "admin-pw"), false},
		{"aliases a signing key", secretVolume("scratch", "inst-auth-signing"), false},
		{
			"aliases the per-generation engine TLS secret",
			secretVolume("certs", testEngineName+SuffixGen+"3"+SuffixEngineTLS),
			false,
		},
		{"aliases a signing key by projection", projectedSecretVolume("proj", "inst-auth-signing"), false},
		{"unrelated secret of the author's own", secretVolume("sidecar-creds", "my-own-secret"), true},
		{
			"unrelated emptyDir",
			corev1.Volume{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := &computev1alpha1.FireboltEngineSpec{
				Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{tc.userVolume},
				}},
			}
			got := appendUserPodVolumes(operatorAuthVolumes(), spec, protectedInfo(), nil)
			if kept := hasVolume(got, tc.userVolume.Name); kept != tc.wantKept {
				t.Fatalf("volume %q kept=%v, want %v (rendered: %v)",
					tc.userVolume.Name, kept, tc.wantKept, volumeNames(got))
			}
			// The operator's own volumes always survive the merge.
			for _, want := range []string{computev1alpha1.EngineAuthAdminVolumeName, "auth-signing-key-1"} {
				if !hasVolume(got, want) {
					t.Errorf("operator volume %q was dropped", want)
				}
			}
		})
	}
}

// TestAppendUserPodVolumesDropsClassSecretAliases pins the class side of the
// merge: a class template is instance-agnostic, so this is the path a shared
// class could otherwise use to reach any Instance's Secrets.
func TestAppendUserPodVolumesDropsClassSecretAliases(t *testing.T) {
	classInfo := &FireboltEngineClassInfo{
		Name: "compute-optimized",
		Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				secretVolume("class-alias", "inst-auth-signing"),
				secretVolume("class-own", "class-secret"),
			},
		}},
	}
	got := appendUserPodVolumes(operatorAuthVolumes(), &computev1alpha1.FireboltEngineSpec{}, protectedInfo(), classInfo)
	if hasVolume(got, "class-alias") {
		t.Error("class template aliased the signing-key Secret and was not dropped")
	}
	if !hasVolume(got, "class-own") {
		t.Error("class template's own unrelated Secret volume must pass through")
	}
}

func TestAppendUserVolumesDropsSecretAliases(t *testing.T) {
	operator := []corev1.Volume{
		secretVolume(computev1alpha1.GatewayTLSVolumeName, "inst-gateway-tls"),
		{
			Name:         computev1alpha1.GatewayTmpVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}
	user := []corev1.Volume{
		secretVolume("innocuous", "inst-gateway-tls"),
		projectedSecretVolume("proj", "inst-gateway-tls"),
		secretVolume("mine", "my-own-secret"),
	}
	got := appendUserVolumes(operator, user, nameSet("inst-gateway-tls"),
		computev1alpha1.GatewayTLSVolumeName, computev1alpha1.GatewayTmpVolumeName)

	for _, dropped := range []string{"innocuous", "proj"} {
		if hasVolume(got, dropped) {
			t.Errorf("volume %q aliased the gateway TLS Secret and was not dropped (rendered: %v)",
				dropped, volumeNames(got))
		}
	}
	if !hasVolume(got, "mine") {
		t.Error("an unrelated Secret volume must pass through")
	}
	if !hasVolume(got, computev1alpha1.GatewayTLSVolumeName) {
		t.Error("the operator's own TLS volume was dropped")
	}
}

func TestEngineProtectedSecret(t *testing.T) {
	info := InstanceInfo{ProtectedSecretNames: []string{"admin-pw", "inst-auth-signing"}}
	isProtected := engineProtectedSecret(info)

	for _, name := range []string{
		"admin-pw",
		"inst-auth-signing",
		// Generation-numbered engine TLS Secrets are matched by shape, since no
		// single reconcile can enumerate every generation.
		testEngineName + SuffixGen + "0" + SuffixEngineTLS,
		testEngineName + SuffixGen + "17" + SuffixEngineTLS,
	} {
		if !isProtected(name) {
			t.Errorf("Secret %q should be protected", name)
		}
	}
	// A SIBLING engine's per-generation Secret is protected too. It used to be
	// excluded (the shape match was anchored on this engine's name), which let one
	// engine's template mount another engine's serving private key.
	if !isProtected("other-engine" + SuffixGen + "0" + SuffixEngineTLS) {
		t.Error("a sibling engine's per-generation TLS Secret should be protected")
	}
	for _, name := range []string{
		"my-own-secret",
		testEngineName + SuffixGen + "0" + SuffixConfig,
	} {
		if isProtected(name) {
			t.Errorf("Secret %q should not be protected", name)
		}
	}
}

func TestEngineAliasedSecretVolumes(t *testing.T) {
	info := InstanceInfo{ProtectedSecretNames: []string{"admin-pw", "inst-auth-signing"}}
	engine := &computev1alpha1.FireboltEngine{}
	engine.Name = testEngineName
	engine.Spec.Template = &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		Volumes: []corev1.Volume{secretVolume("innocuous", "admin-pw")},
	}}

	errs := engineAliasedSecretVolumes(engine, nil, nil, info)
	if len(errs) != 1 {
		t.Fatalf("want 1 error for the engine template, got %d: %v", len(errs), errs)
	}
	if msg := errs[0].Error(); !strings.Contains(msg, "spec.template.spec.volumes[0]") {
		t.Errorf("error %q does not point at the offending path", msg)
	}

	t.Run("class template is reported under its own path", func(t *testing.T) {
		classInfo := &FireboltEngineClassInfo{Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{secretVolume("cls", "inst-auth-signing")},
		}}}
		errs := engineAliasedSecretVolumes(&computev1alpha1.FireboltEngine{}, classInfo, nil, info)
		if len(errs) != 1 {
			t.Fatalf("want 1 error for the class template, got %d: %v", len(errs), errs)
		}
		if msg := errs[0].Error(); !strings.Contains(msg, "engineClassRef") {
			t.Errorf("error %q should point at the class, not the engine template", msg)
		}
	})

	t.Run("no templates and no auth is clean", func(t *testing.T) {
		if errs := engineAliasedSecretVolumes(&computev1alpha1.FireboltEngine{}, nil, nil, InstanceInfo{}); len(errs) != 0 {
			t.Errorf("want no errors, got %v", errs)
		}
	})
}

func TestValidateInstanceTemplatesRejectsSecretAliases(t *testing.T) {
	inst := &computev1alpha1.FireboltInstance{}
	inst.Name = "inst"
	inst.Status.GatewayTLS = &computev1alpha1.GatewayTLSStatus{SecretName: "inst-gateway-tls"}
	inst.Spec.Gateway.Template = &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		Volumes: []corev1.Volume{secretVolume("innocuous", "inst-gateway-tls")},
	}}
	inst.Spec.Metadata.Template = &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		Volumes: []corev1.Volume{secretVolume("creds", pgCredentialsSecretName("inst"))},
	}}

	gateway, metadata := validateInstanceTemplates(inst)
	if len(gateway) == 0 {
		t.Error("aliasing the gateway TLS Secret must be reported on the gateway list")
	}
	if len(metadata) == 0 {
		t.Error("aliasing the Postgres credentials Secret must be reported on the metadata list")
	}

	t.Run("unrelated secrets pass", func(t *testing.T) {
		clean := inst.DeepCopy()
		clean.Spec.Gateway.Template.Spec.Volumes = []corev1.Volume{secretVolume("mine", "my-secret")}
		clean.Spec.Metadata.Template.Spec.Volumes = []corev1.Volume{secretVolume("mine", "my-secret")}
		gateway, metadata := validateInstanceTemplates(clean)
		if len(gateway) != 0 || len(metadata) != 0 {
			t.Errorf("unrelated Secret volumes must pass: gateway=%v metadata=%v", gateway, metadata)
		}
	})
}

// secretEnvContainer builds a container that reads secretName into its
// environment — the route a volume guard cannot see, since no volume is involved.
func secretEnvContainer(name, secretName string) corev1.Container {
	return corev1.Container{
		Name: name,
		Env: []corev1.EnvVar{{
			Name: "STOLEN",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  "tls.key",
			}},
		}},
	}
}

// secretEnvFromContainer pulls a whole Secret into the environment.
func secretEnvFromContainer(name, secretName string) corev1.Container {
	return corev1.Container{
		Name: name,
		EnvFrom: []corev1.EnvFromSource{{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
			},
		}},
	}
}

func TestValidateEngineSecretEnvRefs(t *testing.T) {
	info := InstanceInfo{ProtectedSecretNames: []string{"admin-pw", "inst-auth-signing"}}
	engine := func(containers, initContainers []corev1.Container) *computev1alpha1.FireboltEngine {
		e := &computev1alpha1.FireboltEngine{}
		e.Name = testEngineName
		e.Spec.Template = &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: containers, InitContainers: initContainers,
		}}
		return e
	}

	cases := []struct {
		name       string
		containers []corev1.Container
		init       []corev1.Container
		wantErr    bool
	}{
		{"sidecar reads a signing key via secretKeyRef",
			[]corev1.Container{secretEnvContainer("sidecar", "inst-auth-signing")}, nil, true},
		{"sidecar reads the admin password via envFrom",
			[]corev1.Container{secretEnvFromContainer("sidecar", "admin-pw")}, nil, true},
		{"init container reads a signing key",
			nil, []corev1.Container{secretEnvContainer("boot", "inst-auth-signing")}, true},
		{"the primary container is not exempt",
			[]corev1.Container{secretEnvContainer(computev1alpha1.EngineContainerName, "admin-pw")}, nil, true},
		{"a per-generation TLS secret is matched by shape",
			[]corev1.Container{secretEnvContainer("sidecar", testEngineName+SuffixGen+"2"+SuffixEngineTLS)}, nil, true},
		{"the author's own Secret passes",
			[]corev1.Container{secretEnvContainer("sidecar", "my-own-secret")}, nil, false},
		{"no env at all passes",
			[]corev1.Container{{Name: "sidecar"}}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateEngineSecretEnvRefs(engine(tc.containers, tc.init), nil, nil, info)
			if got := len(errs) > 0; got != tc.wantErr {
				t.Fatalf("rejected=%v, want %v (errs: %v)", got, tc.wantErr, errs)
			}
		})
	}

	t.Run("the class template is checked too", func(t *testing.T) {
		classInfo := &FireboltEngineClassInfo{Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{secretEnvContainer("cls", "inst-auth-signing")},
		}}}
		errs := validateEngineSecretEnvRefs(&computev1alpha1.FireboltEngine{}, classInfo, nil, info)
		if len(errs) != 1 {
			t.Fatalf("want 1 error for the class template, got %d: %v", len(errs), errs)
		}
		if msg := errs[0].Error(); !strings.Contains(msg, "engineClassRef") {
			t.Errorf("error %q should point at the class", msg)
		}
	})

	t.Run("the Defaults template is checked under its own path", func(t *testing.T) {
		defaultsInfo := &FireboltEngineDefaultsInfo{
			Name: computev1alpha1.FireboltEngineDefaultsDefaultName,
			Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{secretEnvContainer("sid", "inst-auth-signing")},
			}},
		}
		errs := validateEngineSecretEnvRefs(&computev1alpha1.FireboltEngine{}, nil, defaultsInfo, info)
		if len(errs) != 1 {
			t.Fatalf("want 1 error for the Defaults template, got %d: %v", len(errs), errs)
		}
		msg := errs[0].Error()
		if !strings.Contains(msg, "FireboltEngineDefaults") || !strings.Contains(msg, defaultsInfo.Name) {
			t.Errorf("error %q should point at FireboltEngineDefaults %q", msg, defaultsInfo.Name)
		}
		if strings.Contains(msg, "engineClassRef") {
			t.Errorf("error %q should not point at a class", msg)
		}
	})
}

// TestValidateEngineTemplatesBlocksEnvButNotVolumes pins the asymmetry that the
// two routes are handled differently on purpose.
//
// An env reference resolves once at pod start, so refusing to render prevents it
// outright. A Secret volume is re-synced for the life of the pod, so an
// already-running pod can acquire material it did not have when it was admitted —
// and blocking the render would freeze the engine on exactly that pod instead of
// rolling to a clean one.
func TestValidateEngineTemplatesBlocksEnvButNotVolumes(t *testing.T) {
	info := InstanceInfo{ProtectedSecretNames: []string{"admin-pw"}}
	e := &computev1alpha1.FireboltEngine{}
	e.Name = testEngineName
	e.Spec.Template = &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		Volumes: []corev1.Volume{secretVolume("innocuous", "admin-pw")},
	}}
	if errs := validateEngineTemplates(e, nil, nil, info); len(errs) != 0 {
		t.Errorf("a volume alias must not block the render, got %v", errs)
	}
	if errs := engineAliasedSecretVolumes(e, nil, nil, info); len(errs) == 0 {
		t.Error("a volume alias must still be reported")
	}

	e.Spec.Template.Spec.Volumes = nil
	e.Spec.Template.Spec.Containers = []corev1.Container{secretEnvContainer("sidecar", "admin-pw")}
	if errs := validateEngineTemplates(e, nil, nil, info); len(errs) == 0 {
		t.Error("an env Secret reference must block the render")
	}
}

// protectedInfo is the InstanceInfo an engine reconcile would carry for an
// Instance whose auth and TLS are provisioned: the Instance-wide protected set,
// not a per-engine one.
func protectedInfo() InstanceInfo {
	return InstanceInfo{ProtectedSecretNames: []string{
		"admin-pw", "inst-auth-signing", "inst-engine-tls", "inst-gateway-tls",
	}}
}

// nameSet builds the predicate the render-time drop takes, for tests that do not
// have a full FireboltInstance to hand.
func nameSet(names ...string) func(string) bool {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return func(name string) bool {
		_, hit := set[name]
		return hit
	}
}

// TestAppendUserPodVolumesDropsCrossComponentAliases pins the fix for the
// per-component scoping hole: an engine template must not reach a SIBLING
// engine's per-generation serving key, nor the gateway's, neither of which
// appears among this pod's own operator volumes.
func TestAppendUserPodVolumesDropsCrossComponentAliases(t *testing.T) {
	cases := []struct {
		name   string
		secret string
	}{
		{"sibling engine's per-generation serving key", "other-engine-g7-engine-tls"},
		{"gateway's downstream serving key", "inst-gateway-tls"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := &computev1alpha1.FireboltEngineSpec{
				Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{secretVolume("sneaky", tc.secret)},
				}},
			}
			got := appendUserPodVolumes(operatorAuthVolumes(), spec, protectedInfo(), nil)
			if hasVolume(got, "sneaky") {
				t.Fatalf("template aliased %q and was not dropped (rendered: %v)", tc.secret, volumeNames(got))
			}
		})
	}
}

// TestIsGeneratedEngineTLSSecretName pins the shape match that makes the
// cross-engine case above work without enumerating live generations.
func TestIsGeneratedEngineTLSSecretName(t *testing.T) {
	cases := map[string]bool{
		"eng-g1-engine-tls":           true,
		"other-engine-g42-engine-tls": true,
		"inst-engine-tls":             false, // the instance anchor: exact-matched instead
		"my-own-engine-tls":           false, // a user Secret that merely ends the same way
		"eng-g1-config":               false,
	}
	for name, want := range cases {
		if got := isGeneratedEngineTLSSecretName(name); got != want {
			t.Errorf("isGeneratedEngineTLSSecretName(%q) = %v, want %v", name, got, want)
		}
	}
}
