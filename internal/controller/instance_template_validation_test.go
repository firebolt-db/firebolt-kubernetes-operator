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
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	fireboltmetrics "github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
)

// instanceTemplateTestScheme returns a runtime.Scheme wired with the
// kinds the validation tests need to write through a fake client:
// core/v1 for Secret/Service/Deployment lookups the rejection path
// must skip, apps/v1 because the gateway/metadata renderers create
// Deployments, and compute/v1alpha1 for FireboltInstance itself.
func instanceTemplateTestScheme(t *testing.T) *runtime.Scheme {
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

// readyInstanceWithTemplates returns a FireboltInstance primed past the
// finalizer-add / ULID-fallback / phase-init early returns, so the next
// Reconcile call lands on the template-validation step. Tests mutate
// the returned object's gateway / metadata templates before priming the
// fake client. Lives in namespace "default".
func readyInstanceWithTemplates() *computev1alpha1.FireboltInstance {
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "fi",
			Namespace:  "default",
			Generation: 1,
			Finalizers: []string{instanceFinalizerName},
		},
		Spec: computev1alpha1.FireboltInstanceSpec{
			ID: "01H000000000000000000DUMMY",
			Metadata: computev1alpha1.MetadataSpec{
				Template: &corev1.PodTemplateSpec{},
			},
			Gateway: computev1alpha1.GatewaySpec{
				Template: &corev1.PodTemplateSpec{},
			},
		},
		Status: computev1alpha1.FireboltInstanceStatus{
			Phase: computev1alpha1.InstancePhaseProvisioning,
		},
	}
	return inst
}

// TestInstanceReconcile_GatewayTemplateRejected pins the gateway
// defense-in-depth branch: a forbidden field on
// spec.gateway.template surfaces as
// GatewayReady=False/TemplateRejected with the field path, returns an
// error so controller-runtime backs off, and no gateway resources are
// rendered. The metadata template here is valid, so this also
// asserts the rejection is component-scoped.
func TestInstanceReconcile_GatewayTemplateRejected(t *testing.T) {
	sch := instanceTemplateTestScheme(t)
	inst := readyInstanceWithTemplates()
	// preStop on the envoy container is operator-owned (it drives the
	// drain hook the zero-downtime contract relies on). The webhook
	// rejects this at apply time; the reconciler must too.
	inst.Spec.Gateway.Template.Spec.Containers = []corev1.Container{{
		Name:      computev1alpha1.GatewayContainerName,
		Lifecycle: &corev1.Lifecycle{},
	}}
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(inst).
		WithStatusSubresource(&computev1alpha1.FireboltInstance{}).
		Build()

	r := &FireboltInstanceReconciler{
		Client:          cli,
		Scheme:          sch,
		MetricsRecorder: fireboltmetrics.NoOpInstanceRecorder{},
	}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: inst.Name, Namespace: inst.Namespace}})
	if err == nil {
		t.Fatal("Reconcile: expected error for rejected gateway template, got nil")
	}
	if !strings.Contains(err.Error(), "spec.gateway.template.spec.containers[0].lifecycle") {
		t.Errorf("Reconcile: error %q does not surface the offending field path", err.Error())
	}

	updated := &computev1alpha1.FireboltInstance{}
	if err := cli.Get(context.Background(), client.ObjectKey{Name: inst.Name, Namespace: inst.Namespace}, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.InstanceConditionGatewayReady)
	if cond == nil {
		t.Fatal("GatewayReady condition missing")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("GatewayReady.Status = %s, want False", cond.Status)
	}
	if cond.Reason != reasonTemplateRejected {
		t.Errorf("GatewayReady.Reason = %q, want %q", cond.Reason, reasonTemplateRejected)
	}
	if !strings.Contains(cond.Message, "spec.gateway.template.spec.containers[0].lifecycle") {
		t.Errorf("GatewayReady.Message = %q, want field path in it", cond.Message)
	}
	// MetadataReady must NOT be flipped — the rejection is per-component.
	mdCond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.InstanceConditionMetadataReady)
	if mdCond != nil && mdCond.Status == metav1.ConditionFalse && mdCond.Reason == reasonTemplateRejected {
		t.Error("MetadataReady = False/TemplateRejected, want untouched (gateway-only failure)")
	}
	// No Deployments rendered: rejection runs before any ensure*.
	var deps appsv1.DeploymentList
	if err := cli.List(context.Background(), &deps, client.InNamespace(inst.Namespace)); err != nil {
		t.Fatalf("List Deployments: %v", err)
	}
	if len(deps.Items) > 0 {
		names := make([]string, 0, len(deps.Items))
		for i := range deps.Items {
			names = append(names, deps.Items[i].Name)
		}
		t.Errorf("Deployments = %v, want none (rejection must short-circuit before ensure*)", names)
	}
}

