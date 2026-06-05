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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

func TestParsePrometheusGauge(t *testing.T) {
	body := []byte(`# HELP firebolt_running_queries Number of running queries
# TYPE firebolt_running_queries gauge
firebolt_running_queries 0
# HELP firebolt_suspended_queries Number of suspended queries
# TYPE firebolt_suspended_queries gauge
firebolt_suspended_queries 2
some_other_metric 42
firebolt_running_queries_total 99
with_timestamp 7 1720000000000
float_value 3.0
`)

	tests := []struct {
		name    string
		metric  string
		wantVal int64
		wantOK  bool
	}{
		{"running zero", MetricRunningQueries, 0, true},
		{"suspended two", MetricSuspendedQueries, 2, true},
		{"missing", "firebolt_missing_metric", 0, false},
		{"must not prefix-match", "firebolt_running_queries_tot", 0, false},
		{"trailing timestamp", "with_timestamp", 7, true},
		{"float truncates to int", "float_value", 3, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parsePrometheusGauge(body, tt.metric)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.wantVal {
				t.Errorf("val = %d, want %d", got, tt.wantVal)
			}
		})
	}
}

func TestParsePrometheusGaugeRejectsLabeledSeries(t *testing.T) {
	// A labeled sample like `firebolt_running_queries{kind="x"} 5` must not
	// be picked up: our exact-prefix matcher expects "<name> <value>".
	// Core currently publishes these gauges without labels; if that changes
	// this test will fail loudly and we will update the parser deliberately.
	body := []byte(`firebolt_running_queries{kind="x"} 5
`)
	_, ok := parsePrometheusGauge(body, MetricRunningQueries)
	if ok {
		t.Fatal("expected labeled series to be ignored, but parser accepted it")
	}
}

// TestGetTerminationGracePeriod_AlwaysDefault pins the post-FB-1426
// invariant: TGPS is operator-owned. Neither the engine spec nor any
// pod template can change it — getTerminationGracePeriod always returns
// the operator default, and the admission webhook rejects user-supplied
// TGPS on both engine.spec.template and FireboltEngineClass.spec.template.
func TestGetTerminationGracePeriod_AlwaysDefault(t *testing.T) {
	if got := getTerminationGracePeriod(&computev1alpha1.FireboltEngineSpec{}); got != DefaultTerminationGracePeriodSeconds {
		t.Errorf("getTerminationGracePeriod(empty) = %d, want %d", got, DefaultTerminationGracePeriodSeconds)
	}
	if got := getTerminationGracePeriod(testSpec()); got != DefaultTerminationGracePeriodSeconds {
		t.Errorf("getTerminationGracePeriod(testSpec) = %d, want %d", got, DefaultTerminationGracePeriodSeconds)
	}
}

func TestBuildStatefulSetDefaultsTGPS(t *testing.T) {
	spec := testSpec()
	sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, nil)
	got := sts.Spec.Template.Spec.TerminationGracePeriodSeconds
	if got == nil || *got != int64(DefaultTerminationGracePeriodSeconds) {
		t.Fatalf("expected default TGPS=%d, got %v", DefaultTerminationGracePeriodSeconds, got)
	}
	if len(sts.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(sts.Spec.Template.Spec.Containers))
	}
	if c := sts.Spec.Template.Spec.Containers[0]; c.Lifecycle != nil && c.Lifecycle.PreStop != nil {
		t.Fatal("engine container must not have a preStop hook")
	}
}

// TestEngineShutdownWaitSeconds_ClampsAndMargin exercises the
// gracePeriod → shutdown-wait conversion directly. Post-FB-1426 the
// production TGPS is always the operator default, so this test pins
// down only the helper's arithmetic (margin subtraction + 1s floor)
// rather than threading TGPS through a spec field that no longer
// exists.
func TestEngineShutdownWaitSeconds_ClampsAndMargin(t *testing.T) {
	tests := []struct {
		name string
		tgps int64
		want int64
	}{
		{"default 60s", 60, 55},
		{"custom 120s", 120, 115},
		{"5s floors to 1s", 5, 1},
		{"boundary 6s floors to 1s", 6, 1},
		{"7s leaves 2s", 7, 2},
		{"very small 1s clamps to 1", 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := engineShutdownWaitSeconds(tt.tgps); got != tt.want {
				t.Errorf("engineShutdownWaitSeconds(%d) = %d, want %d", tt.tgps, got, tt.want)
			}
		})
	}
}

// TestShutdownWaitUnfinished_ConfigMap pins the wiring from the
// operator-default TGPS into the rendered config.yaml's
// engine.termination_grace_period field. The production path is the
// only configurable scenario now that user TGPS overrides are
// rejected at admission on both engine.spec.template and
// FireboltEngineClass.spec.template.
func TestShutdownWaitUnfinished_ConfigMap(t *testing.T) {
	cm := buildConfigMap(testSpec(), testEngineName, testNamespace, 0, testInstanceInfo(), nil)
	var wrapper struct {
		Engine struct {
			TerminationGracePeriod string `json:"termination_grace_period"`
		} `json:"engine"`
	}
	if err := yaml.Unmarshal([]byte(cm.Data[ConfigFileName]), &wrapper); err != nil {
		t.Fatalf("failed to parse config.yaml: %v", err)
	}
	if want := "55s"; wrapper.Engine.TerminationGracePeriod != want {
		t.Errorf("engine.termination_grace_period = %q, want %q",
			wrapper.Engine.TerminationGracePeriod, want)
	}
}

// Compile-time guard: make sure we never silently drop the container name
// the drain check targets. If this constant ever renames we want the test
// suite to yell.
var _ = corev1.Container{Name: computev1alpha1.EngineContainerName}
