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

// ClusterFireboltEngineClassSpec is the SKU-only catalog payload.
//
// It carries a pod-template fragment: instance type / family, engine
// container resources, node affinity / tolerations, and init-container
// node setup. Namespace-resolved identifiers (serviceAccountName,
// Secret refs, IAM annotations) are rejected by the validating webhook
// and by the controller's Ready condition. Storage, rollout, drain-check,
// autoStop, uiSidecar, and customEngineConfig stay on the namespaced
// FireboltEngineClass / FireboltEnginePreset / engine spec.
type ClusterFireboltEngineClassSpec struct {
	// Template is the SKU pod template merged into engines that resolve
	// this catalog object. See the type-level doc for the SKU-only lock
	// and the operator-owned paths the validating webhook rejects.
	//
	// The CRD schema for template.metadata is patched post-controller-gen
	// (scripts/patch-crd-template-metadata.py, invoked by `make manifests`)
	// so labels and annotations survive structural-schema pruning.
	Template corev1.PodTemplateSpec `json:"template"`
}

// ClusterFireboltEngineClassStatus is the observed state of a
// ClusterFireboltEngineClass.
type ClusterFireboltEngineClassStatus struct {
	// ObservedGeneration is the metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// BoundEngines counts FireboltEngines in any namespace that resolved
	// spec.engineClassRef to this cluster object: the engine names this
	// object and no FireboltEngineClass of the same name exists in the
	// engine's namespace. The deletion webhook and the reconciler's
	// deletion-guard finalizer re-list live rather than trusting this
	// cached value.
	// +optional
	BoundEngines int32 `json:"boundEngines,omitempty"`

	// Conditions surface the catalog object's high-level state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ClusterFireboltEngineClassConditionReady is the top-level roll-up
// condition: True when spec.template is admissible and SKU-only.
const ClusterFireboltEngineClassConditionReady = "Ready"

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=cfirengc
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Bound",type=integer,JSONPath=`.status.boundEngines`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterFireboltEngineClass is a cluster-scoped SKU catalog entry.
// Engines select it by name through spec.engineClassRef. The operator
// resolves that name to a namespaced FireboltEngineClass if one exists
// in the engine's namespace, otherwise to this cluster object.
//
// The catalog is SKU-only: instance type, resources, and node setup.
// Namespace-resolved identifiers (ServiceAccount name, Secret refs, IAM
// annotations) belong on FireboltEnginePreset, not here. A namespaced
// FireboltEngineClass of the same name is an explicit override.
type ClusterFireboltEngineClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterFireboltEngineClassSpec   `json:"spec,omitempty"`
	Status ClusterFireboltEngineClassStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterFireboltEngineClassList contains a list of ClusterFireboltEngineClass.
type ClusterFireboltEngineClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterFireboltEngineClass `json:"items"`
}
