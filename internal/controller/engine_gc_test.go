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
	goerrors "errors"
	"strconv"
	"testing"
	"time"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
)

// gcTestEngineUID is the UID the sweep's ownership check compares against.
const gcTestEngineUID = "11111111-2222-3333-4444-555555555555"

// ownedByEngine stamps the controller reference every ensure* call puts on an
// engine child. The sweep requires it before deleting anything, so a fixture
// without it is a fixture of a resource the operator never created.
func ownedByEngine(engineName string, objs ...client.Object) {
	for _, obj := range objs {
		obj.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: computev1alpha1.GroupVersion.String(),
			Kind:       "FireboltEngine",
			Name:       engineName,
			UID:        gcTestEngineUID,
			Controller: ptr(true),
		}})
	}
}

func TestGCOrphanedResources_DeletesOrphans(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = computev1alpha1.AddToScheme(scheme)

	ns := "test-ns"
	engineName := "my-engine"

	orphanedSTS := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: engineName + "-g1", Namespace: ns,
			Labels: map[string]string{LabelEngine: engineName, LabelGeneration: "1"},
		},
	}
	currentSTS := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: engineName + "-g3", Namespace: ns,
			Labels: map[string]string{LabelEngine: engineName, LabelGeneration: "3"},
		},
	}
	orphanedSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: engineName + "-g1-hl", Namespace: ns,
			Labels: map[string]string{LabelEngine: engineName, LabelGeneration: "1"},
		},
	}
	currentSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: engineName + "-g3-hl", Namespace: ns,
			Labels: map[string]string{LabelEngine: engineName, LabelGeneration: "3"},
		},
	}
	clusterSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: engineName + "-service", Namespace: ns,
			Labels: map[string]string{LabelEngine: engineName},
		},
	}
	orphanedCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: engineName + "-g1-config", Namespace: ns,
			Labels: map[string]string{LabelEngine: engineName, LabelGeneration: "1"},
		},
	}
	currentCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: engineName + "-g3-config", Namespace: ns,
			Labels: map[string]string{LabelEngine: engineName, LabelGeneration: "3"},
		},
	}

	ownedByEngine(engineName, orphanedSTS, currentSTS, orphanedSvc, currentSvc, clusterSvc, orphanedCM, currentCM)

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(orphanedSTS, currentSTS, orphanedSvc, currentSvc, clusterSvc, orphanedCM, currentCM).
		Build()

	r := &FireboltEngineReconciler{Client: fc, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}

	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: engineName, Namespace: ns, UID: gcTestEngineUID},
		Status: computev1alpha1.FireboltEngineStatus{
			CurrentGeneration: 3,
			ActiveGeneration:  3,
		},
	}

	r.gcOrphanedResources(context.Background(), engine)

	// Orphaned resources (gen 1) should be deleted.
	if err := fc.Get(context.Background(), types.NamespacedName{Name: orphanedSTS.Name, Namespace: ns}, &appsv1.StatefulSet{}); err == nil {
		t.Error("orphaned StatefulSet should have been deleted")
	}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: orphanedSvc.Name, Namespace: ns}, &corev1.Service{}); err == nil {
		t.Error("orphaned Service should have been deleted")
	}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: orphanedCM.Name, Namespace: ns}, &corev1.ConfigMap{}); err == nil {
		t.Error("orphaned ConfigMap should have been deleted")
	}

	// Current resources (gen 3) should still exist.
	if err := fc.Get(context.Background(), types.NamespacedName{Name: currentSTS.Name, Namespace: ns}, &appsv1.StatefulSet{}); err != nil {
		t.Errorf("current StatefulSet should not have been deleted: %v", err)
	}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: currentSvc.Name, Namespace: ns}, &corev1.Service{}); err != nil {
		t.Errorf("current Service should not have been deleted: %v", err)
	}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: currentCM.Name, Namespace: ns}, &corev1.ConfigMap{}); err != nil {
		t.Errorf("current ConfigMap should not have been deleted: %v", err)
	}

	// Cluster service (no generation label) should still exist.
	if err := fc.Get(context.Background(), types.NamespacedName{Name: clusterSvc.Name, Namespace: ns}, &corev1.Service{}); err != nil {
		t.Errorf("cluster service should not have been deleted: %v", err)
	}
}

// TestGCOrphanedResources_DeletesOrphanedCertsAndSecrets covers
// per-generation engine TLS Certificates and their cert-manager-derived
// Secrets (both carrying LabelEngine + LabelGeneration) must be swept on the
// same keepGens schedule as STS/Svc/CM — reclaiming abandoned generations while
// preserving the current AND the draining generation (whose pods still mount the
// secret).
func TestGCOrphanedResources_DeletesOrphanedCertsAndSecrets(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = computev1alpha1.AddToScheme(scheme)
	_ = certmanagerv1.AddToScheme(scheme)

	ns := "test-ns"
	engineName := "my-engine"
	mk := func(gen int) (*certmanagerv1.Certificate, *corev1.Secret) {
		name := genResourceName(engineName, gen, SuffixEngineTLS)
		labels := map[string]string{LabelEngine: engineName, LabelGeneration: strconv.Itoa(gen)}
		cert := &certmanagerv1.Certificate{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels}}
		ownedByEngine(engineName, cert)
		// No owner reference on the Secret: cert-manager points it at the
		// Certificate, which is why the sweep admits it on provenance instead.
		return cert,
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels, Annotations: map[string]string{certmanagerv1.CertificateNameKey: name}}}
	}
	c1, s1 := mk(1) // orphaned
	c2, s2 := mk(2) // draining — must be preserved
	c3, s3 := mk(3) // current — must be preserved

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(c1, s1, c2, s2, c3, s3).Build()
	r := &FireboltEngineReconciler{Client: fc, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}

	drain := 2
	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: engineName, Namespace: ns, UID: gcTestEngineUID},
		Status: computev1alpha1.FireboltEngineStatus{
			CurrentGeneration: 3, ActiveGeneration: 3, DrainingGeneration: &drain,
		},
	}
	r.gcOrphanedResources(context.Background(), engine)

	gone := func(name string, obj client.Object) bool {
		return errors.IsNotFound(fc.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, obj))
	}
	if !gone(c1.Name, &certmanagerv1.Certificate{}) {
		t.Error("orphaned Certificate (gen 1) should have been deleted")
	}
	if !gone(s1.Name, &corev1.Secret{}) {
		t.Error("orphaned Secret (gen 1) should have been deleted")
	}
	for _, obj := range []client.Object{c2, c3} {
		if gone(obj.GetName(), &certmanagerv1.Certificate{}) {
			t.Errorf("Certificate %q for a kept generation must be preserved", obj.GetName())
		}
	}
	for _, obj := range []client.Object{s2, s3} {
		if gone(obj.GetName(), &corev1.Secret{}) {
			t.Errorf("Secret %q for a kept generation must be preserved", obj.GetName())
		}
	}
}

