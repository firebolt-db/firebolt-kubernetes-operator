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
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

func engineClassTestScheme(t *testing.T) *runtime.Scheme {
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

// newClassFixture returns an EngineClass whose spec.template is valid
// (passes ValidateOperatorOwnedPodTemplate). The status starts zeroed so
// the reconciler's first pass produces a deterministic Status.Update.
// The deletion-guard finalizer is pre-stamped so tests focused on the
// status path don't have to call Reconcile twice (first call adds the
// finalizer and Requeues, returning before any status work happens).
// Tests that explicitly cover the finalizer-add path should drop the
// finalizer on the returned fixture before priming the client.
// EngineClass is namespaced; tests use namespace "firebolt" by default.
func newClassFixture(name string) *computev1alpha1.EngineClass {
	return newClassFixtureIn(name, "firebolt")
}

func newClassFixtureIn(name, namespace string) *computev1alpha1.EngineClass {
	return &computev1alpha1.EngineClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  namespace,
			Generation: 1,
			Finalizers: []string{engineClassFinalizerName},
		},
		Spec: computev1alpha1.EngineClassSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: "my-sa",
				},
			},
		},
	}
}

// newEngineFixture returns a FireboltEngine referencing the given class
// (nil for an engine that does not reference any class).
func newEngineFixture(name, namespace, classRef string) *computev1alpha1.FireboltEngine {
	spec := computev1alpha1.FireboltEngineSpec{
		InstanceRef: "inst",
		Replicas:    1,
	}
	if classRef != "" {
		ref := classRef
		spec.EngineClassRef = &ref
	}
	return &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       spec,
	}
}

func TestEngineClassReconcile_CountsBoundEngines(t *testing.T) {
	sch := engineClassTestScheme(t)
	class := newClassFixtureIn("compute-optimized", "firebolt")
	objs := []client.Object{
		class,
		// Same namespace, same ref → counted.
		newEngineFixture("a", "firebolt", "compute-optimized"),
		newEngineFixture("b", "firebolt", "compute-optimized"),
		// Same namespace, different ref → not counted.
		newEngineFixture("c", "firebolt", "other-class"),
		// Same namespace, no ref → not counted.
		newEngineFixture("d", "firebolt", ""),
		// Different namespace, matching name → NOT counted. EngineClass is
		// namespaced, so an engine in another namespace cannot bind to
		// this class. Includes a same-named EngineClass in that other
		// namespace so the cross-namespace separation is exercised end
		// to end.
		newClassFixtureIn("compute-optimized", "other-ns"),
		newEngineFixture("e", "other-ns", "compute-optimized"),
	}
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(objs...).
		WithStatusSubresource(&computev1alpha1.EngineClass{}).
		Build()

	r := &EngineClassReconciler{Client: cli, Scheme: sch}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "compute-optimized", Namespace: "firebolt"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	updated := &computev1alpha1.EngineClass{}
	if err := cli.Get(context.Background(), client.ObjectKey{Name: "compute-optimized", Namespace: "firebolt"}, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.Status.BoundEngines != 2 {
		t.Errorf("BoundEngines = %d, want 2 (engines a and b in firebolt reference this class; e in other-ns binds to its own class)", updated.Status.BoundEngines)
	}
	if updated.Status.ObservedGeneration != 1 {
		t.Errorf("ObservedGeneration = %d, want 1", updated.Status.ObservedGeneration)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.EngineClassConditionReady)
	if cond == nil {
		t.Fatal("Ready condition missing")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("Ready.Status = %s, want True", cond.Status)
	}
	if cond.Reason != "Admissible" {
		t.Errorf("Ready.Reason = %q, want Admissible", cond.Reason)
	}
}

func TestEngineClassReconcile_DefenseInDepthRejectsOwnedFields(t *testing.T) {
	sch := engineClassTestScheme(t)
	class := newClassFixture("bad-class")
	// Simulate an EngineClass admitted by an older operator whose
	// rejection set did not yet cover Subdomain. The reconciler must mark
	// the class Ready=False with the offending path in the message.
	class.Spec.Template.Spec.Subdomain = "headless"
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(class).
		WithStatusSubresource(&computev1alpha1.EngineClass{}).
		Build()

	r := &EngineClassReconciler{Client: cli, Scheme: sch}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "bad-class", Namespace: "firebolt"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	updated := &computev1alpha1.EngineClass{}
	if err := cli.Get(context.Background(), client.ObjectKey{Name: "bad-class", Namespace: "firebolt"}, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.EngineClassConditionReady)
	if cond == nil {
		t.Fatal("Ready condition missing")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Ready.Status = %s, want False", cond.Status)
	}
	if cond.Reason != "OperatorOwnedFieldSet" {
		t.Errorf("Ready.Reason = %q, want OperatorOwnedFieldSet", cond.Reason)
	}
}

func TestEngineClassReconcile_NotFoundIsNoOp(t *testing.T) {
	sch := engineClassTestScheme(t)
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithStatusSubresource(&computev1alpha1.EngineClass{}).
		Build()

	r := &EngineClassReconciler{Client: cli, Scheme: sch}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "missing", Namespace: "firebolt"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Requeue {
		t.Error("Requeue = true, want false on NotFound")
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %s, want zero on NotFound", res.RequeueAfter)
	}
}

func TestEngineClassReconcile_IdempotentWhenNoChange(t *testing.T) {
	sch := engineClassTestScheme(t)
	class := newClassFixture("steady")
	class.Status.BoundEngines = 0
	class.Status.ObservedGeneration = 1
	apimeta.SetStatusCondition(&class.Status.Conditions, metav1.Condition{
		Type:               computev1alpha1.EngineClassConditionReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 1,
		Reason:             "Admissible",
		Message:            "spec.template contains no operator-owned paths",
	})
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(class).
		WithStatusSubresource(&computev1alpha1.EngineClass{}).
		Build()

	r := &EngineClassReconciler{Client: cli, Scheme: sch}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "steady", Namespace: "firebolt"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("RequeueAfter = 0, want %s (steady-state drift recheck)", engineClassRequeueAfter)
	}
}

