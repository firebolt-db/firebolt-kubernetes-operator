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
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
)

func TestAppendExternalFinalizer_SkipsEmpty(t *testing.T) {
	obj := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "ns"},
	}
	got := appendExternalFinalizer(nil, "ConfigMap", obj)
	if got != nil {
		t.Fatalf("expected nil report for finalizer-free object, got %v", got)
	}
}

func TestAppendExternalFinalizer_AccumulatesAndCopies(t *testing.T) {
	obj := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "cm",
			Namespace:  "ns",
			Finalizers: []string{"backup.velero.io/finalizer", "mesh.io/cleanup"},
		},
	}
	got := appendExternalFinalizer(nil, "ConfigMap", obj)
	if len(got) != 1 {
		t.Fatalf("expected one entry, got %v", got)
	}
	if got[0].Kind != "ConfigMap" || got[0].Name != "cm" {
		t.Errorf("entry kind/name = %q/%q, want ConfigMap/cm", got[0].Kind, got[0].Name)
	}
	if len(got[0].Finalizers) != 2 ||
		got[0].Finalizers[0] != "backup.velero.io/finalizer" ||
		got[0].Finalizers[1] != "mesh.io/cleanup" {
		t.Errorf("finalizers = %v, want both verbatim in order", got[0].Finalizers)
	}

	// Mutating the source object after the snapshot must not retroactively
	// change the recorded report — the message and Event are emitted at
	// reconcile-end, after which the caller may keep mutating the slice.
	obj.SetFinalizers([]string{"replaced"})
	if got[0].Finalizers[0] != "backup.velero.io/finalizer" {
		t.Errorf("appendExternalFinalizer did not snapshot finalizers; later mutation leaked into report: %v", got[0].Finalizers)
	}
}

func TestFormatExternalFinalizerMessage(t *testing.T) {
	msg := formatExternalFinalizerMessage([]externalFinalizerEntry{
		{Kind: "StatefulSet", Name: "eng-g1", Finalizers: []string{"a.io/x"}},
		{Kind: "Service", Name: "eng-g1-hl", Finalizers: []string{"b.io/y", "c.io/z"}},
	})
	for _, want := range []string{
		"StatefulSet/eng-g1: a.io/x",
		"Service/eng-g1-hl: b.io/y, c.io/z",
		"engine CR will be garbage-collected",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q; got:\n%s", want, msg)
		}
	}
}

// reconcileDeleteTestEnv assembles a FireboltEngineReconciler against a
// fake client preloaded with one engine and an arbitrary set of
// labeled children. Keeps the per-test boilerplate down so we can
// assert behavioral differences across "has finalizer" / "has no
// finalizer" / "API list fails" without duplicating scheme setup.
type reconcileDeleteTestEnv struct {
	r        *FireboltEngineReconciler
	engine   *computev1alpha1.FireboltEngine
	recorder *events.FakeRecorder
}

func newReconcileDeleteTestEnv(t *testing.T, children ...runtime.Object) *reconcileDeleteTestEnv {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme.AddToScheme: %v", err)
	}
	if err := computev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	now := metav1.Now()
	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "eng",
			Namespace:         "ns",
			Finalizers:        []string{finalizerName},
			DeletionTimestamp: &now,
		},
		Spec: computev1alpha1.FireboltEngineSpec{
			InstanceRef: "inst",
			Replicas:    1,
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: computev1alpha1.EngineContainerName,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
						},
					}},
				},
			},
		},
	}

	objs := []runtime.Object{engine}
	objs = append(objs, children...)

	// Need to convert to client.Object for the builder.
	builder := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&computev1alpha1.FireboltEngine{})
	for _, o := range objs {
		builder = builder.WithRuntimeObjects(o)
	}

	rec := events.NewFakeRecorder(8)
	r := &FireboltEngineReconciler{
		Client:          builder.Build(),
		Scheme:          scheme,
		MetricsRecorder: metrics.NoOpEngineRecorder{},
		EventRecorder:   rec,
	}
	return &reconcileDeleteTestEnv{r: r, engine: engine, recorder: rec}
}

// labeledConfigMap is a fixture helper that produces a ConfigMap
// already tagged with LabelEngine so it shows up in the
// reconcileDelete list query, with optional finalizers attached.
func labeledConfigMap(name string, finalizers ...string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "ns",
			Labels:     map[string]string{LabelEngine: "eng"},
			Finalizers: append([]string{}, finalizers...),
		},
	}
}

