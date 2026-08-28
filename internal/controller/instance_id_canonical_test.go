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

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	"github.com/oklog/ulid/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/config/images"
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

	computev1alpha1.CanonicalInstanceIDImageFloor = ""
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
	// "latest" is not published for engine or metadata, and an untagged
	// reference defaults to it — both must fail closed, or an arbitrarily
	// old image clears the gate.
	if imageMeetsCanonicalFloor("oci.example/engine:latest") {
		t.Error("latest tag must not meet the floor")
	}
	if imageMeetsCanonicalFloor("oci.example/engine") {
		t.Error("untagged image must not meet the floor")
	}
	// A digest pin carries no tag to compare against the floor.
	if imageMeetsCanonicalFloor("oci.example/engine@sha256:" + strings.Repeat("a", 64)) {
		t.Error("digest-pinned image must not meet the floor")
	}
}

// TestDefaultImagesMeetCanonicalFloor keeps the shipped image defaults
// and the canonicalize floor from drifting apart. MintInstanceID emits
// lowercase ids as soon as the floor is set, so a build whose own
// default engine or metadata image sits below that floor would hand a
// fresh instance an account ID its images cannot read — and would
// refuse to canonicalize it afterwards, because the gate compares
// against those same images. Holds for both variants: the "latest"
// defaults pin the floor tag itself, and the "dev" aliases track the
// dev-branch build of it.
func TestDefaultImagesMeetCanonicalFloor(t *testing.T) {
	if computev1alpha1.CanonicalInstanceIDImageFloor == "" {
		t.Skip("canonicalize floor is unpublished; default images are not gated on it")
	}
	for _, image := range []string{images.DefaultEngine(), images.DefaultMetadata()} {
		if !imageMeetsCanonicalFloor(image) {
			t.Errorf("default %s image %q is below the canonicalize floor %q; bump the %s variant defaults and the floor together",
				images.Variant(), image, computev1alpha1.CanonicalInstanceIDImageFloor, images.Variant())
		}
	}
}

func TestBelowFloorMessageNamesDigestPin(t *testing.T) {
	orig := computev1alpha1.CanonicalInstanceIDImageFloor
	computev1alpha1.CanonicalInstanceIDImageFloor = "release-5.1.0-pre.0.20260828000000.deadbeef"
	t.Cleanup(func() { computev1alpha1.CanonicalInstanceIDImageFloor = orig })

	digest := "oci.example/engine@sha256:" + strings.Repeat("a", 64)
	if msg := belowFloorMessage("metadata", digest); !strings.Contains(msg, "pinned by digest") {
		t.Errorf("digest-pin message = %q, want it to name the digest pin", msg)
	}
	if msg := belowFloorMessage("metadata", "oci.example/engine:release-1.0.0"); strings.Contains(msg, "pinned by digest") {
		t.Errorf("tagged message = %q, want the bump-the-tag wording", msg)
	}
}

// TestIsUppercaseCrockfordULID_RejectsNonCrockford pins the ParseStrict
// requirement: ulid.Parse checks only the 26-byte length, and spec.id has
// no CRD pattern, so a same-length user-supplied id must not be treated
// as a ULID and lowercased.
func TestIsUppercaseCrockfordULID_RejectsNonCrockford(t *testing.T) {
	for _, id := range []string{
		"0Customer-Account-ID-12345", // 26 chars, not base32
		"1234567890ABCDEFGHIJKLMNOP", // 26 chars, contains I, L, O, U
	} {
		if len(id) != 26 {
			t.Fatalf("fixture %q is %d chars, want 26 to exercise the length path", id, len(id))
		}
		if isUppercaseCrockfordULID(id) {
			t.Errorf("isUppercaseCrockfordULID(%q) = true, want false", id)
		}
	}
	if !isUppercaseCrockfordULID(testUppercaseULID) {
		t.Errorf("isUppercaseCrockfordULID(%q) = false, want true", testUppercaseULID)
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

// rejectIDRewrite fails any FireboltInstance Update that changes spec.id away
// from want, standing in for a stale CRD whose CEL rule still forbids
// case-only updates or a policy engine that refuses spec mutations. Updates
// that leave spec.id alone (finalizer add/remove) pass through.
func rejectIDRewrite(want string) interceptor.Funcs {
	return interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if inst, ok := obj.(*computev1alpha1.FireboltInstance); ok && inst.Spec.ID != want {
				return apierrors.NewInvalid(
					schema.GroupKind{Group: computev1alpha1.GroupVersion.Group, Kind: "FireboltInstance"},
					inst.Name,
					field.ErrorList{field.Invalid(
						field.NewPath("spec", "id"), inst.Spec.ID,
						"spec.id is immutable once set")},
				)
			}
			return c.Update(ctx, obj, opts...)
		},
	}
}

