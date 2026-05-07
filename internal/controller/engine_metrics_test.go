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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
)

// Verifies firebolt_engine_* gauges are populated on reconcile paths that
// return early without traversing the success branch.
func TestEngineReconcile_RecordsMetrics(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = computev1alpha1.AddToScheme(scheme)

	const (
		ns         = "engine-metrics-test-ns"
		instanceID = "parent-instance"
	)

	cases := []struct {
		name             string
		engineName       string
		initialPhase     computev1alpha1.EnginePhase
		expectedPhaseSet string
	}{
		{
			name:             "phase empty initializes to Creating then requeues",
			engineName:       "deferred-engine-empty",
			initialPhase:     "",
			expectedPhaseSet: "creating",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := &computev1alpha1.FireboltEngine{
				ObjectMeta: metav1.ObjectMeta{
					Name:       tc.engineName,
					Namespace:  ns,
					Finalizers: []string{finalizerName},
				},
				Spec: computev1alpha1.FireboltEngineSpec{
					InstanceRef: instanceID,
					Replicas:    2,
				},
				Status: computev1alpha1.FireboltEngineStatus{
					Phase: tc.initialPhase,
				},
			}

			fc := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(engine).
				WithStatusSubresource(engine).
				Build()

			r := &FireboltEngineReconciler{
				Client:          fc,
				Scheme:          scheme,
				MetricsRecorder: metrics.NewEngineRecorder(),
			}

			if _, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: tc.engineName, Namespace: ns},
			}); err != nil {
				t.Fatalf("Reconcile returned error: %v", err)
			}

			if v := readGauge(t, metrics.EnginePhase.WithLabelValues(ns, tc.engineName, instanceID, tc.expectedPhaseSet)); v != 1 {
				t.Errorf("EnginePhase{phase=%s} = %v, want 1", tc.expectedPhaseSet, v)
			}
			if v := readGauge(t, metrics.EngineSpecReplicas.WithLabelValues(ns, tc.engineName, instanceID)); v != 2 {
				t.Errorf("EngineSpecReplicas = %v, want 2", v)
			}
			if v := readGauge(t, metrics.EnginePodsReady.WithLabelValues(ns, tc.engineName, instanceID)); v != 0 {
				t.Errorf("EnginePodsReady = %v, want 0 (current is zero before getEngineState runs)", v)
			}
			if v := readGauge(t, metrics.EngineLastReconciled.WithLabelValues(ns, tc.engineName, instanceID)); v == 0 {
				t.Error("EngineLastReconciled = 0, want a non-zero unix timestamp")
			}
		})
	}
}
