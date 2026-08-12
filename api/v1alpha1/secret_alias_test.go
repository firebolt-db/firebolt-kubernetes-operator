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

package v1alpha1

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestVolumeSecretRefs pins every volume source that can route a Secret
// somewhere. A source missing here is a hole in the alias guard: the guard only
// sees what this reports.
func TestVolumeSecretRefs(t *testing.T) {
	cases := []struct {
		name string
		vol  corev1.Volume
		want []string
	}{
		{"no secret at all", corev1.Volume{
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}, nil},
		{"configMap is not a secret", corev1.Volume{
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "cm"},
			}},
		}, nil},
		{"secret", corev1.Volume{
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "s1"}},
		}, []string{"s1"}},
		// The route a secretName-only guard misses.
		{"projected secret", corev1.Volume{
			VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{
					{ConfigMap: &corev1.ConfigMapProjection{
						LocalObjectReference: corev1.LocalObjectReference{Name: "cm"}}},
					{Secret: &corev1.SecretProjection{
						LocalObjectReference: corev1.LocalObjectReference{Name: "s2"}}},
				},
			}},
		}, []string{"s2"}},
		{"multiple projected secrets", corev1.Volume{
			VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{
					{Secret: &corev1.SecretProjection{
						LocalObjectReference: corev1.LocalObjectReference{Name: "s3"}}},
					{Secret: &corev1.SecretProjection{
						LocalObjectReference: corev1.LocalObjectReference{Name: "s4"}}},
				},
			}},
		}, []string{"s3", "s4"}},
		{"csi node-publish secret", corev1.Volume{
			VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{
				Driver:               "d",
				NodePublishSecretRef: &corev1.LocalObjectReference{Name: "s5"},
			}},
		}, []string{"s5"}},
		{"azureFile secret", corev1.Volume{
			VolumeSource: corev1.VolumeSource{AzureFile: &corev1.AzureFileVolumeSource{SecretName: "s6"}},
		}, []string{"s6"}},
		{"iscsi secret", corev1.Volume{
			VolumeSource: corev1.VolumeSource{ISCSI: &corev1.ISCSIVolumeSource{
				SecretRef: &corev1.LocalObjectReference{Name: "s7"},
			}},
		}, []string{"s7"}},
		{"empty name is not a ref", corev1.Volume{
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: ""}},
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := VolumeSecretRefs(&tc.vol)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("VolumeSecretRefs = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidateNoSecretAliasVolumes(t *testing.T) {
	protectedSet := func(names ...string) func(string) bool {
		return func(n string) bool {
			for _, p := range names {
				if p == n {
					return true
				}
			}
			return false
		}
	}
	secretVol := func(volName, secretName string) corev1.Volume {
		return corev1.Volume{
			Name:         volName,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secretName}},
		}
	}

	t.Run("alias under an innocuous volume name is rejected", func(t *testing.T) {
		errs := ValidateNoSecretAliasVolumes(
			[]corev1.Volume{secretVol("totally-fine", "inst-auth-signing")},
			field.NewPath("spec", "template", "spec", "volumes"),
			protectedSet("inst-auth-signing"), "engine")
		if len(errs) != 1 {
			t.Fatalf("want 1 error, got %d: %v", len(errs), errs)
		}
		msg := errs[0].Error()
		for _, want := range []string{"totally-fine", "inst-auth-signing", "volumes[0]"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error %q does not name %q", msg, want)
			}
		}
	})

	t.Run("unrelated secret passes", func(t *testing.T) {
		errs := ValidateNoSecretAliasVolumes(
			[]corev1.Volume{secretVol("my-own-creds", "sidecar-creds")},
			field.NewPath("spec"), protectedSet("inst-auth-signing"), "engine")
		if len(errs) != 0 {
			t.Errorf("unrelated Secret must pass, got %v", errs)
		}
	})

	t.Run("nil predicate protects nothing", func(t *testing.T) {
		errs := ValidateNoSecretAliasVolumes(
			[]corev1.Volume{secretVol("v", "anything")},
			field.NewPath("spec"), nil, "engine")
		if len(errs) != 0 {
			t.Errorf("want no errors for a nil predicate, got %v", errs)
		}
	})
}