func TestGCOrphanedResources_PreservesDrainingGeneration(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = computev1alpha1.AddToScheme(scheme)

	ns := "test-ns"
	engineName := "my-engine"
	drainingGen := 2

	drainingSTS := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: engineName + "-g2", Namespace: ns,
			Labels: map[string]string{LabelEngine: engineName, LabelGeneration: "2"},
		},
	}
	currentSTS := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: engineName + "-g3", Namespace: ns,
			Labels: map[string]string{LabelEngine: engineName, LabelGeneration: "3"},
		},
	}

	ownedByEngine(engineName, drainingSTS, currentSTS)

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(drainingSTS, currentSTS).
		Build()

	r := &FireboltEngineReconciler{Client: fc, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}

	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: engineName, Namespace: ns, UID: gcTestEngineUID},
		Status: computev1alpha1.FireboltEngineStatus{
			CurrentGeneration:  3,
			ActiveGeneration:   3,
			DrainingGeneration: &drainingGen,
		},
	}

	r.gcOrphanedResources(context.Background(), engine)

	// Both draining (gen 2) and current (gen 3) should survive.
	if err := fc.Get(context.Background(), types.NamespacedName{Name: drainingSTS.Name, Namespace: ns}, &appsv1.StatefulSet{}); err != nil {
		t.Errorf("draining StatefulSet should not have been deleted: %v", err)
	}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: currentSTS.Name, Namespace: ns}, &appsv1.StatefulSet{}); err != nil {
		t.Errorf("current StatefulSet should not have been deleted: %v", err)
	}
}

// TestGCOrphanedResources_PreservesUnlabeledResources verifies the GC
// scope invariant: an engine-tagged resource without a LabelGeneration
// is out of scope and must survive the sweep. Without this guard the
// empty-string gen would fail the keepGens lookup and the resource
// would be silently deleted — a strictly larger blast radius than a
// "safety net" should have.
func TestGCOrphanedResources_PreservesUnlabeledResources(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = computev1alpha1.AddToScheme(scheme)

	ns := "test-ns"
	engineName := "my-engine"

	engineLabelsOnly := map[string]string{LabelEngine: engineName}

	unlabeledSTS := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: engineName + "-shared", Namespace: ns,
			Labels: engineLabelsOnly,
		},
	}
	unlabeledCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: engineName + "-shared-config", Namespace: ns,
			Labels: engineLabelsOnly,
		},
	}
	clusterSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: engineName + "-service", Namespace: ns,
			Labels: engineLabelsOnly,
		},
	}
	currentSTS := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: engineName + "-g1", Namespace: ns,
			Labels: map[string]string{LabelEngine: engineName, LabelGeneration: "1"},
		},
	}

	ownedByEngine(engineName, unlabeledSTS, unlabeledCM, clusterSvc, currentSTS)

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(unlabeledSTS, unlabeledCM, clusterSvc, currentSTS).
		Build()

	r := &FireboltEngineReconciler{Client: fc, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}

	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: engineName, Namespace: ns, UID: gcTestEngineUID},
		Status: computev1alpha1.FireboltEngineStatus{
			CurrentGeneration: 1,
			ActiveGeneration:  1,
		},
	}

	r.gcOrphanedResources(context.Background(), engine)

	if err := fc.Get(context.Background(), types.NamespacedName{Name: unlabeledSTS.Name, Namespace: ns}, &appsv1.StatefulSet{}); err != nil {
		t.Errorf("unlabeled StatefulSet should not have been deleted: %v", err)
	}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: unlabeledCM.Name, Namespace: ns}, &corev1.ConfigMap{}); err != nil {
		t.Errorf("unlabeled ConfigMap should not have been deleted: %v", err)
	}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: clusterSvc.Name, Namespace: ns}, &corev1.Service{}); err != nil {
		t.Errorf("cluster Service should not have been deleted: %v", err)
	}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: currentSTS.Name, Namespace: ns}, &appsv1.StatefulSet{}); err != nil {
		t.Errorf("current-generation StatefulSet should not have been deleted: %v", err)
	}
}

func TestGCOrphanedResources_NoOpWhenClean(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = computev1alpha1.AddToScheme(scheme)

	ns := "test-ns"
	engineName := "my-engine"

	currentSTS := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: engineName + "-g1", Namespace: ns,
			Labels: map[string]string{LabelEngine: engineName, LabelGeneration: "1"},
		},
	}

	ownedByEngine(engineName, currentSTS)

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(currentSTS).
		Build()

	r := &FireboltEngineReconciler{Client: fc, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}

	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: engineName, Namespace: ns, UID: gcTestEngineUID},
		Status: computev1alpha1.FireboltEngineStatus{
			CurrentGeneration: 1,
			ActiveGeneration:  1,
		},
	}

	r.gcOrphanedResources(context.Background(), engine)

	if err := fc.Get(context.Background(), types.NamespacedName{Name: currentSTS.Name, Namespace: ns}, &appsv1.StatefulSet{}); err != nil {
		t.Errorf("current StatefulSet should not have been deleted: %v", err)
	}
}

// gcTestEngine returns an engine whose status places it mid-rollout: still
// Creating a new generation while an older one serves traffic.
func gcTestEngine(name, ns string, currentGen, activeGen int) *computev1alpha1.FireboltEngine {
	return &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: gcTestEngineUID},
		Status: computev1alpha1.FireboltEngineStatus{
			Phase:             computev1alpha1.PhaseCreating,
			CurrentGeneration: currentGen,
			ActiveGeneration:  activeGen,
		},
	}
}

