package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// InstancePhases enumerates all possible instance phases for the StateSet metric.
var InstancePhases = []string{
	string(computev1alpha1.InstancePhaseProvisioning),
	string(computev1alpha1.InstancePhaseReady),
	string(computev1alpha1.InstancePhaseDegraded),
	string(computev1alpha1.InstancePhaseFailed),
}

// InstanceConditionTypes enumerates the condition types tracked for instances.
var InstanceConditionTypes = []string{
	computev1alpha1.InstanceConditionReady,
	computev1alpha1.InstanceConditionPostgresReady,
	computev1alpha1.InstanceConditionMetadataReady,
	computev1alpha1.InstanceConditionGatewayReady,
}

// InstanceRecorder records Prometheus metrics for FireboltInstance resources.
// Use NoOpInstanceRecorder in tests to avoid Prometheus dependencies.
type InstanceRecorder interface {
	// Record updates all instance gauges to reflect the current CR state.
	Record(instance *computev1alpha1.FireboltInstance)

	// Delete removes all metric label sets for the given instance,
	// preventing stale metrics after CR deletion.
	Delete(namespace, name string)
}

type instanceRecorder struct{}

// NewInstanceRecorder returns a concrete InstanceRecorder that writes to Prometheus.
func NewInstanceRecorder() InstanceRecorder {
	return &instanceRecorder{}
}

func (r *instanceRecorder) Record(instance *computev1alpha1.FireboltInstance) {
	ns := instance.Namespace
	name := instance.Name

	for _, phase := range InstancePhases {
		val := float64(0)
		if string(instance.Status.Phase) == phase {
			val = 1
		}
		InstancePhase.WithLabelValues(ns, name, phase).Set(val)
	}

	for _, condType := range InstanceConditionTypes {
		val := float64(0)
		for _, c := range instance.Status.Conditions {
			if c.Type == condType && c.Status == metav1.ConditionTrue {
				val = 1
				break
			}
		}
		InstanceCondition.WithLabelValues(ns, name, condType).Set(val)
	}

	pgMode := "internal"
	if instance.Spec.Metadata.Postgres != nil {
		pgMode = "external"
	}
	InstanceInfo.DeletePartialMatch(prometheus.Labels{"namespace": ns, "name": name})
	InstanceInfo.WithLabelValues(ns, name, instance.Spec.ID, pgMode).Set(1)

	InstanceLastReconciled.WithLabelValues(ns, name).Set(float64(time.Now().Unix()))
}

func (r *instanceRecorder) Delete(namespace, name string) {
	match := prometheus.Labels{"namespace": namespace, "name": name}
	InstancePhase.DeletePartialMatch(match)
	InstanceCondition.DeletePartialMatch(match)
	InstanceInfo.DeletePartialMatch(match)
	InstanceLastReconciled.DeletePartialMatch(match)
}

// NoOpInstanceRecorder is a no-op implementation for use in tests
// and when metrics are disabled.
type NoOpInstanceRecorder struct{}

// Record is a no-op.
func (NoOpInstanceRecorder) Record(*computev1alpha1.FireboltInstance) {}

// Delete is a no-op.
func (NoOpInstanceRecorder) Delete(string, string) {}
