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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RolloutStrategy defines how transitions between generations are handled.
// +kubebuilder:validation:Enum=graceful;recreate
type RolloutStrategy string

// RolloutGraceful and RolloutRecreate define the supported rollout strategies.
const (
	RolloutGraceful RolloutStrategy = "graceful"
	RolloutRecreate RolloutStrategy = "recreate"
)

// EnginePhase represents the current phase of the engine transition.
type EnginePhase string

// PhaseStable through PhaseCleaning enumerate the lifecycle phases
// of a FireboltEngine during a blue-green rollout.
const (
	PhaseStable    EnginePhase = "stable"
	PhaseCreating  EnginePhase = "creating"
	PhaseSwitching EnginePhase = "switching"
	PhaseDraining  EnginePhase = "draining"
	PhaseCleaning  EnginePhase = "cleaning"
)

// Condition types for FireboltEngine.
const (
	// ConditionReady is the top-level roll-up condition: True only when
	// the engine is serving traffic on its active generation with all
	// replicas ready, the backing FireboltInstance is healthy, and no
	// transition is in progress. GitOps / ArgoCD-style tooling should
	// key off this condition rather than Phase to decide whether a
	// deployment has converged.
	//
	// Reasons for False:
	//   - Initializing     : status has not been populated yet
	//   - InstanceNotReady : ConditionInstanceReady is False
	//   - Rolling          : phase is Creating / Switching / Draining / Cleaning
	//   - PhaseFailed      : phase is Failed (terminal; human-gated recovery)
	//   - PodsNotReady     : phase is Stable but active-generation pods
	//                        have not yet reported Ready
	ConditionReady = "Ready"

	// ConditionInstanceReady indicates whether the referenced FireboltInstance
	// has a populated metadata endpoint and account ID.
	ConditionInstanceReady = "InstanceReady"
)

// FireboltEngineSpec defines the desired state of a Firebolt engine.
//
// The CEL rule freezes spec.metadataEndpointOverride once it has been set
// (or unset) at creation time. The field selects the cross-cluster metadata
// topology the engine's nodes bake into their on-disk configuration; letting
// users mutate it later would silently force a full blue-green rollout (it
// participates in stsMatchesSpec via AnnotationMetadataOverride) and would
// break the instanceInfo-drift invariant computeStable relies on to allow
// in-place recovery of a deleted ConfigMap (rebuild-at-same-gen produces
// byte-identical content only when all ConfigMap inputs are frozen).
// The wording mirrors the set-once pattern on FireboltInstanceSpec.ID,
// adapted for an optional pointer field via has(): absent-on-create may
// still transition to set, but any set value may not subsequently be
// changed or cleared.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.metadataEndpointOverride) || (has(self.metadataEndpointOverride) && self.metadataEndpointOverride == oldSelf.metadataEndpointOverride)",message="spec.metadataEndpointOverride is immutable once set"
type FireboltEngineSpec struct {
	// InstanceRef is the name of the FireboltInstance in the same namespace
	// that this engine depends on. The engine reconciler will not proceed
	// until the referenced instance has a populated metadata endpoint and
	// account ID. This field is immutable after creation.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="instanceRef is immutable"
	InstanceRef string `json:"instanceRef"`

	// Replicas is the number of engine nodes.
	// +kubebuilder:validation:Minimum=1
	Replicas int32 `json:"replicas"`

	// Image defines the container image to use.
	// If not specified, defaults to the engine image embedded in the operator binary.
	// +optional
	Image *ImageSpec `json:"image,omitempty"`

	// Resources defines the CPU and memory for engine pods.
	Resources ResourceRequirements `json:"resources"`

	// DrainCheckEnabled controls whether the operator performs a SQL-based drain
	// check on old-generation pods during graceful rollouts. When false, the
	// operator skips directly to cleaning after switching traffic, without
	// verifying that in-flight queries have completed. Requires a running
	// node that can execute the drain-check query when enabled.
	// +kubebuilder:default=true
	// +optional
	DrainCheckEnabled *bool `json:"drainCheckEnabled,omitempty"`

	// DrainCheckInterval controls how often to check if old pods have finished serving queries.
	// Only used when drainCheckEnabled is true.
	// +optional
	DrainCheckInterval *metav1.Duration `json:"drainCheckInterval,omitempty"`

	// NodeSelector constrains which nodes the engine pods can be scheduled on.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations allow engine pods to be scheduled on tainted nodes.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Rollout strategy for transitions: "graceful" waits for drain, "recreate" deletes immediately.
	// +kubebuilder:default=graceful
	// +optional
	Rollout RolloutStrategy `json:"rollout,omitempty"`

	// MetadataEndpointOverride overrides the metadata endpoint for this engine.
	// If nil, the engine uses Instance.status.metadataEndpoint (with intra-cluster
	// topology-aware routing). Set this for cross-cluster scenarios where the engine
	// connects to a metadata service in a different cluster via private link.
	//
	// Immutable once set: see the struct-level CEL rule on FireboltEngineSpec.
	// +optional
	MetadataEndpointOverride *string `json:"metadataEndpointOverride,omitempty"`

	// TerminationGracePeriodSeconds is the grace period given to engine pods
	// between SIGTERM and SIGKILL during termination.
	//
	// The operator installs a preStop hook on the engine container that
	// polls Prometheus metrics on 127.0.0.1:9090 (firebolt_running_queries +
	// firebolt_suspended_queries) and blocks termination until the engine
	// reports zero in-flight queries. The hook caps its own wait below this
	// grace period, so an engine with long-running queries will be drained
	// cleanly up to the configured ceiling and then SIGKILLed if queries
	// still have not finished.
	//
	// Defaults to 60 seconds. Raise it for workloads with analytical
	// queries that routinely exceed a minute; lower it for latency-bounded
	// workloads where quicker rollouts are preferable to query survival at
	// the tail.
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=1
	// +optional
	TerminationGracePeriodSeconds *int64 `json:"terminationGracePeriodSeconds,omitempty"`
}

// FireboltEngineStatus defines the observed state of a Firebolt engine.
type FireboltEngineStatus struct {
	// ObservedGeneration is the metadata.generation that was last fully reconciled to stable.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// CurrentGeneration is the latest generation number created.
	CurrentGeneration int `json:"currentGeneration"`

	// ActiveGeneration is the generation currently serving traffic.
	ActiveGeneration int `json:"activeGeneration"`

	// DrainingGeneration is the generation being drained, if any.
	// +optional
	DrainingGeneration *int `json:"drainingGeneration,omitempty"`

	// Phase is the current lifecycle phase of the engine.
	// +optional
	Phase EnginePhase `json:"phase,omitempty"`

	// LastReconciled is the timestamp of the last reconciliation.
	// +optional
	LastReconciled *metav1.Time `json:"lastReconciled,omitempty"`

	// Conditions represent the latest available observations of the engine's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=fireng
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Generation",type=integer,JSONPath=`.status.activeGeneration`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FireboltEngine is the Schema for the fireboltengines API.
type FireboltEngine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FireboltEngineSpec   `json:"spec,omitempty"`
	Status FireboltEngineStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FireboltEngineList contains a list of FireboltEngine.
type FireboltEngineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FireboltEngine `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FireboltEngine{}, &FireboltEngineList{})
}