// genSvc builds a per-generation headless Service the sweep can see, owned by
// the engine the way the operator owns the ones it creates.
func genSvc(engineName, ns string, gen int) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: genResourceName(engineName, gen, SuffixHL), Namespace: ns,
			Labels: map[string]string{LabelEngine: engineName, LabelGeneration: strconv.Itoa(gen)},
		},
	}
	ownedByEngine(engineName, svc)
	return svc
}

// TestGCOrphanedResources_PreservesActiveGenerationMidRollout is the safety
// property that lets the sweep run outside the terminal phases: while the engine
// is Creating, the generation serving traffic is ActiveGeneration, not
// CurrentGeneration. Deleting it would take the StatefulSet queries are landing
// on. GCIgnoresActiveGen in formal/FireboltEngine.tla pins the same hazard in
// the model.
func TestGCOrphanedResources_PreservesActiveGenerationMidRollout(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = computev1alpha1.AddToScheme(scheme)

	ns := "test-ns"
	engineName := "my-engine"

	sts := func(gen int) *appsv1.StatefulSet {
		obj := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: genResourceName(engineName, gen, ""), Namespace: ns,
				Labels: map[string]string{LabelEngine: engineName, LabelGeneration: strconv.Itoa(gen)},
			},
		}
		ownedByEngine(engineName, obj)
		return obj
	}
	abandonedSTS, activeSTS, currentSTS := sts(1), sts(2), sts(3)
	abandonedSvc, activeSvc := genSvc(engineName, ns, 1), genSvc(engineName, ns, 2)

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(abandonedSTS, activeSTS, currentSTS, abandonedSvc, activeSvc).
		Build()

	r := &FireboltEngineReconciler{Client: fc, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}

	if backlogged := r.gcOrphanedResources(context.Background(), gcTestEngine(engineName, ns, 3, 2)); backlogged {
		t.Error("expected the sweep to report no backlog for two orphans")
	}

	exists := func(name string, obj client.Object) bool {
		return fc.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, obj) == nil
	}
	if exists(abandonedSTS.Name, &appsv1.StatefulSet{}) {
		t.Error("abandoned StatefulSet (gen 1) should have been deleted in the Creating phase")
	}
	if exists(abandonedSvc.Name, &corev1.Service{}) {
		t.Error("abandoned headless Service (gen 1) should have been deleted in the Creating phase")
	}
	if !exists(activeSTS.Name, &appsv1.StatefulSet{}) {
		t.Error("active StatefulSet (gen 2) must survive: it is the generation serving traffic")
	}
	if !exists(activeSvc.Name, &corev1.Service{}) {
		t.Error("active headless Service (gen 2) must survive: it resolves the serving pods")
	}
	if !exists(currentSTS.Name, &appsv1.StatefulSet{}) {
		t.Error("current StatefulSet (gen 3) must survive: it is the generation being created")
	}
}

// TestGCOrphanedResources_RetriesFailedDeleteOnNextPass covers the reason the
// sweep exists at all: the abandon path issues its deletes once, so anything
// the API server rejects then has to be reclaimed later. The sweep is
// level-triggered, so a delete that fails is simply attempted again on the next
// pass, and one failure does not stop the rest of the pass.
func TestGCOrphanedResources_RetriesFailedDeleteOnNextPass(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = computev1alpha1.AddToScheme(scheme)

	ns := "test-ns"
	engineName := "my-engine"
	orphanA, orphanB := genSvc(engineName, ns, 1), genSvc(engineName, ns, 2)

	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(orphanA, orphanB, genSvc(engineName, ns, 3)).
		Build()

	// Every delete in the first pass is throttled away, as the API server does
	// under generation churn; the second pass is allowed through.
	throttled := true
	fc := interceptor.NewClient(base, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if throttled {
				return goerrors.New("injected delete throttling")
			}
			return c.Delete(ctx, obj, opts...)
		},
	})

	r := &FireboltEngineReconciler{Client: fc, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}
	engine := gcTestEngine(engineName, ns, 3, -1)

	r.gcOrphanedResources(context.Background(), engine)

	gone := func(name string) bool {
		return errors.IsNotFound(base.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, &corev1.Service{}))
	}
	if gone(orphanA.Name) || gone(orphanB.Name) {
		t.Fatal("expected both orphans to survive a pass whose deletes all failed")
	}

	throttled = false
	r.gcOrphanedResources(context.Background(), engine)

	if !gone(orphanA.Name) {
		t.Errorf("orphan %q should have been deleted once deletes were accepted again", orphanA.Name)
	}
	if !gone(orphanB.Name) {
		t.Errorf("orphan %q should have been deleted once deletes were accepted again", orphanB.Name)
	}
}