func TestInstanceReconcile_SurfacesConditionWhenIDUpdateRejected(t *testing.T) {
	orig := computev1alpha1.CanonicalInstanceIDImageFloor
	computev1alpha1.CanonicalInstanceIDImageFloor = DefaultEngineTag
	t.Cleanup(func() { computev1alpha1.CanonicalInstanceIDImageFloor = orig })

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
		WithInterceptorFuncs(rejectIDRewrite(testUppercaseULID)).
		Build()
	r := &FireboltInstanceReconciler{
		Client:          cli,
		Scheme:          sch,
		MetricsRecorder: fireboltmetrics.NoOpInstanceRecorder{},
	}
	key := client.ObjectKey{Name: inst.Name, Namespace: inst.Namespace}
	// A rejected canonicalize must not abort the pass: returning the error
	// would stall metadata and gateway on an otherwise healthy instance.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	updated := &computev1alpha1.FireboltInstance{}
	if err := cli.Get(context.Background(), key, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.Spec.ID != testUppercaseULID {
		t.Errorf("spec.id = %q, want unchanged %q", updated.Spec.ID, testUppercaseULID)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.InstanceConditionInstanceIDCanonical)
	if cond == nil {
		t.Fatal("InstanceIDCanonical condition missing")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("InstanceIDCanonical.Status = %s, want False", cond.Status)
	}
	if cond.Reason != reasonInstanceIDRejected {
		t.Errorf("InstanceIDCanonical.Reason = %q, want %q", cond.Reason, reasonInstanceIDRejected)
	}
}

func TestInstanceReconcile_DeletesWithoutCanonicalizingID(t *testing.T) {
	orig := computev1alpha1.CanonicalInstanceIDImageFloor
	computev1alpha1.CanonicalInstanceIDImageFloor = DefaultEngineTag
	t.Cleanup(func() { computev1alpha1.CanonicalInstanceIDImageFloor = orig })

	sch := instanceTemplateTestScheme(t)
	// reconcileDelete sweeps Certificates; without the kind registered the
	// fake client fails the sweep for a reason unrelated to this test.
	if err := certmanagerv1.AddToScheme(sch); err != nil {
		t.Fatalf("certmanagerv1.AddToScheme: %v", err)
	}
	now := metav1.Now()
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "fi",
			Namespace:         "default",
			Finalizers:        []string{instanceFinalizerName},
			DeletionTimestamp: &now,
			Generation:        1,
		},
		Spec: computev1alpha1.FireboltInstanceSpec{ID: testUppercaseULID},
	}
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(inst).
		WithStatusSubresource(&computev1alpha1.FireboltInstance{}).
		WithInterceptorFuncs(rejectIDRewrite(testUppercaseULID)).
		Build()
	r := &FireboltInstanceReconciler{
		Client:          cli,
		Scheme:          sch,
		MetricsRecorder: fireboltmetrics.NoOpInstanceRecorder{},
	}
	key := client.ObjectKey{Name: inst.Name, Namespace: inst.Namespace}
	// Teardown must not depend on the id rewrite succeeding — reconcileDelete
	// sweeps by the LabelInstance label, never by spec.id.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if err := cli.Get(context.Background(), key, &computev1alpha1.FireboltInstance{}); !apierrors.IsNotFound(err) {
		t.Errorf("Get after delete = %v, want NotFound (finalizer should have been removed)", err)
	}
}

