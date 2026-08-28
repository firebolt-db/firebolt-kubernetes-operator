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
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

func newClusterClassFixture(name string) *computev1alpha1.ClusterFireboltEngineClass {
	return &computev1alpha1.ClusterFireboltEngineClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Generation: 1,
			Finalizers: []string{clusterEngineClassFinalizerName},
		},
		Spec: computev1alpha1.ClusterFireboltEngineClassSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{"node.kubernetes.io/instance-type": "c6id.2xlarge"},
					Containers: []corev1.Container{{
						Name: computev1alpha1.EngineContainerName,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
						},
					}},
				},
			},
		},
	}
}

func TestClusterFireboltEngineClassReconcile_CountsResolvedEngines(t *testing.T) {
	sch := engineClassTestScheme(t)
	class := newClusterClassFixture("s-amd-co")
	objs := []client.Object{
		class,
		newEngineFixture("a", "ns-a", "s-amd-co"),
		newEngineFixture("b", "ns-b", "s-amd-co"),
		newEngineFixture("c", "ns-a", "other"),
		newEngineFixture("d", "ns-c", ""),
		// Namespaced override in ns-d: engine e must not count.
		newClassFixtureIn("s-amd-co", "ns-d"),
		newEngineFixture("e", "ns-d", "s-amd-co"),
	}
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(objs...).
		WithStatusSubresource(&computev1alpha1.ClusterFireboltEngineClass{}).
		Build()

	r := &ClusterFireboltEngineClassReconciler{Client: cli, Reader: cli, Scheme: sch}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "s-amd-co"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	updated := &computev1alpha1.ClusterFireboltEngineClass{}
	if err := cli.Get(context.Background(), types.NamespacedName{Name: "s-amd-co"}, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.Status.BoundEngines != 2 {
		t.Errorf("BoundEngines = %d, want 2 (a and b; e has a namespaced override)", updated.Status.BoundEngines)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.ClusterFireboltEngineClassConditionReady)
	if cond == nil {
		t.Fatal("Ready condition missing")
	}
	if cond.Status != metav1.ConditionTrue || cond.Reason != reasonAdmissible {
		t.Errorf("Ready = %s/%s, want True/%s", cond.Status, cond.Reason, reasonAdmissible)
	}
}

func TestClusterFireboltEngineClassReconcile_SKUOnlyRejectsServiceAccount(t *testing.T) {
	sch := engineClassTestScheme(t)
	class := newClusterClassFixture("leaky")
	class.Spec.Template.Spec.ServiceAccountName = "tenant-sa"
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(class).
		WithStatusSubresource(&computev1alpha1.ClusterFireboltEngineClass{}).
		Build()

	r := &ClusterFireboltEngineClassReconciler{Client: cli, Reader: cli, Scheme: sch}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "leaky"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	updated := &computev1alpha1.ClusterFireboltEngineClass{}
	if err := cli.Get(context.Background(), types.NamespacedName{Name: "leaky"}, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.ClusterFireboltEngineClassConditionReady)
	if cond == nil {
		t.Fatal("Ready condition missing")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Ready.Status = %s, want False", cond.Status)
	}
	if cond.Reason != reasonNamespaceResolvedFieldSet {
		t.Errorf("Ready.Reason = %q, want %q", cond.Reason, reasonNamespaceResolvedFieldSet)
	}
}

func TestClusterFireboltEngineClassReconcile_AddsFinalizerOnFirstReconcile(t *testing.T) {
	sch := engineClassTestScheme(t)
	class := newClusterClassFixture("fresh")
	class.Finalizers = nil
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(class).
		WithStatusSubresource(&computev1alpha1.ClusterFireboltEngineClass{}).
		Build()

	r := &ClusterFireboltEngineClassReconciler{Client: cli, Reader: cli, Scheme: sch}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "fresh"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !res.Requeue {
		t.Error("Requeue = false, want true after finalizer add")
	}

	updated := &computev1alpha1.ClusterFireboltEngineClass{}
	if err := cli.Get(context.Background(), types.NamespacedName{Name: "fresh"}, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !controllerutil.ContainsFinalizer(updated, clusterEngineClassFinalizerName) {
		t.Errorf("Finalizers = %v, want %q included", updated.Finalizers, clusterEngineClassFinalizerName)
	}
}

func TestClusterFireboltEngineClassReconcile_DeletionBlockedWhileResolved(t *testing.T) {
	sch := engineClassTestScheme(t)
	class := newClusterClassFixture("doomed")
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(class, newEngineFixture("a", "ns-a", "doomed")).
		WithStatusSubresource(&computev1alpha1.ClusterFireboltEngineClass{}).
		Build()

	if err := cli.Delete(context.Background(), class); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	r := &ClusterFireboltEngineClassReconciler{Client: cli, Reader: cli, Scheme: sch}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "doomed"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("RequeueAfter = 0, want %s (deletion held)", engineClassRequeueAfter)
	}

	updated := &computev1alpha1.ClusterFireboltEngineClass{}
	if err := cli.Get(context.Background(), types.NamespacedName{Name: "doomed"}, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !controllerutil.ContainsFinalizer(updated, clusterEngineClassFinalizerName) {
		t.Errorf("Finalizers = %v, want %q still present", updated.Finalizers, clusterEngineClassFinalizerName)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.ClusterFireboltEngineClassConditionReady)
	if cond == nil || cond.Reason != reasonDeletionBlocked {
		t.Fatalf("Ready = %+v, want DeletionBlocked", cond)
	}
	if updated.Status.BoundEngines != 1 {
		t.Errorf("BoundEngines = %d, want 1", updated.Status.BoundEngines)
	}
}

func TestClusterFireboltEngineClassReconcile_DeletionAllowedWhenOnlyOverride(t *testing.T) {
	sch := engineClassTestScheme(t)
	class := newClusterClassFixture("orphan")
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(
			class,
			newClassFixtureIn("orphan", "ns-a"),
			newEngineFixture("e", "ns-a", "orphan"),
		).
		WithStatusSubresource(&computev1alpha1.ClusterFireboltEngineClass{}).
		Build()

	if err := cli.Delete(context.Background(), class); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	r := &ClusterFireboltEngineClassReconciler{Client: cli, Reader: cli, Scheme: sch}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "orphan"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	updated := &computev1alpha1.ClusterFireboltEngineClass{}
	if err := cli.Get(context.Background(), types.NamespacedName{Name: "orphan"}, updated); err != nil {
		if errors.IsNotFound(err) {
			return
		}
		t.Fatalf("Get: %v", err)
	}
	if controllerutil.ContainsFinalizer(updated, clusterEngineClassFinalizerName) {
		t.Errorf("Finalizers = %v, want deletion-guard released (namespaced override must not block)", updated.Finalizers)
	}
}

func TestClusterEngineClassToEngines_ClusterWide(t *testing.T) {
	sch := engineClassTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
		newEngineFixture("a", "ns-a", "s-amd-co"),
		newEngineFixture("b", "ns-b", "s-amd-co"),
		newEngineFixture("c", "ns-a", "other"),
	).Build()
	r := engineRefTestReconciler(cli, sch)

	got := r.clusterEngineClassToEngines(context.Background(), &computev1alpha1.ClusterFireboltEngineClass{
		ObjectMeta: metav1.ObjectMeta{Name: "s-amd-co"},
	})
	if len(got) != 2 {
		t.Fatalf("enqueued = %d, want 2 (every namespace that names the catalog)", len(got))
	}
}