// TestGCOrphanedResources_StopsAtDeleteBudget pins the per-pass bound. An engine
// that churned generations for hours leaves far more orphans than one reconcile
// should spend on them, and the controller reconciles one engine at a time, so
// the sweep stops at GCMaxDeletesPerPass and reports the backlog instead of
// holding the worker until the whole set is gone.
func TestGCOrphanedResources_StopsAtDeleteBudget(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = computev1alpha1.AddToScheme(scheme)

	ns := "test-ns"
	engineName := "my-engine"
	orphanCount := GCMaxDeletesPerPass + 3

	objs := make([]client.Object, 0, orphanCount+1)
	objs = append(objs, genSvc(engineName, ns, 0))
	for gen := 1; gen <= orphanCount; gen++ {
		objs = append(objs, genSvc(engineName, ns, gen))
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	r := &FireboltEngineReconciler{Client: fc, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}
	engine := gcTestEngine(engineName, ns, 0, 0)

	survivors := func() int {
		list := &corev1.ServiceList{}
		if err := fc.List(context.Background(), list, client.InNamespace(ns)); err != nil {
			t.Fatalf("List Services: %v", err)
		}
		return len(list.Items)
	}

	if backlogged := r.gcOrphanedResources(context.Background(), engine); !backlogged {
		t.Error("expected the sweep to report a backlog after spending its budget")
	}
	// One survivor is the current generation, which is never in scope.
	if want := orphanCount - GCMaxDeletesPerPass + 1; survivors() != want {
		t.Errorf("expected %d Services left after one capped pass, got %d", want, survivors())
	}

	if backlogged := r.gcOrphanedResources(context.Background(), engine); backlogged {
		t.Error("expected the remaining orphans to fit in one more pass")
	}
	if survivors() != 1 {
		t.Errorf("expected only the current generation's Service to survive, got %d Services", survivors())
	}
}

// TestGCOrphanedResources_SkipsTerminatingResources covers the objects whose
// delete has already been accepted and is waiting on a finalizer — a
// StatefulSet under foreground propagation waits for its pods. Re-issuing that
// delete every pass spends budget and API calls without moving anything along.
func TestGCOrphanedResources_SkipsTerminatingResources(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = computev1alpha1.AddToScheme(scheme)

	ns := "test-ns"
	engineName := "my-engine"

	terminating := genSvc(engineName, ns, 1)
	terminating.Finalizers = []string{"example.com/holding"}
	terminating.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
	standing := genSvc(engineName, ns, 2)

	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(terminating, standing).
		Build()

	var deleted []string
	fc := interceptor.NewClient(base, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deleted = append(deleted, obj.GetName())
			return c.Delete(ctx, obj, opts...)
		},
	})

	r := &FireboltEngineReconciler{Client: fc, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}
	r.gcOrphanedResources(context.Background(), gcTestEngine(engineName, ns, 3, -1))

	if len(deleted) != 1 || deleted[0] != standing.Name {
		t.Errorf("expected exactly one delete, of %q; got %v", standing.Name, deleted)
	}
}

// TestReconcileSweepsAbandonedGenerationsWhileCreating drives the FULL
// Reconcile, not gcOrphanedResources in isolation, on the engine shape this
// sweep exists for: pods that never become ready, so the phase never leaves
// Creating and no terminal-phase-gated cleanup would ever run. One reconcile
// must reclaim the abandoned generation and leave both the active generation
// (still serving traffic) and the one being created alone.
func TestReconcileSweepsAbandonedGenerationsWhileCreating(t *testing.T) {
	const (
		ns         = "ns-gc-creating"
		instName   = "parent"
		engName    = "eng-gc"
		abandonedG = 0
		activeG    = 1
		creatingG  = 2
	)

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("clientgoscheme.AddToScheme: %v", err)
	}
	if err := computev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("computev1alpha1.AddToScheme: %v", err)
	}

	labelsFor := func(gen int) map[string]string {
		return map[string]string{LabelEngine: engName, LabelGeneration: strconv.Itoa(gen)}
	}
	sts := func(gen int) *appsv1.StatefulSet {
		obj := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
			Name: genResourceName(engName, gen, ""), Namespace: ns, Labels: labelsFor(gen),
		}}
		ownedByEngine(engName, obj)
		return obj
	}
	hlSvc := func(gen int) *corev1.Service {
		obj := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
			Name: genResourceName(engName, gen, SuffixHL), Namespace: ns, Labels: labelsFor(gen),
		}}
		ownedByEngine(engName, obj)
		return obj
	}
	cm := func(gen int) *corev1.ConfigMap {
		obj := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: genResourceName(engName, gen, SuffixConfig), Namespace: ns, Labels: labelsFor(gen),
		}}
		ownedByEngine(engName, obj)
		return obj
	}

	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{
			Name: engName, Namespace: ns,
			Finalizers: []string{finalizerName},
			Generation: 1,
			UID:        gcTestEngineUID,
		},
		Spec: computev1alpha1.FireboltEngineSpec{InstanceRef: instName, Replicas: 1},
		Status: computev1alpha1.FireboltEngineStatus{
			Phase:             computev1alpha1.PhaseCreating,
			CurrentGeneration: creatingG,
			ActiveGeneration:  activeG,
		},
	}
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: instName, Namespace: ns},
		Spec:       computev1alpha1.FireboltInstanceSpec{ID: "01H000000000000000000DUMMY"},
		Status: computev1alpha1.FireboltInstanceStatus{
			MetadataEndpoint: "metadata." + ns + ".svc.cluster.local:50051",
		},
	}

	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			instance, engine,
			sts(abandonedG), hlSvc(abandonedG), cm(abandonedG),
			sts(activeG), hlSvc(activeG), cm(activeG),
		).
		WithStatusSubresource(&computev1alpha1.FireboltEngine{}, &computev1alpha1.FireboltInstance{}).
		Build()

	r := &FireboltEngineReconciler{Client: cli, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: engName, Namespace: ns}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := &computev1alpha1.FireboltEngine{}
	if err := cli.Get(ctx, types.NamespacedName{Name: engName, Namespace: ns}, got); err != nil {
		t.Fatalf("Get engine: %v", err)
	}
	if got.Status.Phase != computev1alpha1.PhaseCreating {
		t.Fatalf("phase = %q, want creating: the fixture has no ready pods, so the engine must not leave Creating", got.Status.Phase)
	}

	gone := func(name string, obj client.Object) bool {
		return errors.IsNotFound(cli.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, obj))
	}
	if !gone(sts(abandonedG).Name, &appsv1.StatefulSet{}) {
		t.Error("abandoned StatefulSet survived a Creating-phase reconcile")
	}
	if !gone(hlSvc(abandonedG).Name, &corev1.Service{}) {
		t.Error("abandoned headless Service survived a Creating-phase reconcile")
	}
	if !gone(cm(abandonedG).Name, &corev1.ConfigMap{}) {
		t.Error("abandoned ConfigMap survived a Creating-phase reconcile")
	}
	if gone(sts(activeG).Name, &appsv1.StatefulSet{}) {
		t.Error("active StatefulSet was deleted: it is the generation serving traffic")
	}
	if gone(hlSvc(activeG).Name, &corev1.Service{}) {
		t.Error("active headless Service was deleted: it resolves the serving pods")
	}
	if gone(cm(activeG).Name, &corev1.ConfigMap{}) {
		t.Error("active ConfigMap was deleted: its pods mount it on restart")
	}
	if gone(genResourceName(engName, creatingG, ""), &appsv1.StatefulSet{}) {
		t.Error("the generation being created has no StatefulSet after the reconcile that should have ensured it")
	}
}

