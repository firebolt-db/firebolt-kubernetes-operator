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

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	fireboltmetrics "github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
)

// TestInstanceReconcile_GeneratesULIDWhenSpecIDEmpty pins the
// controller-side fallback for the mutating defaulter webhook: when
// admission is bypassed and a FireboltInstance lands with spec.id
// empty, the first reconcile must mint a ULID and Update the CR so
// every consumer of inst.Spec.ID gets a stable identifier from this
// point on. The CRD's CEL transition rule specifically permits the
// one-shot empty-to-ULID write. The controller returns Requeue=true
// after the Update so the rest of the reconcile runs against the
// persisted CR.
func TestInstanceReconcile_GeneratesULIDWhenSpecIDEmpty(t *testing.T) {
	sch := instanceTemplateTestScheme(t)
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "fi",
			Namespace:  "default",
			Finalizers: []string{instanceFinalizerName},
			// Generation tracked so observedGeneration math is sane,
			// but irrelevant to the fallback branch.
			Generation: 1,
		},
		// Spec.ID deliberately empty — this is the branch under test.
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
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: inst.Name, Namespace: inst.Namespace}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !res.Requeue {
		t.Error("Requeue = false, want true after ULID Update")
	}

	updated := &computev1alpha1.FireboltInstance{}
	if err := cli.Get(context.Background(), client.ObjectKey{Name: inst.Name, Namespace: inst.Namespace}, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.Spec.ID == "" {
		t.Fatal("spec.id still empty after Reconcile; controller fallback did not mint a ULID")
	}
	// ULIDs are 26-character Crockford-base32 strings (128 bits). The
	// exact length pins the format the defaulter uses, matching the
	// webhook's TestDefaulter_GeneratesULID assertion.
	if len(updated.Spec.ID) != 26 {
		t.Errorf("spec.id length = %d, want 26 (ULID): %q", len(updated.Spec.ID), updated.Spec.ID)
	}
}

// TestInstanceReconcile_PostgresSecretEmptyNameSurfacesCondition pins
// the controller's defense-in-depth for the validating webhook's
// validateExternalPostgres: when admission is bypassed and a
// FireboltInstance ships with spec.metadata.postgres set but
// credentialsSecretRef.Name empty, the metadata branch must surface
// MetadataReady=False/PostgresSecretPreflightFailed with the
// errPostgresSecretRefEmpty message, refuse to render metadata
// resources, and return the error so controller-runtime backs off
// until the user fixes the spec.
func TestInstanceReconcile_PostgresSecretEmptyNameSurfacesCondition(t *testing.T) {
	sch := instanceTemplateTestScheme(t)
	inst := readyInstanceWithTemplates()
	inst.Spec.Metadata.Postgres = &computev1alpha1.PostgresSpec{
		Host:     "pg.example.com",
		Database: "firebolt",
		// CredentialsSecretRef.Name deliberately empty — the branch
		// under test. The webhook normally rejects this at admission.
		CredentialsSecretRef: corev1.LocalObjectReference{},
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
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: inst.Name, Namespace: inst.Namespace}})
	if err == nil {
		t.Fatal("Reconcile: expected error for empty postgres secret name, got nil")
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
	if cond.Reason != "PostgresSecretPreflightFailed" {
		t.Errorf("MetadataReady.Reason = %q, want PostgresSecretPreflightFailed", cond.Reason)
	}
	// The errPostgresSecretRefEmpty message names the field path so
	// kubectl describe points at exactly what the user needs to fix.
	if !strings.Contains(cond.Message, "credentialsSecretRef.name") {
		t.Errorf("MetadataReady.Message = %q, want it to name the offending field", cond.Message)
	}
	if updated.Status.MetadataReady {
		t.Error("Status.MetadataReady = true, want false (the boolean mirror should follow the condition)")
	}
}