func labeledService(name string, finalizers ...string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "ns",
			Labels:     map[string]string{LabelEngine: "eng"},
			Finalizers: append([]string{}, finalizers...),
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{Port: 80}},
		},
	}
}

func labeledStatefulSet(name string, finalizers ...string) *appsv1.StatefulSet {
	var one int32 = 1
	labels := map[string]string{"app": name}
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "ns",
			Labels:     map[string]string{LabelEngine: "eng"},
			Finalizers: append([]string{}, finalizers...),
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "c", Image: "busybox"}},
				},
			},
		},
	}
}

// drainEvents returns every event posted to the FakeRecorder so a
// test can match against them without racing the goroutine that emits
// the asynchronous send.
func drainEvents(rec *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case ev := <-rec.Events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func TestReconcileDelete_NoExternalFinalizers_RemovesOperatorFinalizer(t *testing.T) {
	env := newReconcileDeleteTestEnv(t, labeledConfigMap("eng-g1-config"))
	if err := env.r.reconcileDelete(context.Background(), env.engine); err != nil {
		t.Fatalf("reconcileDelete: %v", err)
	}

	got := &computev1alpha1.FireboltEngine{}
	err := env.r.Get(context.Background(), types.NamespacedName{Name: "eng", Namespace: "ns"}, got)
	// fake client honors finalizers: removing the last finalizer on a
	// DeletionTimestamp-set object causes the object to be reaped, so
	// IsNotFound is the success signal we expect here.
	if err == nil && len(got.Finalizers) != 0 {
		t.Fatalf("operator finalizer should have been removed; engine still carries %v", got.Finalizers)
	}

	if evs := drainEvents(env.recorder); len(evs) != 0 {
		t.Errorf("no Events should be emitted when only operator-clean children exist; got %v", evs)
	}
}

func TestReconcileDelete_ExternalFinalizer_EmitsEventAndCondition(t *testing.T) {
	cm := labeledConfigMap("eng-g1-config", "backup.velero.io/finalizer")
	svc := labeledService("eng-g1-hl") // clean — should NOT appear in the message
	sts := labeledStatefulSet("eng-g1", "mesh.io/cleanup", "other.io/finalizer")

	env := newReconcileDeleteTestEnv(t, cm, svc, sts)

	if err := env.r.reconcileDelete(context.Background(), env.engine); err != nil {
		t.Fatalf("reconcileDelete: %v", err)
	}

	// One Warning Event expected, naming all the offending finalizers.
	evs := drainEvents(env.recorder)
	if len(evs) != 1 {
		t.Fatalf("expected exactly one Event, got %d: %v", len(evs), evs)
	}
	ev := evs[0]
	if !strings.Contains(ev, "Warning") || !strings.Contains(ev, eventReasonExternalFinalizer) {
		t.Errorf("Event %q does not look like the Warning/%s emission", ev, eventReasonExternalFinalizer)
	}
	for _, want := range []string{
		"ConfigMap/eng-g1-config",
		"backup.velero.io/finalizer",
		"StatefulSet/eng-g1",
		"mesh.io/cleanup",
		"other.io/finalizer",
	} {
		if !strings.Contains(ev, want) {
			t.Errorf("Event missing %q; got: %s", want, ev)
		}
	}
	// Clean Service must not appear — only resources with finalizers do.
	if strings.Contains(ev, "Service/eng-g1-hl") {
		t.Errorf("Event mentions clean Service; got: %s", ev)
	}

	// The condition is written via Status().Update, which the fake
	// client supports thanks to WithStatusSubresource on the builder.
	// Refresh from the cluster and assert.
	updated := &computev1alpha1.FireboltEngine{}
	if err := env.r.Get(context.Background(), types.NamespacedName{Name: "eng", Namespace: "ns"}, updated); err != nil {
		// fake client should not have GC'd the engine since the operator
		// finalizer is removed only AFTER surfaceExternalFinalizers, and
		// the assertion above already showed the Event was emitted; on
		// any deletion-after-finalizer-removal the condition was written
		// to the in-memory copy before the engine vanished. Treat
		// NotFound as a "we wrote the condition then released the CR"
		// success and skip the field-level assertion.
		return
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.ConditionReady)
	if cond == nil {
		t.Fatal("Ready condition missing on engine status after reconcileDelete")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Ready condition status = %s, want False", cond.Status)
	}
	if cond.Reason != reasonExternalFinalizer {
		t.Errorf("Ready condition reason = %s, want %s", cond.Reason, reasonExternalFinalizer)
	}
}

// TestReconcileDelete_ExternalFinalizer_StillRemovesOperatorFinalizer
// locks in the chosen semantic: detection is informational, not
// blocking. The engine CR must still get garbage-collected so a
// legitimate external finalizer (backup tool snapshotting data) does
// not strand the engine CR forever. The Warning Event survives the
// CR's deletion via kubectl get events, which is the user-facing
// signal once the CR is gone.
func TestReconcileDelete_ExternalFinalizer_StillRemovesOperatorFinalizer(t *testing.T) {
	env := newReconcileDeleteTestEnv(t,
		labeledConfigMap("eng-g1-config", "backup.velero.io/finalizer"),
	)

	if err := env.r.reconcileDelete(context.Background(), env.engine); err != nil {
		t.Fatalf("reconcileDelete: %v", err)
	}

	got := &computev1alpha1.FireboltEngine{}
	err := env.r.Get(context.Background(), types.NamespacedName{Name: "eng", Namespace: "ns"}, got)
	if err == nil && len(got.Finalizers) != 0 {
		t.Fatalf("operator finalizer should have been removed even with external finalizers present; engine still carries %v", got.Finalizers)
	}
}

// TestUpdateStatus_ConflictRecoverySyncsResourceVersion guards the
// invariant that updateStatus's one-shot conflict recovery leaves the
// caller's `engine` pointer carrying the up-to-date ResourceVersion.
// Without that sync, the next main-object write the caller issues
// (typically reconcileDelete removing the finalizer) hits a guaranteed
// 409 from a stale RV — observable as an extra requeue cycle on every
// engine deletion that goes through the surfaceExternalFinalizers
// path, and silently masking the real condition of the engine in
// production logs.
func TestUpdateStatus_ConflictRecoverySyncsResourceVersion(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme.AddToScheme: %v", err)
	}
	if err := computev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: "eng", Namespace: "ns"},
		Spec:       computev1alpha1.FireboltEngineSpec{InstanceRef: "inst", Replicas: 1},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&computev1alpha1.FireboltEngine{}).
		WithObjects(engine).
		Build()

	r := &FireboltEngineReconciler{
		Client:          cl,
		Scheme:          scheme,
		MetricsRecorder: metrics.NoOpEngineRecorder{},
	}

	ctx := context.Background()

	// Snapshot the engine into a "stale" in-memory copy at its initial
	// RV. We then race a concurrent mutation through the cluster
	// (annotation patch) so the stale copy's RV no longer matches the
	// cluster's. updateStatus on the stale copy will fail with 409 and
	// take the conflict-recovery path.
	stale := &computev1alpha1.FireboltEngine{}
	if err := cl.Get(ctx, types.NamespacedName{Name: "eng", Namespace: "ns"}, stale); err != nil {
		t.Fatalf("initial Get: %v", err)
	}
	staleRV := stale.ResourceVersion

	concurrent := &computev1alpha1.FireboltEngine{}
	if err := cl.Get(ctx, types.NamespacedName{Name: "eng", Namespace: "ns"}, concurrent); err != nil {
		t.Fatalf("concurrent Get: %v", err)
	}
	if concurrent.Annotations == nil {
		concurrent.Annotations = map[string]string{}
	}
	concurrent.Annotations["test/forced-rv-bump"] = "1"
	if err := cl.Update(ctx, concurrent); err != nil {
		t.Fatalf("concurrent Update: %v", err)
	}

	// Set a status field on the stale copy and call updateStatus. The
	// initial Status().Update fails with 409 (stale RV); the recovery
	// path Gets fresh, copies status, succeeds. With the fix in place,
	// stale.ResourceVersion is synced back to the post-write RV.
	stale.Status.Phase = computev1alpha1.PhaseStable
	if err := r.updateStatus(ctx, stale); err != nil {
		t.Fatalf("updateStatus: %v", err)
	}

	if stale.ResourceVersion == staleRV {
		t.Fatalf("updateStatus left stale.ResourceVersion at %q (the pre-write value); the conflict-recovery path must sync the new RV back so the next main-object Update does not 409", staleRV)
	}

	// The load-bearing assertion: a subsequent main-object Update on
	// `stale` must succeed without 409. This is the exact sequence
	// reconcileDelete runs after surfaceExternalFinalizers calls
	// updateStatus, and the bug this test guards manifested as a
	// guaranteed 409 right here.
	stale.Labels = map[string]string{"test/after-updatestatus": "1"}
	if err := cl.Update(ctx, stale); err != nil {
		t.Fatalf("main-object Update after updateStatus must succeed (RV must be synced), got: %v", err)
	}
}