// TestReconcileRetriesAbandonedGenerationDeletesUntilAccepted is the ticket's
// acceptance path at the controller level: an engine held in Creating, deletes
// rejected while the abandoned generation stands, then accepted. Nothing of the
// abandoned generation may survive once the API takes deletes again, and its
// StatefulSet must go with foreground propagation, which is what takes the
// generation's pods with it.
func TestReconcileRetriesAbandonedGenerationDeletesUntilAccepted(t *testing.T) {
	const (
		ns         = "ns-gc-retry"
		instName   = "parent"
		engName    = "eng-gc-retry"
		abandonedG = 0
		creatingG  = 1
	)

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("clientgoscheme.AddToScheme: %v", err)
	}
	if err := computev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("computev1alpha1.AddToScheme: %v", err)
	}

	labels := map[string]string{LabelEngine: engName, LabelGeneration: strconv.Itoa(abandonedG)}
	abandonedSTS := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: genResourceName(engName, abandonedG, ""), Namespace: ns, Labels: labels,
	}}
	abandonedSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: genResourceName(engName, abandonedG, SuffixHL), Namespace: ns, Labels: labels,
	}}
	ownedByEngine(engName, abandonedSTS, abandonedSvc)
	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{
			Name: engName, Namespace: ns,
			Finalizers: []string{finalizerName},
			Generation: 1,
			UID:        gcTestEngineUID,
		},
		Spec: computev1alpha1.FireboltEngineSpec{InstanceRef: instName, Replicas: 1},
		Status: computev1alpha1.FireboltEngineStatus{
			Phase:             computev1alpha1.PhaseCreating,
			CurrentGeneration: creatingG,
			ActiveGeneration:  -1,
		},
	}
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: instName, Namespace: ns},
		Spec:       computev1alpha1.FireboltInstanceSpec{ID: "01H000000000000000000DUMMY"},
		Status: computev1alpha1.FireboltInstanceStatus{
			MetadataEndpoint: "metadata." + ns + ".svc.cluster.local:50051",
		},
	}

	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(instance, engine, abandonedSTS, abandonedSvc).
		WithStatusSubresource(&computev1alpha1.FireboltEngine{}, &computev1alpha1.FireboltInstance{}).
		Build()

	rejecting := true
	var stsPropagation *metav1.DeletionPropagation
	cli := interceptor.NewClient(base, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if rejecting {
				return goerrors.New("injected delete rejection")
			}
			if _, ok := obj.(*appsv1.StatefulSet); ok {
				var applied client.DeleteOptions
				applied.ApplyOptions(opts)
				stsPropagation = applied.PropagationPolicy
			}
			return c.Delete(ctx, obj, opts...)
		},
	})

	r := &FireboltEngineReconciler{Client: cli, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: engName, Namespace: ns}}

	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("Reconcile with deletes rejected: %v", err)
	}
	if err := base.Get(ctx, types.NamespacedName{Name: abandonedSTS.Name, Namespace: ns}, &appsv1.StatefulSet{}); err != nil {
		t.Fatalf("abandoned StatefulSet should still stand while deletes are rejected: %v", err)
	}
	// The rejected deletes are a backlog, so the pass comes back for them
	// sooner than the Creating phase's own interval.
	if res.RequeueAfter != GCBacklogRequeue {
		t.Errorf("RequeueAfter = %v after rejected deletes, want the backlog interval %v", res.RequeueAfter, GCBacklogRequeue)
	}

	rejecting = false
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile with deletes accepted: %v", err)
	}

	got := &computev1alpha1.FireboltEngine{}
	if err := base.Get(ctx, types.NamespacedName{Name: engName, Namespace: ns}, got); err != nil {
		t.Fatalf("Get engine: %v", err)
	}
	if got.Status.Phase != computev1alpha1.PhaseCreating {
		t.Fatalf("phase = %q, want creating: the engine must never have to leave Creating for the retry", got.Status.Phase)
	}
	for _, obj := range []struct {
		name string
		into client.Object
	}{
		{abandonedSTS.Name, &appsv1.StatefulSet{}},
		{abandonedSvc.Name, &corev1.Service{}},
	} {
		if err := base.Get(ctx, types.NamespacedName{Name: obj.name, Namespace: ns}, obj.into); !errors.IsNotFound(err) {
			t.Errorf("%q survived the pass that could delete again (err=%v)", obj.name, err)
		}
	}
	if stsPropagation == nil || *stsPropagation != metav1.DeletePropagationForeground {
		t.Errorf("StatefulSet delete propagation = %v, want Foreground so the generation's pods go with it", stsPropagation)
	}
}

// TestReconcileSweepsWhenAGateEndsThePass covers the placement, not the sweep:
// every gate in Reconcile ends the pass before the phase machine runs, and an
// engine parked on one of them would keep its abandoned generations forever if
// the sweep sat behind that gate. The instance gate is the representative case
// because it is a normal, error-free early return.
func TestReconcileSweepsWhenAGateEndsThePass(t *testing.T) {
	const (
		ns         = "ns-gc-gated"
		instName   = "parent"
		engName    = "eng-gc-gated"
		abandonedG = 0
		currentG   = 1
	)

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("clientgoscheme.AddToScheme: %v", err)
	}
	if err := computev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("computev1alpha1.AddToScheme: %v", err)
	}

	abandonedSvc := genSvc(engName, ns, abandonedG)
	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{
			Name: engName, Namespace: ns,
			Finalizers: []string{finalizerName},
			Generation: 1,
			UID:        gcTestEngineUID,
		},
		Spec: computev1alpha1.FireboltEngineSpec{InstanceRef: instName, Replicas: 1},
		Status: computev1alpha1.FireboltEngineStatus{
			Phase:             computev1alpha1.PhaseCreating,
			CurrentGeneration: currentG,
			ActiveGeneration:  -1,
		},
	}
	// No MetadataEndpoint on the instance: resolveInstanceInfo fails, so the
	// instance gate returns before the phase machine and before applyEngineState.
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: instName, Namespace: ns},
		Spec:       computev1alpha1.FireboltInstanceSpec{ID: "01H000000000000000000DUMMY"},
	}

	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(instance, engine, abandonedSvc).
		WithStatusSubresource(&computev1alpha1.FireboltEngine{}, &computev1alpha1.FireboltInstance{}).
		Build()

	r := &FireboltEngineReconciler{Client: cli, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: engName, Namespace: ns}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := &computev1alpha1.FireboltEngine{}
	if err := cli.Get(ctx, types.NamespacedName{Name: engName, Namespace: ns}, got); err != nil {
		t.Fatalf("Get engine: %v", err)
	}
	if !apimeta.IsStatusConditionFalse(got.Status.Conditions, computev1alpha1.ConditionInstanceReady) {
		t.Fatalf("expected the instance gate to have blocked this pass; conditions = %+v", got.Status.Conditions)
	}
	if err := cli.Get(ctx, types.NamespacedName{Name: abandonedSvc.Name, Namespace: ns}, &corev1.Service{}); !errors.IsNotFound(err) {
		t.Errorf("abandoned Service survived a pass the instance gate ended (err=%v)", err)
	}
}

