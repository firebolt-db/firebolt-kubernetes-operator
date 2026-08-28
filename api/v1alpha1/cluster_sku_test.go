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