// TestEngineClassReconcile_AddsFinalizerOnFirstReconcile pins the
// finalizer-add path: a class created without the deletion-guard
// finalizer must have it added by the first reconcile, with a Requeue
// so no other work fires until the persisted finalizer is observable
// on a subsequent Get.
func TestEngineClassReconcile_AddsFinalizerOnFirstReconcile(t *testing.T) {
	sch := engineClassTestScheme(t)
	class := newClassFixture("fresh")
	class.Finalizers = nil // drop the fixture's pre-stamped finalizer
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(class).
		WithStatusSubresource(&computev1alpha1.EngineClass{}).
		Build()

	r := &EngineClassReconciler{Client: cli, Scheme: sch}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "fresh", Namespace: "firebolt"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !res.Requeue {
		t.Error("Requeue = false, want true after finalizer add")
	}

	updated := &computev1alpha1.EngineClass{}
	if err := cli.Get(context.Background(), client.ObjectKey{Name: "fresh", Namespace: "firebolt"}, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !containsFinalizer(updated.Finalizers, engineClassFinalizerName) {
		t.Errorf("Finalizers = %v, want %q included", updated.Finalizers, engineClassFinalizerName)
	}
	// Status untouched on the finalizer-add pass.
	if updated.Status.BoundEngines != 0 {
		t.Errorf("BoundEngines = %d, want 0 (no status work until next reconcile)", updated.Status.BoundEngines)
	}
}

// TestEngineClassReconcile_DeletionBlockedWhileBound pins the data-
// integrity guard: a class with a deletionTimestamp and at least one
// bound engine must keep its finalizer and surface
// Ready=False/DeletionBlocked with the count.
func TestEngineClassReconcile_DeletionBlockedWhileBound(t *testing.T) {
	sch := engineClassTestScheme(t)
	class := newClassFixture("doomed")
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(
			class,
			newEngineFixture("a", "firebolt", "doomed"),
			newEngineFixture("b", "firebolt", "doomed"),
		).
		WithStatusSubresource(&computev1alpha1.EngineClass{}).
		Build()

	// Trigger deletion through the client so DeletionTimestamp is set
	// the same way Kubernetes would; the finalizer prevents reaping.
	if err := cli.Delete(context.Background(), class); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	r := &EngineClassReconciler{Client: cli, Scheme: sch}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "doomed", Namespace: "firebolt"}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("RequeueAfter = 0, want %s (deletion held)", engineClassRequeueAfter)
	}

	updated := &computev1alpha1.EngineClass{}
	if err := cli.Get(context.Background(), client.ObjectKey{Name: "doomed", Namespace: "firebolt"}, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !containsFinalizer(updated.Finalizers, engineClassFinalizerName) {
		t.Errorf("Finalizers = %v, want %q still present (deletion must stay blocked)", updated.Finalizers, engineClassFinalizerName)
	}
	if updated.DeletionTimestamp.IsZero() {
		t.Error("DeletionTimestamp = zero, want non-zero (class should still be Terminating)")
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.EngineClassConditionReady)
	if cond == nil {
		t.Fatal("Ready condition missing")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Ready.Status = %s, want False", cond.Status)
	}
	if cond.Reason != reasonDeletionBlocked {
		t.Errorf("Ready.Reason = %q, want %q", cond.Reason, reasonDeletionBlocked)
	}
	if updated.Status.BoundEngines != 2 {
		t.Errorf("BoundEngines = %d, want 2", updated.Status.BoundEngines)
	}
}

