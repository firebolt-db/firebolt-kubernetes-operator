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
// node setup. Namespaced references — serviceAccountName, Secret refs,
// IAM annotations, ConfigMap refs, persistentVolumeClaim volumes,
// ephemeral claim data sources, and resourceClaims —
// are rejected by the validating webhook and by the engine resolver's
// live-spec check: a cluster-scoped object has no namespace, so such a
// name would bind to whatever happens to exist in each consumer's.
// Storage, rollout, drain-check, autoStop, uiSidecar, and
// customEngineConfig stay on the namespaced FireboltEngineClass /
// FireboltEnginePreset / engine spec.
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

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=cfirengc
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterFireboltEngineClass is a cluster-scoped SKU catalog entry.
// Engines select it by name through spec.engineClassRef. The operator
// resolves that name to a namespaced FireboltEngineClass if one exists
// in the engine's namespace, otherwise to this cluster object.
//
// The catalog is SKU-only: instance type, resources, and node setup.
// Namespaced references belong on the namespaced FireboltEngineClass or
// FireboltEnginePreset, not here: ServiceAccount name, Secret refs, and
// IAM annotations for identity, ConfigMap refs, claim volumes, and
// resourceClaims for data, storage, and devices. A namespaced
// FireboltEngineClass of the same name is an explicit override.
//
// The operator does not reconcile this object: it is authored by
// cluster admins or a cell manager, and consumed read-only by engine
// reconcilers. Deleting it does not tear down running pods; engines
// that still name it emit EngineClassNotFound and keep the last
// applied StatefulSet.
type ClusterFireboltEngineClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ClusterFireboltEngineClassSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterFireboltEngineClassList contains a list of ClusterFireboltEngineClass.
type ClusterFireboltEngineClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterFireboltEngineClass `json:"items"`
}
