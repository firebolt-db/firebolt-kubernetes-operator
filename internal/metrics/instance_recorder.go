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
	computev1alpha1.InstanceConditionMetadataReady,
	computev1alpha1.InstanceConditionGatewayReady,
	computev1alpha1.InstanceConditionAuthReady,
	computev1alpha1.InstanceConditionEngineTLSReady,
	computev1alpha1.InstanceConditionGatewayTLSReady,
}

// SigningKeyPhases enumerates the signing-key phases reported by
// InstanceSigningKeys, so a phase that drops to zero keeps reporting 0 rather
// than disappearing from the series.
var SigningKeyPhases = []string{
	string(computev1alpha1.SigningKeyActive),
	string(computev1alpha1.SigningKeyValidationOnly),
	string(computev1alpha1.SigningKeyRemoving),
}

// RotationSteps enumerates the rotation steps reported by
// InstanceRotationPendingStep, for the same reason.
var RotationSteps = []string{
	string(computev1alpha1.RotationStepAwaitingPromotion),
	string(computev1alpha1.RotationStepAwaitingRetireAnchor),
	string(computev1alpha1.RotationStepAwaitingRemoval),
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

	r.recordSigningKeys(instance)
}

// recordSigningKeys reports the signing-key inventory and how long the current
// rotation step has been parked. Every phase and step label is written on every
// pass, including zeros, so a rotation that finishes leaves the series at 0
// instead of leaving the last non-zero value to look permanent.
func (r *instanceRecorder) recordSigningKeys(instance *computev1alpha1.FireboltInstance) {
	ns, name := instance.Namespace, instance.Name
	auth := instance.Status.Auth

	byPhase := make(map[string]float64, len(SigningKeyPhases))
	generation, pendingSeconds, lagging := 0, float64(0), 0
	var pendingStep string
	if auth != nil {
		generation = auth.SigningKeyGeneration
		lagging = auth.LaggingEngineCount
		pendingStep = string(auth.PendingRotationStep)
		for _, k := range auth.SigningKeys {
			byPhase[string(k.Phase)]++
		}
		if auth.PendingSince != nil && pendingStep != "" {
			pendingSeconds = time.Since(auth.PendingSince.Time).Seconds()
		}
	}

	InstanceSigningKeyGeneration.WithLabelValues(ns, name).Set(float64(generation))
	for _, phase := range SigningKeyPhases {
		InstanceSigningKeys.WithLabelValues(ns, name, phase).Set(byPhase[phase])
	}
	for _, step := range RotationSteps {
		val := float64(0)
		if step == pendingStep {
			val = 1
		}
		InstanceRotationPendingStep.WithLabelValues(ns, name, step).Set(val)
	}
	InstanceRotationPendingSeconds.WithLabelValues(ns, name).Set(pendingSeconds)
	InstanceRotationLaggingEngines.WithLabelValues(ns, name).Set(float64(lagging))
}

func (r *instanceRecorder) Delete(namespace, name string) {
	match := prometheus.Labels{"namespace": namespace, "name": name}
	InstancePhase.DeletePartialMatch(match)
	InstanceCondition.DeletePartialMatch(match)
	InstanceInfo.DeletePartialMatch(match)
	InstanceLastReconciled.DeletePartialMatch(match)
	InstanceSigningKeyGeneration.DeletePartialMatch(match)
	InstanceSigningKeys.DeletePartialMatch(match)
	InstanceRotationPendingStep.DeletePartialMatch(match)
	InstanceRotationPendingSeconds.DeletePartialMatch(match)
	InstanceRotationLaggingEngines.DeletePartialMatch(match)
}

// NoOpInstanceRecorder is a no-op implementation for use in tests
// and when metrics are disabled.
type NoOpInstanceRecorder struct{}

// Record is a no-op.
func (NoOpInstanceRecorder) Record(*computev1alpha1.FireboltInstance) {}

// Delete is a no-op.
func (NoOpInstanceRecorder) Delete(string, string) {}
