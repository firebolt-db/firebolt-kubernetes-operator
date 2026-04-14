/*
Copyright 2025.

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

const (
	RolloutGraceful RolloutStrategy = "graceful"
	RolloutRecreate RolloutStrategy = "recreate"
)

// EnginePhase represents the current phase of the engine transition.
type EnginePhase string

const (
	PhaseStable    EnginePhase = "stable"
	PhaseCreating  EnginePhase = "creating"
	PhaseSwitching EnginePhase = "switching"
	PhaseDraining  EnginePhase = "draining"
	PhaseCleaning  EnginePhase = "cleaning"
)

// FireboltEngineSpec defines the desired state of a Firebolt engine.
type FireboltEngineSpec struct {
	// Replicas is the number of engine nodes.
	// +kubebuilder:validation:Minimum=1
	Replicas int32 `json:"replicas"`

	// Image defines the container image to use.
	Image ImageSpec `json:"image"`

	// Resources defines the CPU and memory for engine pods.
	Resources ResourceRequirements `json:"resources"`

	// DrainCheckEnabled controls whether the operator performs a SQL-based drain
	// check on old-generation pods during graceful rollouts. When false, the
	// operator skips directly to cleaning after switching traffic, without
	// verifying that in-flight queries have completed. Requires a reachable
	// metadata endpoint (Pensieve) when enabled.
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

	// MetadataEndpointOverride overrides the Pensieve endpoint for this engine.
	// If nil, the engine uses Instance.status.metadataEndpoint (with intra-cluster
	// topology-aware routing). Set this for cross-cluster scenarios where the engine
	// connects to a Pensieve in a different cluster via private link.
	// +optional
	MetadataEndpointOverride *string `json:"metadataEndpointOverride,omitempty"`
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
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=fire;fireng
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
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
