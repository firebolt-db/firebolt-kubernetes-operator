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

	"github.com/oklog/ulid/v2"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	fireboltmetrics "github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
)

const testUppercaseULID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

func TestReleaseTagAtLeast(t *testing.T) {
	floor := "release-5.0.0-pre.0.20260822175432.75d37cc26c66"
	tests := []struct {
		tag  string
		want bool
	}{
		{tag: floor, want: true},
		{tag: "release-5.0.0-pre.0.20260822175432.aaaaaaaaaaaa", want: true},
		{tag: "release-5.0.0-pre.0.20260822175433.aaaaaaaaaaaa", want: true},
		{tag: "release-5.0.0-pre.0.20260822175431.aaaaaaaaaaaa", want: false},
		{tag: "release-5.0.0", want: true},
		{tag: "release-5.1.0-pre.0.20260101000000.aaaaaaaaaaaa", want: true},
		{tag: "release-4.32.0-pre.0.20260822175432.aaaaaaaaaaaa", want: false},
		{tag: "debug-5.0.0-pre.0.20260822175432.aaaaaaaaaaaa", want: true},
		{tag: "not-a-release-tag", want: false},
	}
	for _, tc := range tests {
		if got := releaseTagAtLeast(tc.tag, floor); got != tc.want {
			t.Errorf("releaseTagAtLeast(%q, floor) = %v, want %v", tc.tag, got, tc.want)
		}
	}
}

func TestImageMeetsCanonicalFloor(t *testing.T) {
	orig := computev1alpha1.CanonicalInstanceIDImageFloor
	t.Cleanup(func() { computev1alpha1.CanonicalInstanceIDImageFloor = orig })

	if imageMeetsCanonicalFloor("oci.example/engine:" + DefaultEngineTag) {
		t.Fatal("empty floor must not treat any image as meeting it")
	}

	computev1alpha1.CanonicalInstanceIDImageFloor = "release-5.1.0-pre.0.20260828000000.deadbeef"
	if !imageMeetsCanonicalFloor("oci.example/engine:dev") {
		t.Error("dev tag should meet the floor")
	}
	if !imageMeetsCanonicalFloor("oci.example/engine:" + computev1alpha1.CanonicalInstanceIDImageFloor) {
		t.Error("exact floor tag should meet the floor")
	}
	if imageMeetsCanonicalFloor("oci.example/engine:release-5.0.0-pre.0.20260822175432.75d37cc26c66") {
		t.Error("older release tag must not meet the floor")
	}
}

func TestInstanceReconcile_CanonicalizesUppercaseULIDWhenImagesMeetFloor(t *testing.T) {
	orig := computev1alpha1.CanonicalInstanceIDImageFloor
	computev1alpha1.CanonicalInstanceIDImageFloor = DefaultEngineTag
	t.Cleanup(func() { computev1alpha1.CanonicalInstanceIDImageFloor = orig })

	if _, err := ulid.Parse(testUppercaseULID); err != nil {
		t.Fatalf("fixture is not a ULID: %v", err)
	}

	sch := instanceTemplateTestScheme(t)
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "fi",
			Namespace:  "default",
			Finalizers: []string{instanceFinalizerName},
			Generation: 1,
		},
		Spec: computev1alpha1.FireboltInstanceSpec{ID: testUppercaseULID},
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
		t.Error("Requeue = false, want true after spec.id canonicalize Update")
	}

	updated := &computev1alpha1.FireboltInstance{}
	if err := cli.Get(context.Background(), client.ObjectKey{Name: inst.Name, Namespace: inst.Namespace}, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := strings.ToLower(testUppercaseULID)
	if updated.Spec.ID != want {
		t.Errorf("spec.id = %q, want lowercase %q", updated.Spec.ID, want)
	}
}

