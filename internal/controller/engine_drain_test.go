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

func TestGetTerminationGracePeriod(t *testing.T) {
	tests := []struct {
		name string
		spec *computev1alpha1.FireboltEngineSpec
		want int64
	}{
		{
			name: "default when unset",
			spec: &computev1alpha1.FireboltEngineSpec{},
			want: DefaultTerminationGracePeriodSeconds,
		},
		{
			name: "override when set",
			spec: &computev1alpha1.FireboltEngineSpec{
				TerminationGracePeriodSeconds: int64Pointer(120),
			},
			want: 120,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getTerminationGracePeriod(tt.spec); got != tt.want {
				t.Errorf("getTerminationGracePeriod() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestBuildStatefulSetInstallsTGPS(t *testing.T) {
	spec := testSpec()
	custom := int64(45)
	spec.TerminationGracePeriodSeconds = &custom

	sts := buildStatefulSet(spec, testEngineName, testNamespace, 0)

	podSpec := sts.Spec.Template.Spec
	if podSpec.TerminationGracePeriodSeconds == nil || *podSpec.TerminationGracePeriodSeconds != 45 {
		t.Fatalf("expected TGPS=45, got %v", podSpec.TerminationGracePeriodSeconds)
	}

	if len(podSpec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(podSpec.Containers))
	}
	c := podSpec.Containers[0]
	if c.Lifecycle != nil && c.Lifecycle.PreStop != nil {
		t.Fatal("engine container must not have a preStop hook")
	}
}

func TestBuildStatefulSetDefaultsTGPS(t *testing.T) {
	spec := testSpec()
	spec.TerminationGracePeriodSeconds = nil
	sts := buildStatefulSet(spec, testEngineName, testNamespace, 0)
	got := sts.Spec.Template.Spec.TerminationGracePeriodSeconds
	if got == nil || *got != int64(DefaultTerminationGracePeriodSeconds) {
		t.Fatalf("expected default TGPS=%d, got %v", DefaultTerminationGracePeriodSeconds, got)
	}
}

func TestShutdownWaitUnfinished(t *testing.T) {
	// engine.termination_grace_period in the rendered config.yaml is the
	// engine's post-SIGTERM drain budget; FireboltCoreServer maps it onto
	// the legacy `shutdown_wait_unfinished` Poco setting. Format is a
	// duration string ("Ns") because the structured schema rejects bare
	// integers for durations.
	tests := []struct {
		name string
		tgps int64
		want string
	}{
		{"default 60s", 60, "55s"},
		{"custom 120s", 120, "115s"},
		{"small 5s clamps to tgps-1", 5, "4s"},
		{"very small 1s clamps to 1", 1, "1s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := testSpec()
			spec.TerminationGracePeriodSeconds = &tt.tgps
			cm := buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfo())
			var wrapper struct {
				Engine struct {
					TerminationGracePeriod string `json:"termination_grace_period"`
				} `json:"engine"`
			}
			if err := yaml.Unmarshal([]byte(cm.Data[ConfigFileName]), &wrapper); err != nil {
				t.Fatalf("failed to parse config.yaml: %v", err)
			}
			if wrapper.Engine.TerminationGracePeriod != tt.want {
				t.Errorf("engine.termination_grace_period = %q, want %q",
					wrapper.Engine.TerminationGracePeriod, tt.want)
			}
		})
	}
}

func int64Pointer(v int64) *int64 { return &v }

// Compile-time guard: make sure we never silently drop the container name
// the drain check targets. If this constant ever renames we want the test
// suite to yell.
var _ = corev1.Container{Name: ContainerNameEngine}
