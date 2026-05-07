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

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
)

// Verifies firebolt_instance_* gauges are populated on reconcile paths that
// return early without traversing the success branch.
func TestInstanceReconcile_RecordsMetrics(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = computev1alpha1.AddToScheme(scheme)

	const (
		ns = "metrics-test-ns"
		id = "01METRICSDEFERTEST"
	)

	cases := []struct {
		name             string
		instanceName     string
		initialPhase     computev1alpha1.InstancePhase
		expectedPhaseSet string // value of Status.Phase the recorder should see at function exit
	}{
		{
			name:             "phase=Failed terminal early-return",
			instanceName:     "deferred-failed",
			initialPhase:     computev1alpha1.InstancePhaseFailed,
			expectedPhaseSet: "Failed",
		},
		{
			name:             "phase empty initializes to Provisioning then requeues",
			instanceName:     "deferred-empty",
			initialPhase:     "",
			expectedPhaseSet: "Provisioning",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			instance := &computev1alpha1.FireboltInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:       tc.instanceName,
					Namespace:  ns,
					Finalizers: []string{instanceFinalizerName},
				},
				Spec: computev1alpha1.FireboltInstanceSpec{
					ID:       id,
					Metadata: computev1alpha1.MetadataSpec{},
				},
				Status: computev1alpha1.FireboltInstanceStatus{
					Phase: tc.initialPhase,
				},
			}

			fc := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(instance).
				WithStatusSubresource(instance).
				Build()

			r := &FireboltInstanceReconciler{
				Client:          fc,
				Scheme:          scheme,
				MetricsRecorder: metrics.NewInstanceRecorder(),
			}

			if _, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: tc.instanceName, Namespace: ns},
			}); err != nil {
				t.Fatalf("Reconcile returned error: %v", err)
			}

			if v := readGauge(t, metrics.InstancePhase.WithLabelValues(ns, tc.instanceName, tc.expectedPhaseSet)); v != 1 {
				t.Errorf("InstancePhase{phase=%s} = %v, want 1", tc.expectedPhaseSet, v)
			}
			if v := readGauge(t, metrics.InstanceInfo.WithLabelValues(ns, tc.instanceName, id, "internal")); v != 1 {
				t.Errorf("InstanceInfo{id=%s} = %v, want 1", id, v)
			}
			if v := readGauge(t, metrics.InstanceLastReconciled.WithLabelValues(ns, tc.instanceName)); v == 0 {
				t.Error("InstanceLastReconciled = 0, want a non-zero unix timestamp")
			}
		})
	}
}

func readGauge(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("gauge.Write: %v", err)
	}
	if m.Gauge == nil || m.Gauge.Value == nil {
		t.Fatal("gauge has no value")
	}
	return *m.Gauge.Value
}