// TestEngineClassReconcile_DeletionAllowedWhenUnbound pins the release
// path: a class being deleted with no bound engines has its finalizer
// removed, after which the API server would complete the delete.
func TestEngineClassReconcile_DeletionAllowedWhenUnbound(t *testing.T) {
	sch := engineClassTestScheme(t)
	class := newClassFixture("orphan")
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(class).
		WithStatusSubresource(&computev1alpha1.EngineClass{}).
		Build()

	if err := cli.Delete(context.Background(), class); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	r := &EngineClassReconciler{Client: cli, Scheme: sch}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "orphan", Namespace: "firebolt"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Once the finalizer is removed and no other finalizer remains, the
	// fake client (like real Kubernetes) completes the delete and the
	// next Get returns NotFound.
	updated := &computev1alpha1.EngineClass{}
	err := cli.Get(context.Background(), client.ObjectKey{Name: "orphan", Namespace: "firebolt"}, updated)
	if err == nil {
		t.Fatalf("Get: expected NotFound after finalizer removal, got class with finalizers=%v", updated.Finalizers)
	}
	if !errors.IsNotFound(err) {
		t.Fatalf("Get: expected NotFound, got %v", err)
	}
}

// TestEngineClassReconcile_DeletionLifecycle covers the end-to-end
// transition the guard exists for: a delete arrives while an engine is
// bound (deletion is held), the engine is then removed, and the next
// reconcile releases the finalizer so the class can finish deleting.
func TestEngineClassReconcile_DeletionLifecycle(t *testing.T) {
	sch := engineClassTestScheme(t)
	class := newClassFixture("lifecycle")
	engine := newEngineFixture("only-binder", "firebolt", "lifecycle")
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(class, engine).
		WithStatusSubresource(&computev1alpha1.EngineClass{}).
		Build()

	if err := cli.Delete(context.Background(), class); err != nil {
		t.Fatalf("initial Delete: %v", err)
	}

	r := &EngineClassReconciler{Client: cli, Scheme: sch}
	key := client.ObjectKey{Name: "lifecycle", Namespace: "firebolt"}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("first Reconcile (blocked): %v", err)
	}

	blocked := &computev1alpha1.EngineClass{}
	if err := cli.Get(context.Background(), key, blocked); err != nil {
		t.Fatalf("Get blocked: %v", err)
	}
	if !containsFinalizer(blocked.Finalizers, engineClassFinalizerName) {
		t.Fatalf("Finalizers = %v, want %q present while engine still binds", blocked.Finalizers, engineClassFinalizerName)
	}

	// Remove the bound engine, then reconcile again — the guard releases.
	if err := cli.Delete(context.Background(), engine); err != nil {
		t.Fatalf("Delete engine: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("second Reconcile (release): %v", err)
	}

	released := &computev1alpha1.EngineClass{}
	err := cli.Get(context.Background(), key, released)
	if err == nil {
		t.Fatalf("Get released: expected NotFound after release, got class with finalizers=%v", released.Finalizers)
	}
	if !errors.IsNotFound(err) {
		t.Fatalf("Get released: expected NotFound, got %v", err)
	}
}

// containsFinalizer is a small slice search helper used by the
// deletion-guard tests; kept local to avoid an extra import shared with
// helper packages.
func containsFinalizer(list []string, want string) bool {
	for _, f := range list {
		if f == want {
			return true
		}
	}
	return false
}

func TestEnqueueClassFromEngine_RoutesByRef(t *testing.T) {
	type wantRef struct {
		name      string
		namespace string
	}
	cases := []struct {
		name string
		eng  *computev1alpha1.FireboltEngine
		want []wantRef
	}{
		{
			name: "engine with ref enqueues that class in the engine's namespace",
			eng:  newEngineFixture("e", "ns-a", "compute-optimized"),
			want: []wantRef{{name: "compute-optimized", namespace: "ns-a"}},
		},
		{
			name: "engine without ref enqueues nothing",
			eng:  newEngineFixture("e", "ns-a", ""),
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := enqueueClassFromEngine(context.Background(), tc.eng)
			if len(got) != len(tc.want) {
				t.Fatalf("len(got) = %d, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i].Name != tc.want[i].name {
					t.Errorf("got[%d].Name = %q, want %q", i, got[i].Name, tc.want[i].name)
				}
				if got[i].Namespace != tc.want[i].namespace {
					t.Errorf("got[%d].Namespace = %q, want %q", i, got[i].Namespace, tc.want[i].namespace)
				}
			}
		})
	}
}