// TestApplyGCBacklogRequeue pins how a sweep backlog folds into the reconcile
// result. Two cases carry the weight: a result that already asked for an
// immediate requeue must keep it, since controller-runtime reads RequeueAfter
// first and a delay written over Requeue=true silently replaces a rate-limited
// retry with a wait; and a phase interval longer than the backlog interval must
// give way to it.
func TestApplyGCBacklogRequeue(t *testing.T) {
	cases := []struct {
		name       string
		in         ctrl.Result
		backlogged bool
		want       ctrl.Result
	}{
		{
			name: "no backlog leaves the result alone",
			in:   ctrl.Result{RequeueAfter: 30 * time.Second},
			want: ctrl.Result{RequeueAfter: 30 * time.Second},
		},
		{
			name:       "immediate requeue survives a backlog",
			in:         ctrl.Result{Requeue: true},
			backlogged: true,
			want:       ctrl.Result{Requeue: true},
		},
		{
			name:       "a longer phase interval yields to the backlog",
			in:         ctrl.Result{RequeueAfter: 30 * time.Second},
			backlogged: true,
			want:       ctrl.Result{RequeueAfter: GCBacklogRequeue},
		},
		{
			name:       "a requeue with a delay beside it is not immediate",
			in:         ctrl.Result{Requeue: true, RequeueAfter: 30 * time.Second},
			backlogged: true,
			want:       ctrl.Result{Requeue: true, RequeueAfter: GCBacklogRequeue},
		},
		{
			name:       "a shorter phase interval is kept",
			in:         ctrl.Result{RequeueAfter: time.Second},
			backlogged: true,
			want:       ctrl.Result{RequeueAfter: time.Second},
		},
		{
			name:       "no interval at all takes the backlog interval",
			in:         ctrl.Result{},
			backlogged: true,
			want:       ctrl.Result{RequeueAfter: GCBacklogRequeue},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := applyGCBacklogRequeue(tc.in, tc.backlogged); got != tc.want {
				t.Errorf("applyGCBacklogRequeue(%+v, %t) = %+v, want %+v", tc.in, tc.backlogged, got, tc.want)
			}
		})
	}
}

// TestGCOrphanedResources_ReportsBacklogWhenAListFails covers the read side: a
// pass whose List fails has seen only part of the engine's resources, so it must
// report a backlog rather than "nothing left to do".
func TestGCOrphanedResources_ReportsBacklogWhenAListFails(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = computev1alpha1.AddToScheme(scheme)

	ns := "test-ns"
	engineName := "my-engine"

	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(genSvc(engineName, ns, 0)).
		Build()

	// Services list after StatefulSets, so the sweep gets one clean List first
	// and then fails partway: the orphan Service is never seen.
	fc := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*corev1.ServiceList); ok {
				return goerrors.New("injected list failure")
			}
			return c.List(ctx, list, opts...)
		},
	})

	r := &FireboltEngineReconciler{Client: fc, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}
	if backlogged := r.gcOrphanedResources(context.Background(), gcTestEngine(engineName, ns, 3, -1)); !backlogged {
		t.Error("expected a failed List to report a backlog")
	}
	if err := base.Get(context.Background(), types.NamespacedName{Name: genSvc(engineName, ns, 0).Name, Namespace: ns}, &corev1.Service{}); err != nil {
		t.Errorf("the orphan the failed List hid should still be there for the next pass: %v", err)
	}
}