func TestInstanceReconcile_DoesNotRewriteSameLengthCustomID(t *testing.T) {
	orig := computev1alpha1.CanonicalInstanceIDImageFloor
	computev1alpha1.CanonicalInstanceIDImageFloor = DefaultEngineTag
	t.Cleanup(func() { computev1alpha1.CanonicalInstanceIDImageFloor = orig })

	// Exactly 26 characters, so only ParseStrict's character validation
	// keeps it out of the canonicalize path.
	const customID = "0Customer-Account-ID-12345"

	sch := instanceTemplateTestScheme(t)
	inst := readyInstanceWithTemplates()
	inst.Spec.ID = customID
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
	key := client.ObjectKey{Name: inst.Name, Namespace: inst.Namespace}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	updated := &computev1alpha1.FireboltInstance{}
	if err := cli.Get(context.Background(), key, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.Spec.ID != customID {
		t.Errorf("spec.id = %q, want custom id left unchanged", updated.Spec.ID)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.InstanceConditionInstanceIDCanonical)
	if cond == nil {
		t.Fatal("InstanceIDCanonical condition missing")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("InstanceIDCanonical.Status = %s, want True for a non-ULID id", cond.Status)
	}
}

// TestInstanceReconcile_ClearsCanonicalConditionWhenFloorEmpty covers the
// operator rollback: a build with no floor compiled in must not leave the
// previous build's ImageBelowFloor standing forever.
func TestInstanceReconcile_ClearsCanonicalConditionWhenFloorEmpty(t *testing.T) {
	orig := computev1alpha1.CanonicalInstanceIDImageFloor
	computev1alpha1.CanonicalInstanceIDImageFloor = ""
	t.Cleanup(func() { computev1alpha1.CanonicalInstanceIDImageFloor = orig })

	sch := instanceTemplateTestScheme(t)
	inst := readyInstanceWithTemplates()
	inst.Spec.ID = testUppercaseULID
	inst.Status.Conditions = append(inst.Status.Conditions, metav1.Condition{
		Type:               computev1alpha1.InstanceConditionInstanceIDCanonical,
		Status:             metav1.ConditionFalse,
		Reason:             reasonInstanceIDBelowFloor,
		Message:            "left behind by a floor-published build",
		LastTransitionTime: metav1.Now(),
	})
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
	key := client.ObjectKey{Name: inst.Name, Namespace: inst.Namespace}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	updated := &computev1alpha1.FireboltInstance{}
	if err := cli.Get(context.Background(), key, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.Spec.ID != testUppercaseULID {
		t.Errorf("spec.id = %q, want unchanged %q", updated.Spec.ID, testUppercaseULID)
	}
	if cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.InstanceConditionInstanceIDCanonical); cond != nil {
		t.Errorf("InstanceIDCanonical = %s/%s, want the stale condition removed with no floor", cond.Status, cond.Reason)
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

// boundEngine is an engine fixture for the canonicalize-gate watch
// tests: namespace ns, bound to instanceRef, optionally through a class.
func boundEngine(name, instanceRef, classRef string) *computev1alpha1.FireboltEngine {
	eng := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       computev1alpha1.FireboltEngineSpec{InstanceRef: instanceRef},
	}
	if classRef != "" {
		eng.Spec.EngineClassRef = ptr(classRef)
	}
	return eng
}

func requestNames(reqs []reconcile.Request) map[string]struct{} {
	got := make(map[string]struct{}, len(reqs))
	for _, req := range reqs {
		got[req.Namespace+"/"+req.Name] = struct{}{}
	}
	return got
}

// TestEnqueueInstancesFromEngineClass pins the class arm of the
// canonicalize-gate watch. A bound engine's resolved image often comes
// from its FireboltEngineClass, and a class bump changes no engine's
// generation, so without this mapping an instance would keep an
// uppercase spec.id until an unrelated Instance event.
func TestEnqueueInstancesFromEngineClass(t *testing.T) {
	sch := instanceTemplateTestScheme(t)
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(
			boundEngine("eng-a", "fi", "big"),
			// Second engine on the same instance and class: one request.
			boundEngine("eng-b", "fi", "big"),
			boundEngine("eng-c", "other", "small"),
			boundEngine("eng-d", "unbound-class", ""),
			// Bound to the class but to no instance: nothing to gate.
			boundEngine("eng-e", "", "big"),
		).
		Build()
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch, MetricsRecorder: fireboltmetrics.NoOpInstanceRecorder{}}

	reqs := r.enqueueInstancesFromEngineClass(context.Background(), &computev1alpha1.FireboltEngineClass{
		ObjectMeta: metav1.ObjectMeta{Name: "big", Namespace: "ns"},
	})
	got := requestNames(reqs)
	if len(reqs) != 1 || len(got) != 1 {
		t.Fatalf("enqueue = %v, want exactly one request for ns/fi", reqs)
	}
	if _, ok := got["ns/fi"]; !ok {
		t.Errorf("enqueue = %v, want ns/fi", reqs)
	}

	if reqs := r.enqueueInstancesFromEngineClass(context.Background(), &computev1alpha1.FireboltEngineClass{
		ObjectMeta: metav1.ObjectMeta{Name: "unreferenced", Namespace: "ns"},
	}); len(reqs) != 0 {
		t.Errorf("enqueue for unreferenced class = %v, want none", reqs)
	}
}

// TestEnqueueInstancesFromEnginePreset pins the preset arm. The Preset
// is ambient — every engine in the namespace overlays it — so a preset
// image bump moves bound engine images without touching any engine
// generation. Only the default-named Preset is resolved by the gate.
func TestEnqueueInstancesFromEnginePreset(t *testing.T) {
	sch := instanceTemplateTestScheme(t)
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(
			boundEngine("eng-a", "fi", "big"),
			boundEngine("eng-b", "fi", ""),
			boundEngine("eng-c", "other", ""),
			boundEngine("eng-d", "", ""),
		).
		Build()
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch, MetricsRecorder: fireboltmetrics.NoOpInstanceRecorder{}}

	reqs := r.enqueueInstancesFromEnginePreset(context.Background(), &computev1alpha1.FireboltEnginePreset{
		ObjectMeta: metav1.ObjectMeta{Name: computev1alpha1.FireboltEnginePresetDefaultName, Namespace: "ns"},
	})
	got := requestNames(reqs)
	if len(reqs) != 2 || len(got) != 2 {
		t.Fatalf("enqueue = %v, want one request each for ns/fi and ns/other", reqs)
	}
	for _, want := range []string{"ns/fi", "ns/other"} {
		if _, ok := got[want]; !ok {
			t.Errorf("enqueue = %v, want %s", reqs, want)
		}
	}

	// A differently named Preset is never resolved, so it cannot change
	// the gate's answer.
	if reqs := r.enqueueInstancesFromEnginePreset(context.Background(), &computev1alpha1.FireboltEnginePreset{
		ObjectMeta: metav1.ObjectMeta{Name: "not-the-default", Namespace: "ns"},
	}); len(reqs) != 0 {
		t.Errorf("enqueue for non-default preset = %v, want none", reqs)
	}
}
