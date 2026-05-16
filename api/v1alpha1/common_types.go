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
)

// ImageSpec defines the container image configuration.
//
// Both Repository and Tag are optional. Any field left empty falls back to
// the corresponding operator default (see the Default*Repository /
// Default*Tag constants in internal/controller), so users may override only
// the repository (to pull from a mirror) or only the tag (to pin a version)
// without restating the other half. An empty struct is equivalent to
// omitting the field entirely: defaults are applied to every dimension.
type ImageSpec struct {
	// Repository is the container image repository. When empty, the
	// operator's default repository for this component is used; pair with
	// Tag to override the full reference, or set on its own to pull the
	// operator-default tag from a different repository (e.g. a mirror).
	// +kubebuilder:validation:MinLength=1
	// +optional
	Repository string `json:"repository,omitempty"`

	// Tag is the container image tag. When empty, the operator's default
	// tag for this component is used; set on its own to pin a specific
	// version while keeping the operator-default repository.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Tag string `json:"tag,omitempty"`

	// PullPolicy defines when to pull the image.
	// +kubebuilder:validation:Enum=Always;Never;IfNotPresent
	// +kubebuilder:default=IfNotPresent
	// +optional
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`
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
