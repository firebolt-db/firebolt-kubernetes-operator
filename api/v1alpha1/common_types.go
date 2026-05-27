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
