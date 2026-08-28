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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func validClusterFireboltEngineClass() *ClusterFireboltEngineClass {
	return &ClusterFireboltEngineClass{
		ObjectMeta: metav1.ObjectMeta{Name: "s-amd-co"},
		Spec: ClusterFireboltEngineClassSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{"node.kubernetes.io/instance-type": "c6id.2xlarge"},
					Containers: []corev1.Container{{
						Name: EngineContainerName,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
						},
					}},
				},
			},
		},
	}
}

func TestClusterFireboltEngineClassValidator_CreateAcceptsSKU(t *testing.T) {
	v := &ClusterFireboltEngineClassCustomValidator{}
	if _, err := v.ValidateCreate(context.Background(), validClusterFireboltEngineClass()); err != nil {
		t.Fatalf("ValidateCreate: unexpected error on SKU-only spec: %v", err)
	}
}

func TestClusterFireboltEngineClassValidator_RejectsServiceAccount(t *testing.T) {
	cc := validClusterFireboltEngineClass()
	cc.Spec.Template.Spec.ServiceAccountName = "tenant-sa"
	v := &ClusterFireboltEngineClassCustomValidator{}
	_, err := v.ValidateCreate(context.Background(), cc)
	if err == nil {
		t.Fatal("ValidateCreate: expected SKU-only rejection of serviceAccountName")
	}
	if !strings.Contains(err.Error(), "serviceAccountName") {
		t.Errorf("error %q does not name serviceAccountName", err.Error())
	}
}

func TestClusterFireboltEngineClassValidator_RejectsDeleteWhileResolved(t *testing.T) {
	scheme := fireboltEngineClassWebhookScheme(t)
	cc := validClusterFireboltEngineClass()
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		fireboltEngineRefingClass("e1", "ns-a", cc.Name),
		fireboltEngineRefingClass("e2", "ns-b", cc.Name),
		// Namespaced override: this engine must not count.
		&FireboltEngineClass{ObjectMeta: metav1.ObjectMeta{Name: cc.Name, Namespace: "ns-c"}},
		fireboltEngineRefingClass("e3", "ns-c", cc.Name),
	).Build()
	v := &ClusterFireboltEngineClassCustomValidator{Reader: reader}
	_, err := v.ValidateDelete(context.Background(), cc)
	if err == nil {
		t.Fatal("ValidateDelete: expected refusal while engines resolve to the catalog")
	}
	if !strings.Contains(err.Error(), "2 FireboltEngine") {
		t.Errorf("error %q does not mention resolved count 2", err.Error())
	}
}

func TestClusterFireboltEngineClassValidator_AllowsDeleteWhenOnlyNamespacedOverride(t *testing.T) {
	scheme := fireboltEngineClassWebhookScheme(t)
	cc := validClusterFireboltEngineClass()
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&FireboltEngineClass{ObjectMeta: metav1.ObjectMeta{Name: cc.Name, Namespace: "ns-a"}},
		fireboltEngineRefingClass("e1", "ns-a", cc.Name),
	).Build()
	v := &ClusterFireboltEngineClassCustomValidator{Reader: reader}
	if _, err := v.ValidateDelete(context.Background(), cc); err != nil {
		t.Fatalf("ValidateDelete: namespaced override must not block catalog delete: %v", err)
	}
}

func TestClusterFireboltEngineClassValidator_AllowsDeleteWhenUnbound(t *testing.T) {
	scheme := fireboltEngineClassWebhookScheme(t)
	v := &ClusterFireboltEngineClassCustomValidator{Reader: fake.NewClientBuilder().WithScheme(scheme).Build()}
	if _, err := v.ValidateDelete(context.Background(), validClusterFireboltEngineClass()); err != nil {
		t.Fatalf("ValidateDelete: unexpected refusal with no engines: %v", err)
	}
}

func TestClusterFireboltEngineClassValidator_DeleteFailsWithoutReader(t *testing.T) {
	v := &ClusterFireboltEngineClassCustomValidator{}
	if _, err := v.ValidateDelete(context.Background(), validClusterFireboltEngineClass()); err == nil {
		t.Fatal("ValidateDelete: expected error when Reader is nil")
	}
}
