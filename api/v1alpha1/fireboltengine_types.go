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
	"k8s.io/apimachinery/pkg/api/resource"
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

// ImageSpec defines the container image configuration.
type ImageSpec struct {
	// Repository is the container image repository.
	// +kubebuilder:validation:MinLength=1
	Repository string `json:"repository"`

	// Tag is the container image tag.
	// +kubebuilder:validation:MinLength=1
	Tag string `json:"tag"`

	// PullPolicy defines when to pull the image.
	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	// +kubebuilder:default=IfNotPresent
	// +optional
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`
}

// ResourceRequirements defines the CPU and memory resources for the engine pods.
type ResourceRequirements struct {
	// CPU request and limit (Kubernetes quantity, e.g. "2", "500m").
	CPU resource.Quantity `json:"cpu"`

	// Memory request and limit (Kubernetes quantity, e.g. "8Gi", "4096Mi").
	Memory resource.Quantity `json:"memory"`
}

// MetadataServiceSpec configures the per-engine metadata service.
type MetadataServiceSpec struct {
	// Image overrides the metadata service container image.
	// If not specified, derived from the engine image
	// (same registry prefix, "dedicated-pensieve" repo, same tag).
	// +optional
	Image *ImageSpec `json:"image,omitempty"`
}

// FireboltEngineSpec defines the desired state of a Firebolt engine.
type FireboltEngineSpec struct {
	// Replicas is the number of engine nodes.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Replicas int32 `json:"replicas"`

	// Image defines the container image to use.
	Image ImageSpec `json:"image"`

	// Resources defines the CPU and memory for engine pods.
	Resources ResourceRequirements `json:"resources"`

	// DrainCheckInterval controls how often to check if old pods have finished serving queries.
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

	// MetadataService configures the per-engine metadata service (PostgreSQL + metadata server).
	// +optional
	MetadataService *MetadataServiceSpec `json:"metadataService,omitempty"`
}

// FireboltEngineStatus defines the observed state of a Firebolt engine.
type FireboltEngineStatus struct {
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

	// PendingMutation holds a queued spec change to apply after the current transition completes.
	// +optional
	PendingMutation *FireboltEngineSpec `json:"pendingMutation,omitempty"`

	// LastAppliedConfig is the spec that was used to create the current/active generation.
	// +optional
	LastAppliedConfig *FireboltEngineSpec `json:"lastAppliedConfig,omitempty"`

	// AccountID is the metadata service account identifier, resolved during
	// the first reconciliation and reused on subsequent ones.
	// +optional
	AccountID string `json:"accountId,omitempty"`
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

	Spec   FireboltEngineSpec   `json:"spec"`
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
