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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

func newDefaultsFixture(name string) *computev1alpha1.FireboltEngineDefaults {
	return &computev1alpha1.FireboltEngineDefaults{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "firebolt",
			Generation: 1,
			Finalizers: []string{engineDefaultsFinalizerName},
		},
		Spec: computev1alpha1.FireboltEngineDefaultsSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{ServiceAccountName: "engine-sa"},
			},
		},
	}
}

func TestFireboltEngineDefaultsReconcile_CountsNamespaceEngines(t *testing.T) {
	sch := engineClassTestScheme(t)
	defaults := newDefaultsFixture(computev1alpha1.FireboltEngineDefaultsDefaultName)
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(
			defaults,
			newEngineFixture("a", "firebolt", "sku-a"),
			newEngineFixture("b", "firebolt", ""),
			newEngineFixture("c", "other-ns", "sku-a"),
		).
		WithStatusSubresource(&computev1alpha1.FireboltEngineDefaults{}).
		Build()

	r := &FireboltEngineDefaultsReconciler{Client: cli, Reader: cli, Scheme: sch}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: defaults.Name, Namespace: "firebolt"},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	updated := &computev1alpha1.FireboltEngineDefaults{}
	if err := cli.Get(context.Background(), client.ObjectKey{Name: defaults.Name, Namespace: "firebolt"}, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.Status.BoundEngines != 2 {
		t.Errorf("BoundEngines = %d, want 2 (every engine in the namespace, regardless of class ref)", updated.Status.BoundEngines)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.FireboltEngineDefaultsConditionReady)
	if cond == nil {
		t.Fatal("Ready condition missing")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("Ready.Status = %s, want True", cond.Status)
	}
	if cond.Reason != reasonAdmissible {
		t.Errorf("Ready.Reason = %q, want %s", cond.Reason, reasonAdmissible)
	}
}

func TestFireboltEngineDefaultsReconcile_DefenseInDepthRejectsOwnedFields(t *testing.T) {
	sch := engineClassTestScheme(t)
	defaults := newDefaultsFixture("bad")
	defaults.Spec.Template.Spec.Subdomain = "headless"
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(defaults).
		WithStatusSubresource(&computev1alpha1.FireboltEngineDefaults{}).
		Build()

	r := &FireboltEngineDefaultsReconciler{Client: cli, Reader: cli, Scheme: sch}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: "bad", Namespace: "firebolt"},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	updated := &computev1alpha1.FireboltEngineDefaults{}
	if err := cli.Get(context.Background(), client.ObjectKey{Name: "bad", Namespace: "firebolt"}, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.FireboltEngineDefaultsConditionReady)
	if cond == nil {
		t.Fatal("Ready condition missing")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Ready.Status = %s, want False", cond.Status)
	}
	if cond.Reason != reasonOperatorOwnedFieldSet {
		t.Errorf("Ready.Reason = %q, want %q", cond.Reason, reasonOperatorOwnedFieldSet)
	}
}

func TestFireboltEngineDefaultsReconcile_AddsFinalizerOnFirstReconcile(t *testing.T) {
	sch := engineClassTestScheme(t)
	defaults := newDefaultsFixture("fresh")
	defaults.Finalizers = nil
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(defaults).
		WithStatusSubresource(&computev1alpha1.FireboltEngineDefaults{}).
		Build()

	r := &FireboltEngineDefaultsReconciler{Client: cli, Reader: cli, Scheme: sch}
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: "fresh", Namespace: "firebolt"},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !res.Requeue {
		t.Error("Requeue = false, want true after finalizer add")
	}

	updated := &computev1alpha1.FireboltEngineDefaults{}
	if err := cli.Get(context.Background(), client.ObjectKey{Name: "fresh", Namespace: "firebolt"}, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !containsString(updated.Finalizers, engineDefaultsFinalizerName) {
		t.Errorf("Finalizers = %v, want %q included", updated.Finalizers, engineDefaultsFinalizerName)
	}
}

func TestFireboltEngineDefaultsReconcile_DeletionBlockedWhileEnginesExist(t *testing.T) {
	sch := engineClassTestScheme(t)
	defaults := newDefaultsFixture("doomed")
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(defaults, newEngineFixture("a", "firebolt", "")).
		WithStatusSubresource(&computev1alpha1.FireboltEngineDefaults{}).
		Build()

	if err := cli.Delete(context.Background(), defaults); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	r := &FireboltEngineDefaultsReconciler{Client: cli, Reader: cli, Scheme: sch}
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: "doomed", Namespace: "firebolt"},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("RequeueAfter = 0, want %s (deletion held)", engineDefaultsRequeueAfter)
	}

	updated := &computev1alpha1.FireboltEngineDefaults{}
	if err := cli.Get(context.Background(), client.ObjectKey{Name: "doomed", Namespace: "firebolt"}, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !containsString(updated.Finalizers, engineDefaultsFinalizerName) {
		t.Errorf("Finalizers = %v, want %q still present", updated.Finalizers, engineDefaultsFinalizerName)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.FireboltEngineDefaultsConditionReady)
	if cond == nil {
		t.Fatal("Ready condition missing")
	}
	if cond.Reason != reasonDeletionBlocked {
		t.Errorf("Ready.Reason = %q, want %q", cond.Reason, reasonDeletionBlocked)
	}
	if updated.Status.BoundEngines != 1 {
		t.Errorf("BoundEngines = %d, want 1", updated.Status.BoundEngines)
	}
}

func TestFireboltEngineDefaultsReconcile_DeletionAllowedWhenNoEngines(t *testing.T) {
	sch := engineClassTestScheme(t)
	defaults := newDefaultsFixture("orphan")
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(defaults).
		WithStatusSubresource(&computev1alpha1.FireboltEngineDefaults{}).
		Build()

	if err := cli.Delete(context.Background(), defaults); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	r := &FireboltEngineDefaultsReconciler{Client: cli, Reader: cli, Scheme: sch}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: "orphan", Namespace: "firebolt"},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	updated := &computev1alpha1.FireboltEngineDefaults{}
	err := cli.Get(context.Background(), client.ObjectKey{Name: "orphan", Namespace: "firebolt"}, updated)
	if err == nil {
		t.Fatalf("Get: expected NotFound after finalizer removal, got finalizers=%v", updated.Finalizers)
	}
	if !errors.IsNotFound(err) {
		t.Fatalf("Get: expected NotFound, got %v", err)
	}
}

func TestFireboltEngineDefaultsReconcile_NotFoundIsNoOp(t *testing.T) {
	sch := engineClassTestScheme(t)
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithStatusSubresource(&computev1alpha1.FireboltEngineDefaults{}).
		Build()

	r := &FireboltEngineDefaultsReconciler{Client: cli, Reader: cli, Scheme: sch}
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: "missing", Namespace: "firebolt"},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Requeue || res.RequeueAfter != 0 {
		t.Errorf("unexpected requeue on NotFound: %+v", res)
	}
}

func containsString(items []string, want string) bool {
	for _, s := range items {
		if s == want {
			return true
		}
	}
	return false
}
