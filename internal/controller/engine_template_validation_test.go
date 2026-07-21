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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	enginemetrics "github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
)

// templateTestEngine returns an engine primed past the early-return
// branches (finalizer, phase, instanceRef pointing at a fixture
// instance) so Reconcile reaches the template gate, with the supplied
// env set on the engine container of spec.template. Reuses the
// resource-bounds test scheme/instance helpers (same package).
func templateTestEngine(name, ns, instanceRef string, env []corev1.EnvVar) *computev1alpha1.FireboltEngine {
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
						Name: computev1alpha1.EngineContainerName,
						Env:  env,
					}},
				},
			},
		},
		Status: computev1alpha1.FireboltEngineStatus{
			Phase: computev1alpha1.PhaseCreating,
		},
	}
}

// TestEngineReconcile_TemplateRejectedReservedEnvKey is the regression
// guard. An engine that sets one of the operator-injected
// env keys on spec.template.spec.containers[engine].env must surface
// ConditionReady=False/TemplateRejected naming the offending key, and
// render no StatefulSet — even with webhooks off (the chart default).
// Without the controller-side gate the reserved entry would be appended
// after the operator-injected var and the kubelet's last-wins env
// semantics would let the user value override the operator's, handing
// the engine a forged node identity (POD_INDEX) or runtime behavior.
// One subtest per reserved key so a future addition to
// operatorOwnedEngineEnvKeys that the controller forgets to cover trips here.
func TestEngineReconcile_TemplateRejectedReservedEnvKey(t *testing.T) {
	const (
		ns       = "ns-a"
		instName = "parent"
		engName  = "reserved-env"
	)
	for _, key := range []string{
		computev1alpha1.EnginePodIndexEnvKey,
		computev1alpha1.EngineAwsEC2MetadataClientEnabledEnvKey,
	} {
		t.Run(key, func(t *testing.T) {
			sch := resourceBoundsTestScheme(t)
			instance := boundsTestInstance(instName, ns)
			engine := templateTestEngine(engName, ns, instName, []corev1.EnvVar{
				{Name: key, Value: "0"},
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
			if cond.Reason != reasonTemplateRejected {
				t.Errorf("Ready.Reason = %q, want %q", cond.Reason, reasonTemplateRejected)
			}
			if !strings.Contains(cond.Message, key) {
				t.Errorf("Ready.Message = %q, want it to name the reserved key %q", cond.Message, key)
			}
			if !strings.Contains(cond.Message, "spec.template.spec.containers") {
				t.Errorf("Ready.Message = %q, want it to name the template container env path", cond.Message)
			}

			var stsList appsv1.StatefulSetList
			if err := cli.List(context.Background(), &stsList, client.InNamespace(ns)); err != nil {
				t.Fatalf("List StatefulSets: %v", err)
			}
			if len(stsList.Items) > 0 {
				t.Errorf("StatefulSets = %d, want none (template gate must short-circuit before applyEngineState)", len(stsList.Items))
			}
		})
	}
}

// TestEngineReconcile_ValidEngineTemplatePasses is the false-positive
// guard: a template that sets only a non-reserved env var on the engine
// container must NOT trip TemplateRejected. We don't assert the
// downstream reconcile outcome (no real cluster behind the engine),
// only that the template gate let it through.
func TestEngineReconcile_ValidEngineTemplatePasses(t *testing.T) {
	sch := resourceBoundsTestScheme(t)
	const (
		ns       = "ns-a"
		instName = "parent"
		engName  = "valid-template"
	)

	instance := boundsTestInstance(instName, ns)
	engine := templateTestEngine(engName, ns, instName, []corev1.EnvVar{
		{Name: "DATABASE_URL", Value: "postgres://shared"},
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
	}
	// Downstream apply may fail (no real cluster) — we only assert the
	// template gate did not flip the condition to TemplateRejected.
	_, _ = r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: engName, Namespace: ns},
	})

	updated := &computev1alpha1.FireboltEngine{}
	if err := cli.Get(context.Background(), types.NamespacedName{Name: engName, Namespace: ns}, updated); err != nil {
		t.Fatalf("Get engine: %v", err)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.ConditionReady)
	if cond != nil && cond.Reason == reasonTemplateRejected {
		t.Errorf("Ready.Reason = %q for a template with only a non-reserved env var, want gate to pass", cond.Reason)
	}
}