// TestReconcileReclaimsTheGenerationItAbandonsInTheSamePass pins why the sweep
// is deferred rather than run at the top of the pass. The abandon path deletes
// only what it observed, and a generation's TLS Secret is never in that set, so
// the sweep is the only thing that reclaims it. Running last means the sweep
// reads the status the pass wrote, so the generation abandoned by this pass is
// already outside the keep set.
func TestReconcileReclaimsTheGenerationItAbandonsInTheSamePass(t *testing.T) {
	const (
		ns         = "ns-gc-samepass"
		instName   = "parent"
		engName    = "eng-gc-samepass"
		abandonedG = 1
	)

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("clientgoscheme.AddToScheme: %v", err)
	}
	if err := computev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("computev1alpha1.AddToScheme: %v", err)
	}

	labels := map[string]string{LabelEngine: engName, LabelGeneration: strconv.Itoa(abandonedG)}
	// Replicas disagree with the spec below, which is the first comparison
	// stsMatchesSpec makes: this pass abandons generation 1.
	driftedSTS := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: genResourceName(engName, abandonedG, ""), Namespace: ns, Labels: labels,
		},
		Spec: appsv1.StatefulSetSpec{Replicas: ptr(int32(3))},
	}
	tlsSecretName := genResourceName(engName, abandonedG, SuffixEngineTLS)
	ownedByEngine(engName, driftedSTS)
	// The derived Secret carries no engine owner reference in production either;
	// the sweep admits it on the cert-manager annotation.
	tlsSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: tlsSecretName, Namespace: ns, Labels: labels,
		Annotations: map[string]string{certmanagerv1.CertificateNameKey: tlsSecretName},
	}}

	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{
			Name: engName, Namespace: ns,
			Finalizers: []string{finalizerName},
			Generation: 1,
			UID:        gcTestEngineUID,
		},
		Spec: computev1alpha1.FireboltEngineSpec{InstanceRef: instName, Replicas: 1},
		Status: computev1alpha1.FireboltEngineStatus{
			Phase:             computev1alpha1.PhaseCreating,
			CurrentGeneration: abandonedG,
			ActiveGeneration:  -1,
		},
	}
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: instName, Namespace: ns},
		Spec:       computev1alpha1.FireboltInstanceSpec{ID: "01H000000000000000000DUMMY"},
		Status: computev1alpha1.FireboltInstanceStatus{
			MetadataEndpoint: "metadata." + ns + ".svc.cluster.local:50051",
		},
	}

	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(instance, engine, driftedSTS, tlsSecret).
		WithStatusSubresource(&computev1alpha1.FireboltEngine{}, &computev1alpha1.FireboltInstance{}).
		Build()

	r := &FireboltEngineReconciler{Client: cli, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: engName, Namespace: ns}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := &computev1alpha1.FireboltEngine{}
	if err := cli.Get(ctx, types.NamespacedName{Name: engName, Namespace: ns}, got); err != nil {
		t.Fatalf("Get engine: %v", err)
	}
	if got.Status.CurrentGeneration != abandonedG+1 {
		t.Fatalf("CurrentGeneration = %d, want %d: this pass was supposed to abandon the drifted generation",
			got.Status.CurrentGeneration, abandonedG+1)
	}
	if err := cli.Get(ctx, types.NamespacedName{Name: tlsSecretName, Namespace: ns}, &corev1.Secret{}); !errors.IsNotFound(err) {
		t.Errorf("the abandoned generation's TLS Secret survived the pass that abandoned it (err=%v)", err)
	}
}

// TestReconcilePanicSkipsTheSweep covers the other half of the deferred call. A
// panic means the pass reached a state the reconciler holds to be impossible, so
// the status backing the keep set cannot be trusted: the panic must propagate
// and nothing may be deleted on the way out.
func TestReconcilePanicSkipsTheSweep(t *testing.T) {
	const (
		ns       = "ns-gc-panic"
		instName = "parent"
		engName  = "eng-gc-panic"
	)

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("clientgoscheme.AddToScheme: %v", err)
	}
	if err := computev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("computev1alpha1.AddToScheme: %v", err)
	}

	orphan := genSvc(engName, ns, 5)
	// Stable with no active generation is the state computeStable panics on.
	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{
			Name: engName, Namespace: ns,
			Finalizers: []string{finalizerName},
			Generation: 1,
			UID:        gcTestEngineUID,
		},
		Spec: computev1alpha1.FireboltEngineSpec{InstanceRef: instName, Replicas: 1},
		Status: computev1alpha1.FireboltEngineStatus{
			Phase:             computev1alpha1.PhaseStable,
			CurrentGeneration: 0,
			ActiveGeneration:  -1,
		},
	}
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: instName, Namespace: ns},
		Spec:       computev1alpha1.FireboltInstanceSpec{ID: "01H000000000000000000DUMMY"},
		Status: computev1alpha1.FireboltInstanceStatus{
			MetadataEndpoint: "metadata." + ns + ".svc.cluster.local:50051",
		},
	}

	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(instance, engine, orphan).
		WithStatusSubresource(&computev1alpha1.FireboltEngine{}, &computev1alpha1.FireboltInstance{}).
		Build()

	r := &FireboltEngineReconciler{Client: cli, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}
	ctx := context.Background()

	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected the terminal-phase invariant to panic")
			}
		}()
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: engName, Namespace: ns}})
	}()

	if err := cli.Get(ctx, types.NamespacedName{Name: orphan.Name, Namespace: ns}, &corev1.Service{}); err != nil {
		t.Errorf("an orphan was deleted while unwinding a panic: %v", err)
	}
}

// TestGCOrphanedResources_LeavesUnownedResourcesAlone is the blast-radius
// guard. Labels are copyable: a Service, ConfigMap, Secret or StatefulSet a user
// or another controller tags with this engine's name and a stale generation
// would be swept on label match alone, and for a Secret that is unrecoverable.
// The operator's own children carry a controller reference to the engine, so the
// reference is what admits them.
func TestGCOrphanedResources_LeavesUnownedResourcesAlone(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = computev1alpha1.AddToScheme(scheme)

	ns := "test-ns"
	engineName := "my-engine"
	staleLabels := map[string]string{LabelEngine: engineName, LabelGeneration: "1"}

	// Same labels as an abandoned generation, none of the provenance.
	foreignSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "someone-elses-service", Namespace: ns, Labels: staleLabels,
	}}
	foreignSTS := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: "someone-elses-sts", Namespace: ns, Labels: staleLabels,
	}}
	foreignSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "someone-elses-secret", Namespace: ns, Labels: staleLabels,
	}}
	// Owned, but by a different engine of the same name in another incarnation:
	// the UID is what distinguishes them.
	recreatedEngineSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "older-incarnation", Namespace: ns, Labels: staleLabels,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: computev1alpha1.GroupVersion.String(),
			Kind:       "FireboltEngine",
			Name:       engineName,
			UID:        "99999999-9999-9999-9999-999999999999",
			Controller: ptr(true),
		}},
	}}
	// A Secret labeled like a generation's TLS Secret but whose cert-manager
	// annotation points somewhere else entirely.
	foreignCertSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: genResourceName(engineName, 1, SuffixEngineTLS), Namespace: ns, Labels: staleLabels,
		Annotations: map[string]string{certmanagerv1.CertificateNameKey: "unrelated-cert"},
	}}
	// The operator's own orphan, to prove the sweep still runs in this pass.
	ownedOrphan := genSvc(engineName, ns, 1)

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(foreignSvc, foreignSTS, foreignSecret, recreatedEngineSvc, foreignCertSecret, ownedOrphan).
		Build()

	r := &FireboltEngineReconciler{Client: fc, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}
	r.gcOrphanedResources(context.Background(), gcTestEngine(engineName, ns, 3, -1))

	survivors := []struct {
		name string
		into client.Object
	}{
		{foreignSvc.Name, &corev1.Service{}},
		{foreignSTS.Name, &appsv1.StatefulSet{}},
		{foreignSecret.Name, &corev1.Secret{}},
		{recreatedEngineSvc.Name, &corev1.Service{}},
		{foreignCertSecret.Name, &corev1.Secret{}},
	}
	for _, s := range survivors {
		if err := fc.Get(context.Background(), types.NamespacedName{Name: s.name, Namespace: ns}, s.into); err != nil {
			t.Errorf("%q was deleted: the sweep may only delete what this engine owns (%v)", s.name, err)
		}
	}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: ownedOrphan.Name, Namespace: ns}, &corev1.Service{}); !errors.IsNotFound(err) {
		t.Errorf("the engine's own orphan should still have been deleted in this pass (err=%v)", err)
	}
}

