// Package metrics defines Prometheus metrics for the Firebolt operator.
//
// All metrics are level-triggered gauges (or monotonic counters) that
// reflect the current state of FireboltEngine and FireboltInstance CRs.
// They are registered with the controller-runtime metrics registry in
// init() and appear on the operator's /metrics endpoint automatically.
//
// Controllers interact with metrics through the EngineRecorder and
// InstanceRecorder interfaces, which decouple controller logic from
// Prometheus types and allow no-op implementations in tests.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Engine metric label keys.
var engineLabels = []string{"namespace", "name", "instance"}

// Instance metric label keys.
var instanceLabels = []string{"namespace", "name"}

// --- FireboltEngine metrics ---

// EnginePhase reports the current lifecycle phase of each FireboltEngine.
// Exactly one phase label has value 1 per engine; all others are 0.
var EnginePhase = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "firebolt_engine_status_phase",
	Help: "Current lifecycle phase of the FireboltEngine (1=active, 0=inactive).",
}, append(engineLabels, "phase"))

// EngineCondition reports whether each status condition is True (1) or not (0).
var EngineCondition = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "firebolt_engine_status_condition",
	Help: "Status condition of the FireboltEngine (1=True, 0=False/Unknown).",
}, append(engineLabels, "type"))

// EngineSpecReplicas reports the desired replica count from spec.replicas.
var EngineSpecReplicas = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "firebolt_engine_spec_replicas",
	Help: "Desired replica count from the FireboltEngine spec.",
}, engineLabels)

// EngineActiveGeneration reports the generation currently serving traffic.
var EngineActiveGeneration = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "firebolt_engine_active_generation",
	Help: "Generation number currently serving traffic.",
}, engineLabels)

// EnginePodsReady reports the number of ready pods in the active generation.
var EnginePodsReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "firebolt_engine_pods_ready",
	Help: "Number of ready pods in the active generation.",
}, engineLabels)

// EnginePodsTotal reports the total number of pods in the active generation.
var EnginePodsTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "firebolt_engine_pods_total",
	Help: "Total number of pods in the active generation (includes non-ready).",
}, engineLabels)

// EngineDrainingGeneration reports which generation is being drained.
// Set to -1 when no drain is in progress.
var EngineDrainingGeneration = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "firebolt_engine_draining_generation",
	Help: "Generation being drained (-1 if not draining).",
}, engineLabels)

// EngineLastReconciled reports the unix timestamp of the last successful reconcile.
var EngineLastReconciled = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "firebolt_engine_last_reconciled_timestamp",
	Help: "Unix timestamp of the last successful reconcile.",
}, engineLabels)

// EngineDrainCheckErrors counts cumulative drain probe failures.
// This is a monotonic counter that survives operator restarts (resets to 0).
var EngineDrainCheckErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "firebolt_engine_drain_check_errors_total",
	Help: "Cumulative drain probe failures (pod unreachable, metrics missing).",
}, engineLabels)

// --- FireboltInstance metrics ---

// InstancePhase reports the current lifecycle phase of each FireboltInstance.
var InstancePhase = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "firebolt_instance_status_phase",
	Help: "Current lifecycle phase of the FireboltInstance (1=active, 0=inactive).",
}, append(instanceLabels, "phase"))

// InstanceCondition reports whether each status condition is True (1) or not (0).
var InstanceCondition = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "firebolt_instance_status_condition",
	Help: "Status condition of the FireboltInstance (1=True, 0=False/Unknown).",
}, append(instanceLabels, "type"))

// InstanceInfo is an info-style gauge (always 1) carrying static metadata.
var InstanceInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "firebolt_instance_info",
	Help: "Static metadata about the FireboltInstance (always 1).",
}, append(instanceLabels, "id", "postgres_mode"))

// InstanceLastReconciled reports the unix timestamp of the last successful reconcile.
var InstanceLastReconciled = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "firebolt_instance_last_reconciled_timestamp",
	Help: "Unix timestamp of the last successful reconcile.",
}, instanceLabels)

func init() {
	ctrlmetrics.Registry.MustRegister(
		EnginePhase,
		EngineCondition,
		EngineSpecReplicas,
		EngineActiveGeneration,
		EnginePodsReady,
		EnginePodsTotal,
		EngineDrainingGeneration,
		EngineLastReconciled,
		EngineDrainCheckErrors,
		InstancePhase,
		InstanceCondition,
		InstanceInfo,
		InstanceLastReconciled,
	)
}