func TestInstanceReconcile_LeavesUppercaseULIDWhenImageBelowFloor(t *testing.T) {
	orig := computev1alpha1.CanonicalInstanceIDImageFloor
	computev1alpha1.CanonicalInstanceIDImageFloor = "release-9.0.0-pre.0.20990101000000.deadbeef"
	t.Cleanup(func() { computev1alpha1.CanonicalInstanceIDImageFloor = orig })

	sch := instanceTemplateTestScheme(t)
	inst := readyInstanceWithTemplates()
	inst.Spec.ID = testUppercaseULID
	inst.Spec.Metadata.Template.Spec.Containers = []corev1.Container{{
		Name:  computev1alpha1.MetadataContainerName,
		Image: "ghcr.io/firebolt-db/metadata:release-5.0.0-pre.0.20260822175432.75d37cc26c66",
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
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: inst.Name, Namespace: inst.Namespace}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	updated := &computev1alpha1.FireboltInstance{}
	if err := cli.Get(context.Background(), client.ObjectKey{Name: inst.Name, Namespace: inst.Namespace}, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.Spec.ID != testUppercaseULID {
		t.Errorf("spec.id = %q, want unchanged uppercase %q", updated.Spec.ID, testUppercaseULID)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.InstanceConditionInstanceIDCanonical)
	if cond == nil {
		t.Fatal("InstanceIDCanonical condition missing")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("InstanceIDCanonical.Status = %s, want False", cond.Status)
	}
	if cond.Reason != reasonInstanceIDBelowFloor {
		t.Errorf("InstanceIDCanonical.Reason = %q, want %q", cond.Reason, reasonInstanceIDBelowFloor)
	}
}

func TestInstanceReconcile_LeavesUppercaseULIDWhenBoundEngineBelowFloor(t *testing.T) {
	orig := computev1alpha1.CanonicalInstanceIDImageFloor
	computev1alpha1.CanonicalInstanceIDImageFloor = DefaultEngineTag
	t.Cleanup(func() { computev1alpha1.CanonicalInstanceIDImageFloor = orig })

	sch := instanceTemplateTestScheme(t)
	inst := readyInstanceWithTemplates()
	inst.Spec.ID = testUppercaseULID
	eng := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: "eng", Namespace: "default"},
		Spec: computev1alpha1.FireboltEngineSpec{
			InstanceRef: inst.Name,
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  computev1alpha1.EngineContainerName,
						Image: "oci.firebolt.io/firebolt-db/engine:release-4.0.0-pre.0.20260101000000.aaaaaaaaaaaa",
					}},
				},
			},
		},
	}
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(inst, eng).
		WithStatusSubresource(&computev1alpha1.FireboltInstance{}).
		Build()
	r := &FireboltInstanceReconciler{
		Client:          cli,
		Scheme:          sch,
		MetricsRecorder: fireboltmetrics.NoOpInstanceRecorder{},
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: inst.Name, Namespace: inst.Namespace}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	updated := &computev1alpha1.FireboltInstance{}
	if err := cli.Get(context.Background(), client.ObjectKey{Name: inst.Name, Namespace: inst.Namespace}, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.Spec.ID != testUppercaseULID {
		t.Errorf("spec.id = %q, want unchanged uppercase while a bound engine is below the floor", updated.Spec.ID)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.InstanceConditionInstanceIDCanonical)
	if cond == nil || cond.Reason != reasonInstanceIDBelowFloor {
		t.Fatalf("InstanceIDCanonical = %+v, want Reason %q", cond, reasonInstanceIDBelowFloor)
	}
}

func TestInstanceReconcile_ContinuesWhenBoundEngineClassMissing(t *testing.T) {
	orig := computev1alpha1.CanonicalInstanceIDImageFloor
	computev1alpha1.CanonicalInstanceIDImageFloor = DefaultEngineTag
	t.Cleanup(func() { computev1alpha1.CanonicalInstanceIDImageFloor = orig })

	sch := instanceTemplateTestScheme(t)
	inst := readyInstanceWithTemplates()
	inst.Spec.ID = testUppercaseULID
	eng := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: "eng", Namespace: "default"},
		Spec: computev1alpha1.FireboltEngineSpec{
			InstanceRef:    inst.Name,
			EngineClassRef: ptr("missing-class"),
		},
	}
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(inst, eng).
		WithStatusSubresource(&computev1alpha1.FireboltInstance{}).
		Build()
	r := &FireboltInstanceReconciler{
		Client:          cli,
		Scheme:          sch,
		MetricsRecorder: fireboltmetrics.NoOpInstanceRecorder{},
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: inst.Name, Namespace: inst.Namespace}}); err != nil {
		t.Fatalf("Reconcile: %v, want nil so a dangling class ref cannot stall the instance", err)
	}

	updated := &computev1alpha1.FireboltInstance{}
	if err := cli.Get(context.Background(), client.ObjectKey{Name: inst.Name, Namespace: inst.Namespace}, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.Spec.ID != testUppercaseULID {
		t.Errorf("spec.id = %q, want unchanged uppercase while the bound engine class is missing", updated.Spec.ID)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.InstanceConditionInstanceIDCanonical)
	if cond == nil {
		t.Fatal("InstanceIDCanonical condition missing")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("InstanceIDCanonical.Status = %s, want False", cond.Status)
	}
	if cond.Reason != reasonInstanceIDResolveError {
		t.Errorf("InstanceIDCanonical.Reason = %q, want %q", cond.Reason, reasonInstanceIDResolveError)
	}
}

func TestInstanceReconcile_DoesNotRewriteCustomID(t *testing.T) {
	orig := computev1alpha1.CanonicalInstanceIDImageFloor
	computev1alpha1.CanonicalInstanceIDImageFloor = DefaultEngineTag
	t.Cleanup(func() { computev1alpha1.CanonicalInstanceIDImageFloor = orig })

	sch := instanceTemplateTestScheme(t)
	inst := readyInstanceWithTemplates()
	inst.Spec.ID = "MY-CUSTOM-ID"
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
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: inst.Name, Namespace: inst.Namespace}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	updated := &computev1alpha1.FireboltInstance{}
	if err := cli.Get(context.Background(), client.ObjectKey{Name: inst.Name, Namespace: inst.Namespace}, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.Spec.ID != "MY-CUSTOM-ID" {
		t.Errorf("spec.id = %q, want custom id left unchanged", updated.Spec.ID)
	}
}

func TestEnqueueInstanceFromEngine(t *testing.T) {
	reqs := enqueueInstanceFromEngine(context.Background(), &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: "eng", Namespace: "ns"},
		Spec:       computev1alpha1.FireboltEngineSpec{InstanceRef: "fi"},
	})
	if len(reqs) != 1 || reqs[0].Name != "fi" || reqs[0].Namespace != "ns" {
		t.Errorf("enqueue = %v, want instance fi/ns", reqs)
	}
}
