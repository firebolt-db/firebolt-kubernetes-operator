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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
)

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

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(orphanedSTS, currentSTS, orphanedSvc, currentSvc, clusterSvc, orphanedCM, currentCM).
		Build()

	r := &FireboltEngineReconciler{Client: fc, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}

	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: engineName, Namespace: ns},
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

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(drainingSTS, currentSTS).
		Build()

	r := &FireboltEngineReconciler{Client: fc, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}

	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: engineName, Namespace: ns},
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

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(unlabeledSTS, unlabeledCM, clusterSvc, currentSTS).
		Build()

	r := &FireboltEngineReconciler{Client: fc, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}

	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: engineName, Namespace: ns},
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

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(currentSTS).
		Build()

	r := &FireboltEngineReconciler{Client: fc, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}

	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: engineName, Namespace: ns},
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
