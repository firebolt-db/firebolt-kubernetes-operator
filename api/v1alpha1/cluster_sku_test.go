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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func validSKUTemplate() *corev1.PodTemplateSpec {
	return &corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{"node.kubernetes.io/instance-type": "c6id.2xlarge"},
			Containers: []corev1.Container{{
				Name: EngineContainerName,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("6120m"),
						corev1.ResourceMemory: resource.MustParse("11623Mi"),
					},
				},
			}},
		},
	}
}

func TestValidateClusterEngineClassSKUOnly_AcceptsSKUTemplate(t *testing.T) {
	errs := ValidateClusterEngineClassSKUOnly(validSKUTemplate(), field.NewPath("spec", "template"))
	if len(errs) != 0 {
		t.Fatalf("valid SKU template rejected: %v", errs.ToAggregate())
	}
}

// TestValidateClusterEngineClassSKUOnly_AcceptsNamespaceIndependentVolumes
// pins the other edge of the ConfigMap/PVC rejection: a volume source is
// forbidden because it binds an object the catalog author cannot see in the
// consumer's namespace, not because it is storage. emptyDir and downwardAPI
// name nothing, and an ephemeral claim template creates a fresh PVC in the
// consumer's own namespace from a cluster-scoped StorageClass, so all three
// resolve identically wherever the catalog is consumed.
func TestValidateClusterEngineClassSKUOnly_AcceptsNamespaceIndependentVolumes(t *testing.T) {
	tmpl := validSKUTemplate()
	storageClass := "gp3"
	tmpl.Spec.Volumes = []corev1.Volume{
		{
			Name:         "scratch",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		{
			Name: "podinfo",
			VolumeSource: corev1.VolumeSource{
				DownwardAPI: &corev1.DownwardAPIVolumeSource{
					Items: []corev1.DownwardAPIVolumeFile{{
						Path:     "labels",
						FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.labels"},
					}},
				},
			},
		},
		{
			Name: "ephemeral-data",
			VolumeSource: corev1.VolumeSource{
				Ephemeral: &corev1.EphemeralVolumeSource{
					VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{
						Spec: corev1.PersistentVolumeClaimSpec{
							StorageClassName: &storageClass,
							AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						},
					},
				},
			},
		},
	}
	errs := ValidateClusterEngineClassSKUOnly(tmpl, field.NewPath("spec", "template"))
	if len(errs) != 0 {
		t.Fatalf("namespace-independent volumes rejected: %v", errs.ToAggregate())
	}
}

func TestValidateClusterEngineClassSKUOnly_RejectsNamespaceIdentifiers(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*corev1.PodTemplateSpec)
		wantField string
	}{
		{
			name: "serviceAccountName",
			mutate: func(tmpl *corev1.PodTemplateSpec) {
				tmpl.Spec.ServiceAccountName = "workload-sa"
			},
			wantField: "serviceAccountName",
		},
		{
			name: "imagePullSecrets",
			mutate: func(tmpl *corev1.PodTemplateSpec) {
				tmpl.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "regcred"}}
			},
			wantField: "imagePullSecrets",
		},
		{
			name: "IAM annotation",
			mutate: func(tmpl *corev1.PodTemplateSpec) {
				tmpl.Annotations = map[string]string{annotationIAMRole: "arn:aws:iam::1:role/x"}
			},
			wantField: annotationIAMRole,
		},
		{
			name: "Secret volume",
			mutate: func(tmpl *corev1.PodTemplateSpec) {
				tmpl.Spec.Volumes = []corev1.Volume{{
					Name: "creds",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{SecretName: "aws-creds"},
					},
				}}
			},
			wantField: "volumes",
		},
		{
			name: "Secret env",
			mutate: func(tmpl *corev1.PodTemplateSpec) {
				tmpl.Spec.Containers[0].Env = []corev1.EnvVar{{
					Name: "AWS_SECRET_ACCESS_KEY",
					ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "aws-creds"},
							Key:                  "key",
						},
					},
				}}
			},
			wantField: "containers",
		},
		{
			name: "ConfigMap volume",
			mutate: func(tmpl *corev1.PodTemplateSpec) {
				tmpl.Spec.Volumes = []corev1.Volume{{
					Name: "engine-tuning",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: "tuning"},
						},
					},
				}}
			},
			wantField: "volumes",
		},
		{
			name: "projected ConfigMap volume",
			mutate: func(tmpl *corev1.PodTemplateSpec) {
				tmpl.Spec.Volumes = []corev1.Volume{{
					Name: "engine-tuning",
					VolumeSource: corev1.VolumeSource{
						Projected: &corev1.ProjectedVolumeSource{
							Sources: []corev1.VolumeProjection{{
								ConfigMap: &corev1.ConfigMapProjection{
									LocalObjectReference: corev1.LocalObjectReference{Name: "tuning"},
								},
							}},
						},
					},
				}}
			},
			wantField: "volumes",
		},
		{
			name: "persistentVolumeClaim volume",
			mutate: func(tmpl *corev1.PodTemplateSpec) {
				tmpl.Spec.Volumes = []corev1.Volume{{
					Name: "scratch",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "scratch-claim",
						},
					},
				}}
			},
			wantField: "volumes",
		},
		{
			name: "ConfigMap env",
			mutate: func(tmpl *corev1.PodTemplateSpec) {
				tmpl.Spec.Containers[0].Env = []corev1.EnvVar{{
					Name: "TUNING",
					ValueFrom: &corev1.EnvVarSource{
						ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "tuning"},
							Key:                  "level",
						},
					},
				}}
			},
			wantField: "containers",
		},
		{
			name: "ConfigMap envFrom",
			mutate: func(tmpl *corev1.PodTemplateSpec) {
				tmpl.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{
					ConfigMapRef: &corev1.ConfigMapEnvSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: "tuning"},
					},
				}}
			},
			wantField: "containers",
		},
		{
			name: "init container ConfigMap env",
			mutate: func(tmpl *corev1.PodTemplateSpec) {
				tmpl.Spec.InitContainers = []corev1.Container{{
					Name: "node-setup",
					EnvFrom: []corev1.EnvFromSource{{
						ConfigMapRef: &corev1.ConfigMapEnvSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: "tuning"},
						},
					}},
				}}
			},
			wantField: "initContainers",
		},
		{
			name: "resourceClaims naming a ResourceClaim",
			mutate: func(tmpl *corev1.PodTemplateSpec) {
				name := "shared-gpu"
				tmpl.Spec.ResourceClaims = []corev1.PodResourceClaim{{
					Name:              "gpu",
					ResourceClaimName: &name,
				}}
			},
			wantField: "resourceClaims",
		},
		{
			name: "resourceClaims naming a ResourceClaimTemplate",
			mutate: func(tmpl *corev1.PodTemplateSpec) {
				name := "gpu-template"
				tmpl.Spec.ResourceClaims = []corev1.PodResourceClaim{{
					Name:                      "gpu",
					ResourceClaimTemplateName: &name,
				}}
			},
			wantField: "resourceClaimTemplateName",
		},
		{
			name: "engine container resources.claims",
			mutate: func(tmpl *corev1.PodTemplateSpec) {
				tmpl.Spec.Containers[0].Resources.Claims = []corev1.ResourceClaim{{Name: "gpu"}}
			},
			wantField: "resources.claims",
		},
		{
			name: "init container resources.claims",
			mutate: func(tmpl *corev1.PodTemplateSpec) {
				tmpl.Spec.InitContainers = []corev1.Container{{
					Name:      "node-setup",
					Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "gpu"}}},
				}}
			},
			wantField: "initContainers",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := validSKUTemplate()
			tc.mutate(tmpl)
			errs := ValidateClusterEngineClassSKUOnly(tmpl, field.NewPath("spec", "template"))
			if len(errs) == 0 {
				t.Fatal("expected rejection, got none")
			}
			got := errs.ToAggregate().Error()
			if !strings.Contains(got, tc.wantField) {
				t.Errorf("error %q does not name %q", got, tc.wantField)
			}
			if !strings.Contains(got, "SKU-only") {
				t.Errorf("error %q does not explain the SKU-only lock", got)
			}
		})
	}
}
