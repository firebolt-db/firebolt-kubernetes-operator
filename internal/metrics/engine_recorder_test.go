package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

func gaugeValue(g prometheus.Gauge) float64 {
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		return -999
	}
	return m.GetGauge().GetValue()
}

func counterValue(c prometheus.Counter) float64 {
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		return -999
	}
	return m.GetCounter().GetValue()
}

func TestEngineRecorderRecord(t *testing.T) {
	rec := NewEngineRecorder()
	drainingGen := 1
	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: "eng-1", Namespace: "ns"},
		Spec: computev1alpha1.FireboltEngineSpec{
			InstanceRef: "inst-1",
			Replicas:    3,
		},
		Status: computev1alpha1.FireboltEngineStatus{
			Phase:              computev1alpha1.PhaseDraining,
			ActiveGeneration:   2,
			DrainingGeneration: &drainingGen,
			Conditions: []metav1.Condition{
				{Type: computev1alpha1.ConditionReady, Status: metav1.ConditionFalse},
				{Type: computev1alpha1.ConditionInstanceReady, Status: metav1.ConditionTrue},
			},
		},
	}

	rec.Record(engine, 2, 3)

	// Phase StateSet: exactly one 1
	if v := gaugeValue(EnginePhase.WithLabelValues("ns", "eng-1", "inst-1", "draining")); v != 1 {
		t.Errorf("draining phase = %v, want 1", v)
	}
	if v := gaugeValue(EnginePhase.WithLabelValues("ns", "eng-1", "inst-1", "stable")); v != 0 {
		t.Errorf("stable phase = %v, want 0", v)
	}

	// Conditions
	if v := gaugeValue(EngineCondition.WithLabelValues("ns", "eng-1", "inst-1", "Ready")); v != 0 {
		t.Errorf("Ready condition = %v, want 0", v)
	}
	if v := gaugeValue(EngineCondition.WithLabelValues("ns", "eng-1", "inst-1", "InstanceReady")); v != 1 {
		t.Errorf("InstanceReady condition = %v, want 1", v)
	}

	// Scalar gauges
	if v := gaugeValue(EngineSpecReplicas.WithLabelValues("ns", "eng-1", "inst-1")); v != 3 {
		t.Errorf("spec_replicas = %v, want 3", v)
	}
	if v := gaugeValue(EngineActiveGeneration.WithLabelValues("ns", "eng-1", "inst-1")); v != 2 {
		t.Errorf("active_generation = %v, want 2", v)
	}
	if v := gaugeValue(EnginePodsReady.WithLabelValues("ns", "eng-1", "inst-1")); v != 2 {
		t.Errorf("pods_ready = %v, want 2", v)
	}
	if v := gaugeValue(EnginePodsTotal.WithLabelValues("ns", "eng-1", "inst-1")); v != 3 {
		t.Errorf("pods_total = %v, want 3", v)
	}
	if v := gaugeValue(EngineDrainingGeneration.WithLabelValues("ns", "eng-1", "inst-1")); v != 1 {
		t.Errorf("draining_generation = %v, want 1", v)
	}
}

func TestEngineRecorderNoDraining(t *testing.T) {
	rec := NewEngineRecorder()
	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: "eng-2", Namespace: "ns"},
		Spec: computev1alpha1.FireboltEngineSpec{
			InstanceRef: "inst-1",
			Replicas:    1,
		},
		Status: computev1alpha1.FireboltEngineStatus{
			Phase: computev1alpha1.PhaseStable,
		},
	}

	rec.Record(engine, 1, 1)

	if v := gaugeValue(EngineDrainingGeneration.WithLabelValues("ns", "eng-2", "inst-1")); v != -1 {
		t.Errorf("draining_generation = %v, want -1 (no drain)", v)
	}
}

func TestEngineRecorderDelete(t *testing.T) {
	rec := NewEngineRecorder()
	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: "eng-del", Namespace: "ns"},
		Spec: computev1alpha1.FireboltEngineSpec{
			InstanceRef: "inst-1",
			Replicas:    1,
		},
		Status: computev1alpha1.FireboltEngineStatus{
			Phase: computev1alpha1.PhaseStable,
		},
	}

	rec.Record(engine, 1, 1)

	// Metric should exist before delete.
	if v := gaugeValue(EngineSpecReplicas.WithLabelValues("ns", "eng-del", "inst-1")); v != 1 {
		t.Fatalf("pre-delete: spec_replicas = %v, want 1", v)
	}

	rec.Delete("ns", "eng-del")

	// After delete, WithLabelValues creates a fresh zero-value gauge.
	// The original label set should have been removed by DeletePartialMatch.
	// We can verify by checking the metric count decreased, but the simplest
	// check is that the phase metric for this engine no longer has the old values.
	for _, phase := range EnginePhases {
		v := gaugeValue(EnginePhase.WithLabelValues("ns", "eng-del", "inst-1", phase))
		if v != 0 {
			t.Errorf("post-delete: phase %s = %v, want 0", phase, v)
		}
	}
}

func TestEngineRecorderDrainCheckError(t *testing.T) {
	rec := NewEngineRecorder()
	rec.RecordDrainCheckError("ns", "eng-1", "inst-1")
	rec.RecordDrainCheckError("ns", "eng-1", "inst-1")

	if v := counterValue(EngineDrainCheckErrors.WithLabelValues("ns", "eng-1", "inst-1")); v != 2 {
		t.Errorf("drain_check_errors = %v, want 2", v)
	}
}
