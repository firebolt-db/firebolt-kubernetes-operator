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
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	enginemetrics "github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
)

// resourceBoundsTestScheme returns a runtime.Scheme wired with the
// kinds the bounds tests need: compute/v1alpha1 for the CRs and
// apps/v1 so the "no STS rendered" assertion can List StatefulSets.
func resourceBoundsTestScheme(t *testing.T) *runtime.Scheme {
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

// boundsTestEngine returns an engine primed past the early-return
// branches (finalizer added, phase set, instanceRef pointing at a
// fixture instance) so Reconcile reaches the bounds gate. The
// resources block is supplied by the caller; tests vary it across
// over-bound, within-bound, and empty cases.
func boundsTestEngine(name, ns, instanceRef string, resources corev1.ResourceRequirements) *computev1alpha1.FireboltEngine {
	return &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  ns,
			Finalizers: []string{finalizerName},
			Generation: 1,
		},
		Spec: computev1alpha1.FireboltEngineSpec{
			InstanceRef: instanceRef,
			Replicas:    1,
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:      computev1alpha1.EngineContainerName,
						Resources: resources,
					}},
				},
			},
		},
		Status: computev1alpha1.FireboltEngineStatus{
			Phase: computev1alpha1.PhaseCreating,
		},
	}
}

// boundsTestInstance returns a FireboltInstance whose status has the
// fields resolveInstanceInfo requires (MetadataEndpoint non-empty,
// Spec.ID non-empty) so the engine reconciler's instance gate
// passes before the bounds gate fires.
func boundsTestInstance(name, ns string) *computev1alpha1.FireboltInstance {
	return &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       computev1alpha1.FireboltInstanceSpec{ID: "01H000000000000000000DUMMY"},
		Status: computev1alpha1.FireboltInstanceStatus{
			MetadataEndpoint: "metadata." + ns + ".svc.cluster.local:50051",
		},
	}
}

// TestEngineReconcile_ResourceBoundsExceeded pins the gate: with a
// bound configured and a spec.resources.limit above it, Reconcile
// must surface ConditionReady=False/ResourceBoundsExceeded carrying
// the field path and configured maximum, persist the status, and
// render no StatefulSet. Matches the field-path/message shape the
// webhook produces at admission time so users see the same diagnostic
// regardless of which path caught the violation.
func TestEngineReconcile_ResourceBoundsExceeded(t *testing.T) {
	sch := resourceBoundsTestScheme(t)
	const (
		ns       = "ns-a"
		instName = "parent"
		engName  = "overbound"
	)

	instance := boundsTestInstance(instName, ns)
	engine := boundsTestEngine(engName, ns, instName, corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("64"),
		},
	})
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(instance, engine).
		WithStatusSubresource(&computev1alpha1.FireboltEngine{}, &computev1alpha1.FireboltInstance{}).
		Build()

	r := &FireboltEngineReconciler{
		Client:          cli,
		Scheme:          sch,
		MetricsRecorder: enginemetrics.NoOpEngineRecorder{},
		ResourceBounds: computev1alpha1.EngineResourceBounds{
			MaxCPU: resource.MustParse("32"),
		},
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: engName, Namespace: ns},
	}); err != nil {
		t.Fatalf("Reconcile: unexpected error (gate should set status and requeue, not return err): %v", err)
	}

	updated := &computev1alpha1.FireboltEngine{}
	if err := cli.Get(context.Background(), types.NamespacedName{Name: engName, Namespace: ns}, updated); err != nil {
		t.Fatalf("Get engine: %v", err)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.ConditionReady)
	if cond == nil {
		t.Fatal("Ready condition missing")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Ready.Status = %s, want False", cond.Status)
	}
	if cond.Reason != reasonResourceBoundsExceeded {
		t.Errorf("Ready.Reason = %q, want %q", cond.Reason, reasonResourceBoundsExceeded)
	}
	if !strings.Contains(cond.Message, "spec.template.spec.containers") || !strings.Contains(cond.Message, "resources.limits") {
		t.Errorf("Ready.Message = %q, want it to name the template-container resources.limits path", cond.Message)
	}
	if !strings.Contains(cond.Message, "32") {
		t.Errorf("Ready.Message = %q, want it to name the configured maximum (32)", cond.Message)
	}

	var stsList appsv1.StatefulSetList
	if err := cli.List(context.Background(), &stsList, client.InNamespace(ns)); err != nil {
		t.Fatalf("List StatefulSets: %v", err)
	}
	if len(stsList.Items) > 0 {
		names := make([]string, 0, len(stsList.Items))
		for i := range stsList.Items {
			names = append(names, stsList.Items[i].Name)
		}
		t.Errorf("StatefulSets = %v, want none (bounds gate must short-circuit before applyEngineState)", names)
	}
}

