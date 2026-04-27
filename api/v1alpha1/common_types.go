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
	"k8s.io/apimachinery/pkg/api/resource"
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

// ResourceRequirements defines the CPU and memory resources for engine pods.
type ResourceRequirements struct {
	// CPU request and limit (Kubernetes quantity, e.g. "2", "500m").
	CPU resource.Quantity `json:"cpu"`

	// Memory request and limit (Kubernetes quantity, e.g. "8Gi", "4096Mi").
	Memory resource.Quantity `json:"memory"`
}

// ComponentSpec defines deployment configuration shared by operator-managed
// sub-components (gateway, metadata).
type ComponentSpec struct {
	// Replicas is the number of pods for this component.
	// When nil, the controller applies a component-specific default
	// (1 for metadata, 2 for gateway).
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Image overrides the container image for this component.
	// +optional
	Image *ImageSpec `json:"image,omitempty"`

	// Resources overrides CPU and memory for this component's pods.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// NodeSelector constrains which nodes this component's pods can be scheduled on.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations allow this component's pods to be scheduled on tainted nodes.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Affinity defines scheduling affinity rules for this component's pods.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// Labels are additional labels applied to this component's pods.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations are additional annotations applied to this component's pods.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// ServiceAccountName is the ServiceAccount for this component's pods.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// MetricsPort is the container port that exposes Prometheus metrics.
	// The operator always adds this port to the pod spec so that a
	// PodMonitor can reference it by the well-known name "metrics".
	// Override only when the component binary listens on a non-default port.
	// +kubebuilder:default=9090
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	MetricsPort int32 `json:"metricsPort,omitempty"`
}