// TestGCOrphanedResources_MovesOnWhenAKindKeepsFailing covers budget fairness.
// Kinds are swept in a fixed order, so a kind whose deletes are rejected
// persistently — an RBAC gap on one resource, say — would spend the whole
// per-pass budget on the same prefix every pass, and the kinds after it would
// never be reached at all.
func TestGCOrphanedResources_MovesOnWhenAKindKeepsFailing(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = computev1alpha1.AddToScheme(scheme)

	ns := "test-ns"
	engineName := "my-engine"

	objs := make([]client.Object, 0, GCMaxDeletesPerPass+4)
	for gen := 1; gen <= GCMaxDeletesPerPass+2; gen++ {
		sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
			Name: genResourceName(engineName, gen, ""), Namespace: ns,
			Labels: map[string]string{LabelEngine: engineName, LabelGeneration: strconv.Itoa(gen)},
		}}
		ownedByEngine(engineName, sts)
		objs = append(objs, sts)
	}
	orphanSvc := genSvc(engineName, ns, 1)
	objs = append(objs, orphanSvc)

	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	stsDeletes := 0
	fc := interceptor.NewClient(base, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if _, ok := obj.(*appsv1.StatefulSet); ok {
				stsDeletes++
				return goerrors.New("injected StatefulSet delete rejection")
			}
			return c.Delete(ctx, obj, opts...)
		},
	})

	r := &FireboltEngineReconciler{Client: fc, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}
	if backlogged := r.gcOrphanedResources(context.Background(), gcTestEngine(engineName, ns, 0, -1)); !backlogged {
		t.Error("expected a backlog while a whole kind's deletes are failing")
	}

	if stsDeletes > GCMaxKindFailuresPerPass {
		t.Errorf("kept retrying a failing kind: %d StatefulSet deletes, want at most %d",
			stsDeletes, GCMaxKindFailuresPerPass)
	}
	if err := base.Get(context.Background(), types.NamespacedName{Name: orphanSvc.Name, Namespace: ns}, &corev1.Service{}); !errors.IsNotFound(err) {
		t.Errorf("the Service after the failing kind was never reached (err=%v)", err)
	}
}

// TestGCOrphanedResources_KeepsTheGenerationTheServiceSelects covers the
// divergence case. Status and the cluster Service selector normally agree, but a
// pass whose Service repair failed returns an error with the status ahead of the
// selector, and the sweep runs on that path deliberately. Deleting the selected
// generation there would cut the traffic that is still landing on it.
func TestGCOrphanedResources_KeepsTheGenerationTheServiceSelects(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = computev1alpha1.AddToScheme(scheme)

	ns := "test-ns"
	engineName := "my-engine"
	const (
		servingGen = 2 // what the Service still points at
		statusGen  = 3 // what the status has moved on to
	)

	servingSTS := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: genResourceName(engineName, servingGen, ""), Namespace: ns,
		Labels: map[string]string{LabelEngine: engineName, LabelGeneration: strconv.Itoa(servingGen)},
	}}
	staleSTS := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: genResourceName(engineName, 1, ""), Namespace: ns,
		Labels: map[string]string{LabelEngine: engineName, LabelGeneration: "1"},
	}}
	ownedByEngine(engineName, servingSTS, staleSTS)

	clusterSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: engineName + SuffixService, Namespace: ns,
			Labels: map[string]string{LabelEngine: engineName},
		},
		Spec: corev1.ServiceSpec{Selector: map[string]string{
			LabelEngine:     engineName,
			LabelGeneration: strconv.Itoa(servingGen),
		}},
	}
	ownedByEngine(engineName, clusterSvc)

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(servingSTS, staleSTS, clusterSvc).
		Build()

	r := &FireboltEngineReconciler{Client: fc, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}
	r.gcOrphanedResources(context.Background(), gcTestEngine(engineName, ns, statusGen, statusGen))

	if err := fc.Get(context.Background(), types.NamespacedName{Name: servingSTS.Name, Namespace: ns}, &appsv1.StatefulSet{}); err != nil {
		t.Errorf("the generation the cluster Service selects was deleted: %v", err)
	}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: staleSTS.Name, Namespace: ns}, &appsv1.StatefulSet{}); !errors.IsNotFound(err) {
		t.Errorf("a generation nothing points at should still be swept (err=%v)", err)
	}
}

// TestGCOrphanedResources_BailsWhenTheClusterServiceIsUnreadable covers the
// fail-closed side of the same read: without the selector there is no way to
// tell which generation is serving, so the pass reports a backlog and deletes
// nothing.
func TestGCOrphanedResources_BailsWhenTheClusterServiceIsUnreadable(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = computev1alpha1.AddToScheme(scheme)

	ns := "test-ns"
	engineName := "my-engine"
	orphan := genSvc(engineName, ns, 1)

	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(orphan).Build()
	fc := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == engineName+SuffixService {
				return goerrors.New("injected cluster Service read failure")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	r := &FireboltEngineReconciler{Client: fc, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}
	if backlogged := r.gcOrphanedResources(context.Background(), gcTestEngine(engineName, ns, 3, -1)); !backlogged {
		t.Error("expected an unreadable cluster Service to report a backlog")
	}
	if err := base.Get(context.Background(), types.NamespacedName{Name: orphan.Name, Namespace: ns}, &corev1.Service{}); err != nil {
		t.Errorf("nothing may be deleted while the serving generation is unknown: %v", err)
	}
}
