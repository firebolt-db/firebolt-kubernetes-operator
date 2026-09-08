// metricsdump prints the operator's metric exposition for one FireboltInstance
// and one FireboltEngine, exactly as a scraper would read it from /metrics.
//
// Consumers of these metrics (the firebolt-instance-observability chart's
// recording rules and alerts) test against label sets written by hand, which
// cannot notice when the operator exposes a label a scraper overwrites. This
// program gives them a real exposition to scrape instead:
//
//	go run ./hack/metricsdump > operator-exposition.prom
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
)

func main() {
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Namespace: "firebolt", Name: "prod"},
		Spec:       computev1alpha1.FireboltInstanceSpec{ID: "01hxxxxdumpinstance0000001"},
		Status: computev1alpha1.FireboltInstanceStatus{
			Phase:      "Ready",
			Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}},
		},
	}
	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Namespace: "firebolt", Name: "eng"},
		Spec:       computev1alpha1.FireboltEngineSpec{InstanceRef: "prod", Replicas: 1},
		Status: computev1alpha1.FireboltEngineStatus{
			Phase:            "stable",
			ActiveGeneration: 1,
			Conditions:       []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}},
		},
	}
	metrics.NewInstanceRecorder().Record(instance)
	metrics.NewEngineRecorder().Record(engine, 1, 1)

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", http.NoBody)
	promhttp.HandlerFor(ctrlmetrics.Registry, promhttp.HandlerOpts{}).ServeHTTP(rec, req)
	if rec.Code != 200 {
		fmt.Fprintf(os.Stderr, "metrics handler returned %d\n", rec.Code)
		os.Exit(1)
	}
	if _, err := os.Stdout.Write(rec.Body.Bytes()); err != nil {
		fmt.Fprintf(os.Stderr, "write exposition: %v\n", err)
		os.Exit(1)
	}
}
