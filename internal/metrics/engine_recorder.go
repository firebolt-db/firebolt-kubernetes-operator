package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// EnginePhases enumerates all possible engine phases for the StateSet metric.
var EnginePhases = []string{
	string(computev1alpha1.PhaseStable),
	string(computev1alpha1.PhaseCreating),
	string(computev1alpha1.PhaseSwitching),
	string(computev1alpha1.PhaseDraining),
	string(computev1alpha1.PhaseCleaning),
	string(computev1alpha1.PhaseStopped),
}

// EngineConditionTypes enumerates the condition types tracked for engines.
var EngineConditionTypes = []string{
	computev1alpha1.ConditionReady,
	computev1alpha1.ConditionInstanceReady,
}

// EngineRecorder records Prometheus metrics for FireboltEngine resources.
// Use NoOpEngineRecorder in tests to avoid Prometheus dependencies.
type EngineRecorder interface {
	// Record updates all engine gauges to reflect the current CR state.
	// podReady and podTotal come from the EngineState observed during reconcile.
	Record(engine *computev1alpha1.FireboltEngine, podReady, podTotal int)

	// RecordDrainCheckError increments the drain check error counter.
	RecordDrainCheckError(namespace, name, instance string)

	// Delete removes all metric label sets for the given engine,
	// preventing stale metrics after CR deletion.
	Delete(namespace, name string)
}

type engineRecorder struct{}

// NewEngineRecorder returns a concrete EngineRecorder that writes to Prometheus.
func NewEngineRecorder() EngineRecorder {
	return &engineRecorder{}
}

func (r *engineRecorder) Record(engine *computev1alpha1.FireboltEngine, podReady, podTotal int) {
	ns := engine.Namespace
	name := engine.Name
	inst := engine.Spec.InstanceRef

	for _, phase := range EnginePhases {
		val := float64(0)
		if string(engine.Status.Phase) == phase {
			val = 1
		}
		EnginePhase.WithLabelValues(ns, name, inst, phase).Set(val)
	}

	for _, condType := range EngineConditionTypes {
		val := float64(0)
		for _, c := range engine.Status.Conditions {
			if c.Type == condType && c.Status == metav1.ConditionTrue {
				val = 1
				break
			}
		}
		EngineCondition.WithLabelValues(ns, name, inst, condType).Set(val)
	}

	EngineSpecReplicas.WithLabelValues(ns, name, inst).Set(float64(engine.Spec.Replicas))
	EngineActiveGeneration.WithLabelValues(ns, name, inst).Set(float64(engine.Status.ActiveGeneration))
	EnginePodsReady.WithLabelValues(ns, name, inst).Set(float64(podReady))
	EnginePodsTotal.WithLabelValues(ns, name, inst).Set(float64(podTotal))

	drainingGen := float64(-1)
	if engine.Status.DrainingGeneration != nil {
		drainingGen = float64(*engine.Status.DrainingGeneration)
	}
	EngineDrainingGeneration.WithLabelValues(ns, name, inst).Set(drainingGen)

	if engine.Status.LastReconciled != nil {
		EngineLastReconciled.WithLabelValues(ns, name, inst).Set(float64(engine.Status.LastReconciled.Unix()))
	} else {
		EngineLastReconciled.WithLabelValues(ns, name, inst).Set(float64(time.Now().Unix()))
	}
}

func (r *engineRecorder) RecordDrainCheckError(namespace, name, instance string) {
	EngineDrainCheckErrors.WithLabelValues(namespace, name, instance).Inc()
}

func (r *engineRecorder) Delete(namespace, name string) {
	match := prometheus.Labels{"namespace": namespace, "name": name}
	EnginePhase.DeletePartialMatch(match)
	EngineCondition.DeletePartialMatch(match)
	EngineSpecReplicas.DeletePartialMatch(match)
	EngineActiveGeneration.DeletePartialMatch(match)
	EnginePodsReady.DeletePartialMatch(match)
	EnginePodsTotal.DeletePartialMatch(match)
	EngineDrainingGeneration.DeletePartialMatch(match)
	EngineLastReconciled.DeletePartialMatch(match)
	EngineDrainCheckErrors.DeletePartialMatch(match)
}

// NoOpEngineRecorder is a no-op implementation for use in tests
// and when metrics are disabled.
type NoOpEngineRecorder struct{}

// Record is a no-op.
func (NoOpEngineRecorder) Record(*computev1alpha1.FireboltEngine, int, int) {}

// RecordDrainCheckError is a no-op.
func (NoOpEngineRecorder) RecordDrainCheckError(string, string, string) {}

// Delete is a no-op.
func (NoOpEngineRecorder) Delete(string, string) {}