// TestInstanceReconcile_MetadataTemplateRejected mirrors the gateway
// test for the metadata side: an operator-owned env key on the
// metadata container surfaces MetadataReady=False/TemplateRejected
// and short-circuits the rest of the pipeline. The gateway template
// is left valid so the assertion that the failure is component-scoped
// holds.
func TestInstanceReconcile_MetadataTemplateRejected(t *testing.T) {
	sch := instanceTemplateTestScheme(t)
	inst := readyInstanceWithTemplates()
	inst.Spec.Metadata.Template.Spec.Containers = []corev1.Container{{
		Name: computev1alpha1.MetadataContainerName,
		Env: []corev1.EnvVar{
			{Name: computev1alpha1.MetadataPostgresUsernameEnvKey, Value: "/etc/attacker"},
		},
	}}
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(inst).
		WithStatusSubresource(&computev1alpha1.FireboltInstance{}).
		Build()

	r := &FireboltInstanceReconciler{
		Client:          cli,
		Scheme:          sch,
		MetricsRecorder: fireboltmetrics.NoOpInstanceRecorder{},
	}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: inst.Name, Namespace: inst.Namespace}})
	if err == nil {
		t.Fatal("Reconcile: expected error for rejected metadata template, got nil")
	}
	if !strings.Contains(err.Error(), "spec.metadata.template.spec.containers[0].env") {
		t.Errorf("Reconcile: error %q does not surface the offending field path", err.Error())
	}

	updated := &computev1alpha1.FireboltInstance{}
	if err := cli.Get(context.Background(), client.ObjectKey{Name: inst.Name, Namespace: inst.Namespace}, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.InstanceConditionMetadataReady)
	if cond == nil {
		t.Fatal("MetadataReady condition missing")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("MetadataReady.Status = %s, want False", cond.Status)
	}
	if cond.Reason != reasonTemplateRejected {
		t.Errorf("MetadataReady.Reason = %q, want %q", cond.Reason, reasonTemplateRejected)
	}
	gwCond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.InstanceConditionGatewayReady)
	if gwCond != nil && gwCond.Status == metav1.ConditionFalse && gwCond.Reason == reasonTemplateRejected {
		t.Error("GatewayReady = False/TemplateRejected, want untouched (metadata-only failure)")
	}
}

// TestInstanceReconcile_MetadataChecksRunBeforeGateway pins the order:
// when both templates carry forbidden fields, the metadata error
// surfaces first. Matches the existing reconcile pipeline ordering
// (metadata → gateway) so a user who fixes one error at a time
// progresses in the same direction the rendering pass would.
func TestInstanceReconcile_MetadataChecksRunBeforeGateway(t *testing.T) {
	sch := instanceTemplateTestScheme(t)
	inst := readyInstanceWithTemplates()
	inst.Spec.Metadata.Template.Spec.Hostname = "metadata-0"
	inst.Spec.Gateway.Template.Spec.Subdomain = "headless"
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(inst).
		WithStatusSubresource(&computev1alpha1.FireboltInstance{}).
		Build()

	r := &FireboltInstanceReconciler{
		Client:          cli,
		Scheme:          sch,
		MetricsRecorder: fireboltmetrics.NoOpInstanceRecorder{},
	}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: inst.Name, Namespace: inst.Namespace}})
	if err == nil {
		t.Fatal("Reconcile: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "spec.metadata.template") {
		t.Errorf("Reconcile: error %q does not name metadata first", err.Error())
	}
	if strings.Contains(err.Error(), "spec.gateway.template") {
		t.Errorf("Reconcile: error %q mentions gateway; expected metadata-only on first pass", err.Error())
	}
}

// TestInstanceReconcile_ValidTemplatesPass confirms the validator is a
// no-op when both templates carry only user-permitted fields. The test
// stops short of asserting downstream resources because the postgres /
// metadata / gateway ensure paths require their own fixtures; the goal
// here is to lock in the false-positive guard — well-formed templates
// must not trigger TemplateRejected.
func TestInstanceReconcile_ValidTemplatesPass(t *testing.T) {
	sch := instanceTemplateTestScheme(t)
	inst := readyInstanceWithTemplates()
	inst.Spec.Gateway.Template.Spec = corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  computev1alpha1.GatewayContainerName,
			Image: "envoyproxy/envoy:custom",
		}},
	}
	inst.Spec.Metadata.Template.Spec = corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  computev1alpha1.MetadataContainerName,
			Image: "ghcr.io/firebolt-db/metadata:custom",
		}},
	}
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(inst).
		WithStatusSubresource(&computev1alpha1.FireboltInstance{}).
		Build()

	r := &FireboltInstanceReconciler{
		Client:          cli,
		Scheme:          sch,
		MetricsRecorder: fireboltmetrics.NoOpInstanceRecorder{},
	}
	// We don't assert on the (likely-failing) downstream ensure step —
	// only that the failure, if any, is NOT TemplateRejected.
	_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: inst.Name, Namespace: inst.Namespace}})

	updated := &computev1alpha1.FireboltInstance{}
	if err := cli.Get(context.Background(), client.ObjectKey{Name: inst.Name, Namespace: inst.Namespace}, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, condType := range []string{
		computev1alpha1.InstanceConditionGatewayReady,
		computev1alpha1.InstanceConditionMetadataReady,
	} {
		cond := apimeta.FindStatusCondition(updated.Status.Conditions, condType)
		if cond != nil && cond.Reason == reasonTemplateRejected {
			t.Errorf("%s = %s/%s, want anything other than TemplateRejected for valid templates",
				condType, cond.Status, cond.Reason)
		}
	}
}