// TestEngineReconcile_ResourceBoundsWithinLimitsPasses is the false-
// positive guard: spec.resources at the configured maximum (Cmp == 0)
// must NOT trigger ResourceBoundsExceeded. The webhook tests already
// pin this for admission; the controller's defense-in-depth must agree.
// We don't assert on the downstream reconcile outcome (the engine has
// no real cluster behind it), only that the bounds gate did not fire.
func TestEngineReconcile_ResourceBoundsWithinLimitsPasses(t *testing.T) {
	sch := resourceBoundsTestScheme(t)
	const (
		ns       = "ns-a"
		instName = "parent"
		engName  = "withinbound"
	)

	instance := boundsTestInstance(instName, ns)
	engine := boundsTestEngine(engName, ns, instName, corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("32"),
			corev1.ResourceMemory: resource.MustParse("256Gi"),
		},
	})
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(instance, engine).
		WithStatusSubresource(&computev1alpha1.FireboltEngine{}, &computev1alpha1.FireboltInstance{}).
		Build()

	r := &FireboltEngineReconciler{
		Client:          cli,
		Scheme:          sch,
		MetricsRecorder: enginemetrics.NoOpEngineRecorder{},
		ResourceBounds: computev1alpha1.EngineResourceBounds{
			MaxCPU:    resource.MustParse("32"),
			MaxMemory: resource.MustParse("256Gi"),
		},
	}
	// Downstream apply may fail because no real cluster — we only assert
	// the bounds gate did not flip the condition.
	_, _ = r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: engName, Namespace: ns},
	})

	updated := &computev1alpha1.FireboltEngine{}
	if err := cli.Get(context.Background(), types.NamespacedName{Name: engName, Namespace: ns}, updated); err != nil {
		t.Fatalf("Get engine: %v", err)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.ConditionReady)
	if cond != nil && cond.Reason == reasonResourceBoundsExceeded {
		t.Errorf("Ready.Reason = %q for spec at the bound, want anything other than %q",
			cond.Reason, reasonResourceBoundsExceeded)
	}
}

// TestEngineReconcile_EmptyResourceBoundsIsNoOp confirms the default
// (empty) bound matrix admits arbitrarily large spec.resources values.
// Pins the IsEmpty short-circuit behind ResourceBounds.Validate: when
// the platform team has not opted into bounds via --engine-max-*, the
// gate must not turn into an accidental "must declare resources"
// guard.
func TestEngineReconcile_EmptyResourceBoundsIsNoOp(t *testing.T) {
	sch := resourceBoundsTestScheme(t)
	const (
		ns       = "ns-a"
		instName = "parent"
		engName  = "unbounded"
	)

	instance := boundsTestInstance(instName, ns)
	engine := boundsTestEngine(engName, ns, instName, corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("999"),
			corev1.ResourceMemory: resource.MustParse("999Ti"),
		},
	})
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(instance, engine).
		WithStatusSubresource(&computev1alpha1.FireboltEngine{}, &computev1alpha1.FireboltInstance{}).
		Build()

	// ResourceBounds left zero-valued (IsEmpty()==true).
	r := &FireboltEngineReconciler{
		Client:          cli,
		Scheme:          sch,
		MetricsRecorder: enginemetrics.NoOpEngineRecorder{},
	}
	_, _ = r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: engName, Namespace: ns},
	})

	updated := &computev1alpha1.FireboltEngine{}
	if err := cli.Get(context.Background(), types.NamespacedName{Name: engName, Namespace: ns}, updated); err != nil {
		t.Fatalf("Get engine: %v", err)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.ConditionReady)
	if cond != nil && cond.Reason == reasonResourceBoundsExceeded {
		t.Errorf("Ready.Reason = %q with empty bounds, want gate to be a no-op", cond.Reason)
	}
}
