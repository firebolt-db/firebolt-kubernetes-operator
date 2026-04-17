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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	computev1alpha1 "github.com/firebolt-analytics/firebolt-kubernetes-operator/api/v1alpha1"
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

func TestBuildEnginePreStopScript(t *testing.T) {
	script := BuildEnginePreStopScript(60)

	// Script must be valid bash - no obvious syntax issues.
	// We can't fully execute it without an actual engine, but we can assert
	// the critical pieces are there so a refactor cannot silently break the
	// contract (scrape metrics -> check both gauges -> exit 0 on drained).
	musts := []string{
		"/dev/tcp/127.0.0.1/9090",
		"GET /metrics",
		MetricRunningQueries,
		MetricSuspendedQueries,
		// Deadline is computed from date +%s so termination is bounded.
		"date +%s",
		"exit 0",
	}
	for _, m := range musts {
		if !strings.Contains(script, m) {
			t.Errorf("preStop script missing %q; script=\n%s", m, script)
		}
	}

	// A tiny TGPS must still produce a script with a deadline of at least
	// one second (otherwise the loop never runs and we effectively skip
	// drain entirely).
	tiny := BuildEnginePreStopScript(1)
	if !strings.Contains(tiny, "DEADLINE=$(($(date +%s) + 1))") {
		t.Errorf("expected 1s deadline in tiny preStop script, got:\n%s", tiny)
	}
}

func TestBuildStatefulSetInstallsPreStopAndTGPS(t *testing.T) {
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
	if c.Lifecycle == nil || c.Lifecycle.PreStop == nil || c.Lifecycle.PreStop.Exec == nil {
		t.Fatal("expected preStop exec handler on engine container")
	}
	cmd := c.Lifecycle.PreStop.Exec.Command
	if len(cmd) != 3 || cmd[0] != "/bin/bash" || cmd[1] != "-c" {
		t.Fatalf("unexpected preStop command: %v", cmd)
	}
	if !strings.Contains(cmd[2], MetricRunningQueries) {
		t.Errorf("preStop script does not scrape %s", MetricRunningQueries)
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

func int64Pointer(v int64) *int64 { return &v }

// Compile-time guard: make sure we never silently drop the container name
// the drain check targets. If this constant ever renames we want the test
// suite to yell.
var _ = corev1.Container{Name: ContainerNameEngine}