func TestInstanceOperatorSecretNames(t *testing.T) {
	inst := &FireboltInstance{
		Spec: FireboltInstanceSpec{
			Auth: &AuthSpec{Enabled: true, Local: &LocalAuthSpec{
				Admin: AdminSpec{Password: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "admin-pw"}, Key: "password",
				}},
			}},
		},
		Status: FireboltInstanceStatus{
			Auth: &AuthStatus{SigningKeys: []SigningKeyStatus{
				{ID: "key-1", SecretName: "inst-auth-signing"},
				{ID: "key-2", SecretName: "inst-auth-signing-key-2"},
			}},
			EngineTLS: &EngineTLSStatus{SecretName: "inst-engine-tls"},
		},
	}
	got := InstanceOperatorSecretNames(inst)
	want := []string{"admin-pw", "inst-auth-signing", "inst-auth-signing-key-2", "inst-engine-tls"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("InstanceOperatorSecretNames = %v, want %v", got, want)
	}

	t.Run("auth disabled contributes no admin password", func(t *testing.T) {
		off := inst.DeepCopy()
		off.Spec.Auth.Enabled = false
		for _, n := range InstanceOperatorSecretNames(off) {
			if n == "admin-pw" {
				t.Error("admin password Secret must not be protected while auth is disabled")
			}
		}
	})

	t.Run("unprovisioned instance protects nothing", func(t *testing.T) {
		if got := InstanceOperatorSecretNames(&FireboltInstance{}); len(got) != 0 {
			t.Errorf("want no names, got %v", got)
		}
	})
}

// TestFireboltEngineValidator_RejectsSecretAliasVolume covers the admission
// half: the Instance is read live, so the rejection names the Secret actually
// mounted.
func TestFireboltEngineValidator_RejectsSecretAliasVolume(t *testing.T) {
	sch := runtime.NewScheme()
	if err := scheme.AddToScheme(sch); err != nil {
		t.Fatalf("scheme.AddToScheme: %v", err)
	}
	if err := AddToScheme(sch); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	inst := &FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "default"},
		Status: FireboltInstanceStatus{
			Auth: &AuthStatus{SigningKeys: []SigningKeyStatus{{ID: "key-1", SecretName: "inst-auth-signing"}}},
		},
	}
	reader := fake.NewClientBuilder().WithScheme(sch).WithObjects(client.Object(inst)).Build()
	v := &FireboltEngineCustomValidator{Reader: reader}

	withVolume := func(volName, secretName string) *FireboltEngine {
		eng := fireboltEngineWithRef(nil)
		eng.Spec.Template = &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name:         volName,
				VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secretName}},
			}},
		}}
		return eng
	}

	_, err := v.ValidateCreate(context.Background(), withVolume("innocuous", "inst-auth-signing"))
	if err == nil {
		t.Fatal("aliasing the signing-key Secret must be rejected")
	}
	if !strings.Contains(err.Error(), "inst-auth-signing") {
		t.Errorf("error %q does not name the aliased Secret", err.Error())
	}

	if _, err := v.ValidateCreate(context.Background(), withVolume("mine", "my-secret")); err != nil {
		t.Errorf("an unrelated Secret volume must pass, got %v", err)
	}

	t.Run("missing instance does not block admission", func(t *testing.T) {
		eng := withVolume("innocuous", "inst-auth-signing")
		eng.Spec.InstanceRef = "not-created-yet"
		if _, err := v.ValidateCreate(context.Background(), eng); err != nil {
			t.Errorf("an unresolvable Instance must not fail admission, got %v", err)
		}
	})
}
