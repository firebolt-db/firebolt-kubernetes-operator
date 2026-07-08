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
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	enginemetrics "github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
)

// TestReconcileDraining_BusyPodBlocksCleanup drives the FULL Reconcile
// (not isPodDrained in isolation, which engine_scrape_test.go already pins)
// through the draining phase against a live metrics endpoint: a draining-
// generation pod reporting active queries must hold the engine in draining
// with the drain-check requeue, and only after the gauges read zero may the
// reconciler advance to cleaning and delete the old generation's resources.
//
// Transport: the default PodIP scrape mode dials http://<PodIP>:9090/metrics
// with plain HTTP, so the test points the fixture pod's status.podIP at
// 127.0.0.1 and serves a real exposition body on the metrics port. This
// exercises the same read path production uses (getEngineState ->
// checkDrainComplete -> podIPScraper -> parsePrometheusGauge) with no test
// seam in the reconciler.
func TestReconcileDraining_BusyPodBlocksCleanup(t *testing.T) {
	lc := &net.ListenConfig{}
	ln, err := lc.Listen(context.Background(), "tcp4", fmt.Sprintf("127.0.0.1:%d", MetricsPort))
	if err != nil {
		t.Skipf("cannot bind 127.0.0.1:%d for the fake engine metrics endpoint: %v", MetricsPort, err)
	}
	var body atomic.Value
	body.Store("firebolt_running_queries 2\nfirebolt_suspended_queries 1\n")
	srv := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, body.Load().(string))
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	const (
		ns       = "ns-drain"
		instName = "parent"
		engName  = "eng-drain"
		oldGen   = 0
		newGen   = 1
	)
	drainInterval := 2 * time.Second

	sch := drainBlockTestScheme(t)

	drainingGen := oldGen
	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{
			Name:       engName,
			Namespace:  ns,
			Finalizers: []string{finalizerName},
			Generation: 1,
		},
		Spec: computev1alpha1.FireboltEngineSpec{
			InstanceRef:        instName,
			Replicas:           1,
			DrainCheckInterval: &metav1.Duration{Duration: drainInterval},
			// DrainCheckEnabled left nil: the effective default is true,
			// which is exactly the production posture under test.
		},
		Status: computev1alpha1.FireboltEngineStatus{
			Phase:              computev1alpha1.PhaseDraining,
			CurrentGeneration:  newGen,
			ActiveGeneration:   newGen,
			DrainingGeneration: &drainingGen,
			ObservedGeneration: 1,
		},
	}
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: instName, Namespace: ns},
		Spec:       computev1alpha1.FireboltInstanceSpec{ID: "01H000000000000000000DUMMY"},
		Status: computev1alpha1.FireboltInstanceStatus{
			MetadataEndpoint: "metadata." + ns + ".svc.cluster.local:50051",
		},
	}
	oldSTS := drainBlockSTS(genResourceName(engName, oldGen, ""), ns, engName, oldGen)
	newSTS := drainBlockSTS(genResourceName(engName, newGen, ""), ns, engName, newGen)
	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      genResourceName(engName, oldGen, "") + "-0",
			Namespace: ns,
			Labels: map[string]string{
				LabelEngine:     engName,
				LabelGeneration: strconv.Itoa(oldGen),
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "engine", Image: "x"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "127.0.0.1",
		},
	}

	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(instance, engine, oldSTS, newSTS, oldPod).
		WithStatusSubresource(&computev1alpha1.FireboltEngine{}, &computev1alpha1.FireboltInstance{}).
		Build()

	r := &FireboltEngineReconciler{
		Client:          cli,
		Scheme:          sch,
		MetricsRecorder: enginemetrics.NoOpEngineRecorder{},
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: engName, Namespace: ns}}

	// Phase 1: the pod reports active queries. Multiple consecutive
	// reconciles must hold draining, keep the old STS, and requeue at the
	// drain-check interval.
	for i := 0; i < 3; i++ {
		res, err := r.Reconcile(context.Background(), req)
		if err != nil {
			t.Fatalf("Reconcile #%d (busy): %v", i, err)
		}
		if res.RequeueAfter != drainInterval {
			t.Fatalf("Reconcile #%d (busy): RequeueAfter = %v, want the drain-check interval %v", i, res.RequeueAfter, drainInterval)
		}
		got := drainBlockGetEngine(t, cli, engName, ns)
		if got.Status.Phase != computev1alpha1.PhaseDraining {
			t.Fatalf("Reconcile #%d (busy): phase = %q, want draining — a busy pod was released", i, got.Status.Phase)
		}
		if got.Status.DrainingGeneration == nil || *got.Status.DrainingGeneration != oldGen {
			t.Fatalf("Reconcile #%d (busy): drainingGeneration = %v, want %d", i, got.Status.DrainingGeneration, oldGen)
		}
		if err := cli.Get(context.Background(), types.NamespacedName{Name: oldSTS.Name, Namespace: ns}, &appsv1.StatefulSet{}); err != nil {
			t.Fatalf("Reconcile #%d (busy): old-generation STS gone while its pod had active queries: %v", i, err)
		}
	}

	// Phase 2: queries finish. The next reconciles must advance through
	// cleaning and delete the old generation's StatefulSet.
	body.Store("firebolt_running_queries 0\nfirebolt_suspended_queries 0\n")

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("Reconcile (drained): %v", err)
		}
		got := drainBlockGetEngine(t, cli, engName, ns)
		if got.Status.Phase == computev1alpha1.PhaseStable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("engine never reached stable after drain completed; phase = %q", got.Status.Phase)
		}
	}

	got := drainBlockGetEngine(t, cli, engName, ns)
	if got.Status.DrainingGeneration != nil {
		t.Errorf("drainingGeneration = %v after cleanup, want nil", *got.Status.DrainingGeneration)
	}
	err = cli.Get(context.Background(), types.NamespacedName{Name: oldSTS.Name, Namespace: ns}, &appsv1.StatefulSet{})
	if !errors.IsNotFound(err) {
		t.Errorf("old-generation STS still present after drain completed (err=%v), want deleted", err)
	}
	if err := cli.Get(context.Background(), types.NamespacedName{Name: newSTS.Name, Namespace: ns}, &appsv1.StatefulSet{}); err != nil {
		t.Errorf("new-generation STS must survive cleanup: %v", err)
	}
}

func drainBlockTestScheme(t *testing.T) *runtime.Scheme {
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

func drainBlockSTS(name, ns, engName string, gen int) *appsv1.StatefulSet {
	replicas := int32(1)
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				LabelEngine:     engName,
				LabelGeneration: strconv.Itoa(gen),
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{LabelEngine: engName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{LabelEngine: engName}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "engine", Image: "x"}}},
			},
		},
	}
}

func drainBlockGetEngine(t *testing.T, cli client.Client, name, ns string) *computev1alpha1.FireboltEngine {
	t.Helper()
	engine := &computev1alpha1.FireboltEngine{}
	if err := cli.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, engine); err != nil {
		t.Fatalf("Get engine: %v", err)
	}
	return engine
}
