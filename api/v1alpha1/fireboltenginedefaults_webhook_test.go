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
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// validFireboltEngineDefaults returns a FireboltEngineDefaults whose
// spec.template contains only user-allowed fields. Lives in namespace
// "firebolt" so delete-webhook tests exercise the same-namespace filter.
func validFireboltEngineDefaults() *FireboltEngineDefaults {
	return &FireboltEngineDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: FireboltEngineDefaultsDefaultName, Namespace: "firebolt"},
		Spec: FireboltEngineDefaultsSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"team": "data"},
					Annotations: map[string]string{"iam.amazonaws.com/role": "arn:aws:iam::1:role/x"},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "engine-sa",
					Containers: []corev1.Container{{
						Name: EngineContainerName,
						Env: []corev1.EnvVar{{
							Name: "AWS_ACCESS_KEY_ID",
							ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "creds"},
									Key:                  "AWS_ACCESS_KEY_ID",
								},
							},
						}},
					}},
				},
			},
		},
	}
}

func TestFireboltEngineDefaultsValidator_CreateAcceptsValid(t *testing.T) {
	v := &FireboltEngineDefaultsCustomValidator{}
	if _, err := v.ValidateCreate(context.Background(), validFireboltEngineDefaults()); err != nil {
		t.Fatalf("ValidateCreate: unexpected error on valid spec: %v", err)
	}
}

func TestFireboltEngineDefaultsValidator_UpdateAcceptsValid(t *testing.T) {
	v := &FireboltEngineDefaultsCustomValidator{}
	if _, err := v.ValidateUpdate(context.Background(), validFireboltEngineDefaults(), validFireboltEngineDefaults()); err != nil {
		t.Fatalf("ValidateUpdate: unexpected error on valid spec: %v", err)
	}
}

func TestFireboltEngineDefaultsValidator_RejectsOwnedFields(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*FireboltEngineDefaults)
		wantField string
	}{
		{
			name: "reserved label prefix",
			mutate: func(d *FireboltEngineDefaults) {
				d.Spec.Template.Labels["firebolt.io/config-hash"] = "abc"
			},
			wantField: "spec.template.metadata.labels",
		},
		{
			name: "pod terminationGracePeriodSeconds",
			mutate: func(d *FireboltEngineDefaults) {
				v := int64(30)
				d.Spec.Template.Spec.TerminationGracePeriodSeconds = &v
			},
			wantField: "spec.template.spec.terminationGracePeriodSeconds",
		},
		{
			name: "reserved engine env key",
			mutate: func(d *FireboltEngineDefaults) {
				d.Spec.Template.Spec.Containers[0].Env = append(
					d.Spec.Template.Spec.Containers[0].Env,
					corev1.EnvVar{Name: "POD_INDEX", Value: "0"},
				)
			},
			wantField: "spec.template.spec.containers[0].env",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := validFireboltEngineDefaults()
			tt.mutate(d)
			v := &FireboltEngineDefaultsCustomValidator{}
			_, err := v.ValidateCreate(context.Background(), d)
			if err == nil {
				t.Fatalf("ValidateCreate: expected error mentioning %q", tt.wantField)
			}
			if !strings.Contains(err.Error(), tt.wantField) {
				t.Errorf("error %q does not mention field path %q", err, tt.wantField)
			}
		})
	}
}

func TestFireboltEngineDefaultsValidator_DeleteRefusesWhileEnginesExist(t *testing.T) {
	scheme := fireboltEngineClassWebhookScheme(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&FireboltEngine{
			ObjectMeta: metav1.ObjectMeta{Name: "analytics", Namespace: "firebolt"},
			Spec:       FireboltEngineSpec{InstanceRef: "inst", Replicas: 1},
		},
	).Build()
	v := &FireboltEngineDefaultsCustomValidator{Reader: cli}
	_, err := v.ValidateDelete(context.Background(), validFireboltEngineDefaults())
	if err == nil {
		t.Fatal("ValidateDelete: expected error while a FireboltEngine exists in the namespace")
	}
	if !strings.Contains(err.Error(), "referenced by 1 FireboltEngine") &&
		!strings.Contains(err.Error(), "1 FireboltEngine") {
		t.Errorf("error %q does not mention the bound engine count", err)
	}
}

func TestFireboltEngineDefaultsValidator_DeleteAllowsWhenNoEngines(t *testing.T) {
	scheme := fireboltEngineClassWebhookScheme(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).Build()
	v := &FireboltEngineDefaultsCustomValidator{Reader: cli}
	if _, err := v.ValidateDelete(context.Background(), validFireboltEngineDefaults()); err != nil {
		t.Fatalf("ValidateDelete: unexpected error with no engines: %v", err)
	}
}

func TestFireboltEngineDefaultsValidator_DeleteIgnoresOtherNamespaceEngines(t *testing.T) {
	scheme := fireboltEngineClassWebhookScheme(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&FireboltEngine{
			ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "other-ns"},
			Spec:       FireboltEngineSpec{InstanceRef: "inst", Replicas: 1, EngineClassRef: ptr.To("x")},
		},
	).Build()
	v := &FireboltEngineDefaultsCustomValidator{Reader: cli}
	if _, err := v.ValidateDelete(context.Background(), validFireboltEngineDefaults()); err != nil {
		t.Fatalf("ValidateDelete: engine in another namespace must not block: %v", err)
	}
}

func TestFireboltEngineDefaultsValidator_DeleteRequiresReader(t *testing.T) {
	v := &FireboltEngineDefaultsCustomValidator{}
	_, err := v.ValidateDelete(context.Background(), validFireboltEngineDefaults())
	if err == nil {
		t.Fatal("ValidateDelete: expected error when Reader is nil")
	}
}
